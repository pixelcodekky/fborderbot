package obs

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// sentryTags are the log fields copied onto Sentry events as tags, so issues
// can be searched and filtered by them. Every field still goes in as extra data.
var sentryTags = map[string]bool{
	"request_id":      true,
	"source":          true,
	"step":            true,
	"event_type":      true,
	"page_id":         true,
	"order_id":        true,
	"comment_id":      true,
	"stripe_event_id": true,
}

// sentryThrottle caps how often one log message is sent. Logs keep every
// occurrence; Sentry only needs enough to alert, and a DB outage hit by
// health checks every few seconds would otherwise burn through the quota.
const sentryThrottle = time.Minute

// sentryHandler passes every record to next and also reports ERROR records to
// Sentry. Attributes added via With are tracked so events carry them too.
type sentryHandler struct {
	next  slog.Handler
	attrs []slog.Attr
	group string
	last  *sync.Map // message -> time.Time of the last event sent
}

func newSentryHandler(next slog.Handler) *sentryHandler {
	return &sentryHandler{next: next, last: &sync.Map{}}
}

func (h *sentryHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *sentryHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && h.allow(r.Message, r.Time) {
		h.capture(r)
	}
	return h.next.Handle(ctx, r)
}

func (h *sentryHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = append(slices.Clip(h.attrs), prefixed(h.group, as)...)
	return &c
}

func (h *sentryHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = joinKey(h.group, name)
	return &c
}

func (h *sentryHandler) allow(msg string, now time.Time) bool {
	if prev, ok := h.last.Load(msg); ok && now.Sub(prev.(time.Time)) < sentryThrottle {
		return false
	}
	h.last.Store(msg, now)
	return true
}

func (h *sentryHandler) capture(r slog.Record) {
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Message = r.Message
	// Group by message, not by the error text, which varies per order.
	event.Fingerprint = []string{r.Message}
	event.Tags = map[string]string{}
	event.Extra = map[string]any{}

	var errText string
	var add func(key string, v slog.Value)
	add = func(key string, v slog.Value) {
		v = v.Resolve()
		if v.Kind() == slog.KindGroup {
			for _, a := range v.Group() {
				add(joinKey(key, a.Key), a.Value)
			}
			return
		}
		val := v.Any()
		if err, ok := val.(error); ok {
			val = err.Error() // errors marshal to {} otherwise
		}
		event.Extra[key] = val
		if sentryTags[key] {
			event.Tags[key] = v.String()
		}
		if key == "err" {
			errText = v.String()
		}
	}
	for _, a := range h.attrs {
		add(a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(joinKey(h.group, a.Key), a.Value)
		return true
	})

	if errText != "" {
		// Shows up as the issue title: "<message>: <error>".
		event.Exception = []sentry.Exception{{Type: r.Message, Value: errText}}
	}
	sentry.CaptureEvent(event)
}

func prefixed(group string, as []slog.Attr) []slog.Attr {
	if group == "" {
		return as
	}
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = slog.Attr{Key: joinKey(group, a.Key), Value: a.Value}
	}
	return out
}

func joinKey(group, key string) string {
	if group == "" {
		return key
	}
	return group + "." + key
}

// Flush waits briefly for queued Sentry events to be sent. Call it before
// os.Exit, which skips deferred calls. It is a no-op when Sentry is off.
func Flush() {
	sentry.Flush(2 * time.Second)
}
