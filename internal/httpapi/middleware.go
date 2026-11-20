package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

// CorrelationIDHeader carries the request's correlation ID in both directions
// (K21).
const CorrelationIDHeader = "X-Correlation-ID"

// A client's correlation ID is used only if it is at most 64 characters from
// A–Z a–z 0–9 - _ . so a client cannot write arbitrary text into our logs.
var validCorrelationID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ctxKey int

const (
	correlationIDKey ctxKey = iota
	loggerKey
)

// correlationIDFrom returns the request's correlation ID.
func correlationIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

// loggerFrom returns the request's logger, which carries its correlation ID.
// slog's JSON handler ignores the context, so the ID travels in a logger made
// with With, not in the context alone (K21).
func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return fallback
}

// withCorrelationID reads X-Correlation-ID or generates one, returns it in the
// response header and puts it, and a logger carrying it, on the context.
func withCorrelationID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationIDHeader)
		if len(r.Header.Values(CorrelationIDHeader)) != 1 || !validCorrelationID.MatchString(id) {
			id = rand.Text() // 26 characters of base32, inside the allowed set
		}
		w.Header().Set(CorrelationIDHeader, id)
		ctx := context.WithValue(r.Context(), correlationIDKey, id)
		ctx = context.WithValue(ctx, loggerKey, logger.With(slog.String("correlation_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// logRequests writes one JSON line per request. Health checks are logged at
// debug level so a probe every few seconds does not flood the log (K21, K30).
// Paths are logged without the query string; bodies are never logged.
func logRequests(fallback *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		loggerFrom(r.Context(), fallback).LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// recoverPanics turns a panic in a handler into a 500 problem response and an
// error log line with the stack (K16).
func recoverPanics(fallback *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // the server's own way of aborting a response
			}
			loggerFrom(r.Context(), fallback).LogAttrs(r.Context(), slog.LevelError, "panic in handler",
				slog.String("panic", fmt.Sprint(v)),
				slog.String("stack", string(debug.Stack())),
			)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"Internal Server Error","status":500,"detail":"unexpected error"}`))
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code written through it.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
