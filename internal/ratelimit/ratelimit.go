// Package ratelimit protects the public, browser-facing pages (receipts, the
// payment return page) from floods: each client gets a token bucket, and a
// global cap bounds how many of these requests run at once, so a flood can't
// tie up the database the webhooks need. It is in-memory, per instance; a CDN
// rule in front (e.g. Cloudflare) is the defence against large attacks.
package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limiter is a per-client token bucket plus a cap on requests in flight.
type Limiter struct {
	PerMinute   float64 // sustained requests per client per minute
	Burst       int     // requests a client may make at once
	MaxInFlight int     // limited requests running at once, across clients; 0 means no cap
	// ProxyHops is how many proxies in front of the app append to
	// X-Forwarded-For (Render: 1; Cloudflare in front of Render: 2). The
	// client is the address that many entries from the right; addresses
	// further left are the client's own claims and are ignored. 0 uses the
	// connection's address.
	ProxyHops  int
	MaxClients int              // buckets kept; 0 means 100000
	Now        func() time.Time // defaults to time.Now; overridden in tests
	Log        *slog.Logger     // for the per-minute summary; defaults to slog.Default()

	mu        sync.Mutex
	clients   map[string]*bucket
	inFlight  int
	lastSweep time.Time
	// Rejections since the last summary, logged at most once a minute so a
	// flood doesn't flood the logs too.
	rejected     int
	rejectedFrom map[string]struct{}
	lastSummary  time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// Wrap limits next. A rejected request gets 429 with Retry-After.
func (l *Limiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := l.ClientIP(r)
		ok, retry := l.acquire(client)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Too many requests. Please wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		defer l.release()
		next.ServeHTTP(w, r)
	})
}

// acquire takes a token for client and a place in flight. retry is the
// seconds to wait when refused.
func (l *Limiter) acquire(client string) (ok bool, retry int) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.summarize(now)

	if l.MaxInFlight > 0 && l.inFlight >= l.MaxInFlight {
		l.reject(client)
		return false, 1
	}
	b := l.clients[client]
	if b == nil {
		if len(l.clients) >= l.maxClients() {
			l.sweep(now)
		}
		if len(l.clients) >= l.maxClients() {
			// More clients than we track: refuse rather than grow without
			// bound. Only a large distributed flood gets here.
			l.reject(client)
			return false, 60
		}
		if l.clients == nil {
			l.clients = map[string]*bucket{}
		}
		b = &bucket{tokens: float64(l.Burst), at: now}
		l.clients[client] = b
	}
	b.tokens = l.refill(b, now)
	b.at = now
	if b.tokens < 1 {
		l.reject(client)
		return false, int((1-b.tokens)*60/l.PerMinute) + 1
	}
	b.tokens--
	l.inFlight++
	if now.Sub(l.lastSweep) > time.Minute {
		l.sweep(now)
	}
	return true, 0
}

func (l *Limiter) release() {
	l.mu.Lock()
	l.inFlight--
	l.mu.Unlock()
}

func (l *Limiter) refill(b *bucket, now time.Time) float64 {
	t := b.tokens + now.Sub(b.at).Minutes()*l.PerMinute
	if t > float64(l.Burst) {
		t = float64(l.Burst)
	}
	return t
}

// sweep forgets clients whose bucket has refilled: a new bucket is the same.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.clients {
		if l.refill(b, now) >= float64(l.Burst) {
			delete(l.clients, k)
		}
	}
	l.lastSweep = now
}

func (l *Limiter) reject(client string) {
	l.rejected++
	if l.rejectedFrom == nil {
		l.rejectedFrom = map[string]struct{}{}
	}
	if len(l.rejectedFrom) < 1000 {
		l.rejectedFrom[client] = struct{}{}
	}
}

func (l *Limiter) summarize(now time.Time) {
	if l.rejected == 0 || now.Sub(l.lastSummary) < time.Minute {
		return
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("requests rate limited", "source", "ratelimit",
		"rejected", l.rejected, "clients", len(l.rejectedFrom), "in_flight", l.inFlight, "tracked_clients", len(l.clients))
	l.rejected, l.rejectedFrom, l.lastSummary = 0, nil, now
}

// ClientIP is the address the request came from, as described on ProxyHops.
func (l *Limiter) ClientIP(r *http.Request) string {
	if l.ProxyHops > 0 {
		var hops []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, h := range strings.Split(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					hops = append(hops, h)
				}
			}
		}
		if i := len(hops) - l.ProxyHops; i >= 0 && i < len(hops) {
			if ip := net.ParseIP(hops[i]); ip != nil {
				return clientKey(ip)
			}
		}
		// Fewer entries than proxies: the request didn't come through all
		// of them, so the connection's address is all there is.
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return clientKey(ip)
	}
	return host
}

// clientKey groups IPv6 clients by /64, which one subscriber usually gets
// whole; otherwise every address in it would be a fresh bucket.
func clientKey(ip net.IP) string {
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Limiter) maxClients() int {
	if l.MaxClients > 0 {
		return l.MaxClients
	}
	return 100000
}
