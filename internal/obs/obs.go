// Package obs holds the app's observability plumbing: a JSON slog logger, a
// per-request logger carried in the context, and the HTTP access-log middleware.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"
)

// Setup installs a JSON logger on stdout as the slog default. level is one of
// debug|info|warn|error; anything else falls back to info.
func Setup(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	l := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(l)
	return l
}

type ctxKey struct{}

// WithLogger returns a copy of ctx that carries l.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// Logger returns the request-scoped logger from ctx (already tagged with
// request_id), or the default logger if there is none.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Middleware tags every request with a request_id, logs one access line per
// request, and turns panics into a logged 500 instead of a dropped connection.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" || len(reqID) > 64 {
			reqID = newID()
		}
		w.Header().Set("X-Request-Id", reqID)

		l := slog.Default().With("request_id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if p := recover(); p != nil {
				l.Error("panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if !sw.wroteHeader {
					sw.WriteHeader(http.StatusInternalServerError)
				}
			}

			lvl := slog.LevelInfo
			switch {
			case sw.status >= 500:
				lvl = slog.LevelError
			case sw.status >= 400:
				lvl = slog.LevelWarn
			case r.URL.Path == "/health":
				lvl = slog.LevelDebug // platform health checks would drown everything else
			}
			l.Log(r.Context(), lvl, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()

		next.ServeHTTP(sw, r.WithContext(WithLogger(r.Context(), l)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
