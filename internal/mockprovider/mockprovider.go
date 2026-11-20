// Package mockprovider is the local provider Compose starts by default (K14).
// It answers like the case's provider and records when each request arrives,
// per channel. It measures arrivals, not the worker's send starts (K5).
package mockprovider

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/ratecheck"
)

// Channels are the channels /stats reports, in this order.
var Channels = []string{"sms", "email", "push"}

const maxBody = 64 << 10

// Server is the mock provider.
type Server struct {
	delay  time.Duration
	logger *slog.Logger

	mu       sync.Mutex
	arrivals map[string][]time.Time
}

// New returns a mock that answers each valid request after delay.
func New(delay time.Duration, logger *slog.Logger) *Server {
	return &Server{delay: delay, logger: logger, arrivals: map[string][]time.Time{}}
}

// Handler serves POST / and GET /stats.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", s.deliver)
	mux.HandleFunc("GET /stats", s.stats)
	return mux
}

// request is the body the mock accepts: exactly {to, channel, content}.
type request struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

// Acceptance is the 202 body, in the case's shape (R13).
type Acceptance struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

func (s *Server) deliver(w http.ResponseWriter, r *http.Request) {
	arrived := time.Now()

	var req request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() || !valid(req) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {to, channel, content} with a known channel"})
		return
	}

	s.mu.Lock()
	s.arrivals[req.Channel] = append(s.arrivals[req.Channel], arrived)
	s.mu.Unlock()
	s.logger.Debug("request arrived", slog.String("channel", req.Channel),
		slog.String("correlation_id", r.Header.Get("X-Correlation-ID")))

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusAccepted, Acceptance{
		MessageID: newUUID(),
		Status:    "accepted",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func valid(req request) bool {
	return req.To != "" && req.Content != "" && slices.Contains(Channels, req.Channel)
}

// ChannelStats is one channel's line in /stats.
type ChannelStats struct {
	Arrivals int `json:"arrivals"`
	// MaxArrivalsPerSecond is the highest number of arrivals in any sliding
	// one-second window [t, t + 1 s).
	MaxArrivalsPerSecond int `json:"max_arrivals_per_second"`
}

// Stats is the body of GET /stats.
type Stats struct {
	Channels map[string]ChannelStats `json:"channels"`
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

// Snapshot returns the arrival counts so far.
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Channels: make(map[string]ChannelStats, len(Channels))}
	for _, ch := range Channels {
		times := s.arrivals[ch]
		out.Channels[ch] = ChannelStats{Arrivals: len(times), MaxArrivalsPerSecond: ratecheck.MaxInWindow(times, ratecheck.Window)}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
