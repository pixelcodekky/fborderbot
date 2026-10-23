package ratelimit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newLimiter(c *clock) *Limiter {
	return &Limiter{PerMinute: 6, Burst: 3, Now: c.now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func get(h http.Handler, remote string, xff ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/receipts/x", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

func TestBurstThenRefill(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
	h := newLimiter(c).Wrap(ok)
	for i := 0; i < 3; i++ {
		if w := get(h, "1.2.3.4:5"); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := get(h, "1.2.3.4:5")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "11" {
		t.Fatalf("4th: %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
	// Another client has its own bucket.
	if w := get(h, "5.6.7.8:5"); w.Code != http.StatusOK {
		t.Errorf("other client: %d", w.Code)
	}
	// 6 a minute: one token back every 10 seconds.
	c.advance(10 * time.Second)
	if w := get(h, "1.2.3.4:5"); w.Code != http.StatusOK {
		t.Errorf("after refill: %d", w.Code)
	}
	if w := get(h, "1.2.3.4:5"); w.Code != http.StatusTooManyRequests {
		t.Errorf("refill gave more than one token: %d", w.Code)
	}
}

func TestClientIPTrustsOnlyProxyHops(t *testing.T) {
	l := &Limiter{ProxyHops: 1}
	req := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	cases := []struct {
		r    *http.Request
		hops int
		want string
	}{
		// The proxy appends the real client; what the client sent is ignored.
		{req("10.0.0.1:1", "6.6.6.6, 203.0.113.9"), 1, "203.0.113.9"},
		{req("10.0.0.1:1", "6.6.6.6", "203.0.113.9"), 1, "203.0.113.9"},
		// Cloudflare then Render: two hops.
		{req("10.0.0.1:1", "6.6.6.6, 203.0.113.9, 172.68.1.1"), 2, "203.0.113.9"},
		// No header, or junk: the connection's address.
		{req("198.51.100.7:1"), 1, "198.51.100.7"},
		{req("198.51.100.7:1", "not-an-ip"), 1, "198.51.100.7"},
		// Hops 0: the header is not trusted at all.
		{req("198.51.100.7:1", "203.0.113.9"), 0, "198.51.100.7"},
		// IPv6 clients are grouped by /64.
		{req("10.0.0.1:1", "2001:db8:1:2:aaaa::1"), 1, "2001:db8:1:2::/64"},
	}
	for i, c := range cases {
		l.ProxyHops = c.hops
		if got := l.ClientIP(c.r); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

func TestSpoofedForwardedForDoesNotBypass(t *testing.T) {
	c := &clock{t: time.Now()}
	l := newLimiter(c)
	l.ProxyHops = 1
	h := l.Wrap(ok)
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		// A new made-up address each time, then the proxy's real one.
		codes[get(h, "10.0.0.1:1", "192.0.2."+string(rune('0'+i))+", 203.0.113.9").Code]++
	}
	if codes[http.StatusOK] != 3 || codes[http.StatusTooManyRequests] != 7 {
		t.Errorf("codes %v", codes)
	}
}

func TestMaxInFlight(t *testing.T) {
	c := &clock{t: time.Now()}
	l := newLimiter(c)
	l.MaxInFlight = 2
	release := make(chan struct{})
	var started sync.WaitGroup
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Done()
		<-release
	})
	h := l.Wrap(slow)
	var done sync.WaitGroup
	for i := 0; i < 2; i++ {
		started.Add(1)
		done.Add(1)
		go func(i int) { defer done.Done(); get(h, "198.51.100."+string(rune('1'+i))+":1") }(i)
	}
	started.Wait()
	if w := get(h, "198.51.100.9:1"); w.Code != http.StatusTooManyRequests {
		t.Errorf("third in flight: %d", w.Code)
	}
	close(release)
	done.Wait()
	started.Add(1) // the next request runs slow, which calls Done
	if w := get(h, "198.51.100.9:1"); w.Code != http.StatusOK {
		t.Errorf("after the others finished: %d", w.Code)
	}
}

func TestClientsAreForgottenAndCapped(t *testing.T) {
	c := &clock{t: time.Now()}
	l := newLimiter(c)
	l.MaxClients = 2
	h := l.Wrap(ok)
	get(h, "198.51.100.1:1")
	get(h, "198.51.100.2:1")
	if w := get(h, "198.51.100.3:1"); w.Code != http.StatusTooManyRequests {
		t.Errorf("past the cap: %d", w.Code)
	}
	// Once their buckets refill, the old clients are dropped to make room.
	c.advance(time.Minute)
	if w := get(h, "198.51.100.3:1"); w.Code != http.StatusOK {
		t.Errorf("after refill: %d", w.Code)
	}
	if n := len(l.clients); n != 1 {
		t.Errorf("%d clients tracked, want 1", n)
	}
}
