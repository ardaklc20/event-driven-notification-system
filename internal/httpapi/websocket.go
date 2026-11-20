package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/realtime"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
)

// DefaultWSWriteTimeout bounds one WebSocket write when WebSocketDeps leaves
// it at 0 (K25).
const DefaultWSWriteTimeout = 5 * time.Second

// WebSocketDeps configure GET /ws/notifications/{id} and GET /ws/batches/{id}
// (K24, K25). With a nil Hub the endpoints are not registered.
type WebSocketDeps struct {
	Hub      *realtime.Hub
	Statuses *service.Statuses
	// Conns tracks the open connections for shutdown.
	Conns *WebSockets
	// WriteTimeout bounds each write (WS_WRITE_TIMEOUT).
	WriteTimeout time.Duration

	hooks wsHooks
}

// wsHooks let tests stop a connection at fixed points; all nil in the API.
type wsHooks struct {
	afterUpgrade     func(r *http.Request)
	beforeSnapshot   func(r *http.Request)
	beforeFirstWrite func(ctx context.Context, r *http.Request)
}

// Close statuses the server sends.
const (
	closeListenerDown = websocket.StatusTryAgainLater // 1013: reconnect for a fresh snapshot
	closeTooSlow      = websocket.StatusPolicyViolation
	closeGoingAway    = websocket.StatusGoingAway
)

// A UUID in its text form. Anything else cannot name a notification or a
// batch, so it gets 404 like an unknown ID.
var uuidText = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// StatusMessage is one notification's status on the wire: a notification
// snapshot (type "snapshot"), an update (type "update") or one item of a
// batch snapshot (no type).
type StatusMessage struct {
	Type           string    `json:"type,omitempty"`
	NotificationID string    `json:"notification_id"`
	BatchID        *string   `json:"batch_id"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	Version        int64     `json:"version"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// BatchSnapshot is the first message of a batch subscription.
type BatchSnapshot struct {
	Type          string          `json:"type"`
	BatchID       string          `json:"batch_id"`
	Notifications []StatusMessage `json:"notifications"`
}

func statusMessage(typ string, ev domain.StatusEvent) StatusMessage {
	return StatusMessage{
		Type:           typ,
		NotificationID: ev.NotificationID,
		BatchID:        ev.BatchID,
		Status:         string(ev.Status),
		Attempts:       ev.Attempts,
		Version:        ev.Version,
		UpdatedAt:      ev.UpdatedAt.UTC(),
	}
}

// snapshotFunc reads the current state: the first message and the versions
// that seed the filter.
type snapshotFunc func(ctx context.Context) (any, []domain.StatusEvent, error)

func registerWebSockets(mux *http.ServeMux, deps Deps) {
	ws := deps.WebSocket
	if ws.Hub == nil {
		return
	}
	if ws.WriteTimeout <= 0 {
		ws.WriteTimeout = DefaultWSWriteTimeout
	}
	if ws.Conns == nil {
		ws.Conns = NewWebSockets()
	}
	h := &wsHandler{deps: ws, logger: deps.Logger}

	mux.HandleFunc("GET /ws/notifications/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		h.serve(w, r, realtime.NotificationTopic(id), "notification", func(ctx context.Context) (any, []domain.StatusEvent, error) {
			ev, err := ws.Statuses.Notification(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			return statusMessage("snapshot", ev), []domain.StatusEvent{ev}, nil
		})
	})
	mux.HandleFunc("GET /ws/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		h.serve(w, r, realtime.BatchTopic(id), "batch", func(ctx context.Context) (any, []domain.StatusEvent, error) {
			events, err := ws.Statuses.Batch(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			snap := BatchSnapshot{Type: "snapshot", BatchID: id, Notifications: make([]StatusMessage, len(events))}
			for i, ev := range events {
				snap.Notifications[i] = statusMessage("", ev)
			}
			return snap, events, nil
		})
	})
}

type wsHandler struct {
	deps   WebSocketDeps
	logger *slog.Logger
}

// serve answers before the upgrade with 404 for an unknown ID and 503 while
// the listener is down (K46), then runs the subscription: register, read the
// snapshot, send it, then pass newer events (K24).
func (h *wsHandler) serve(w http.ResponseWriter, r *http.Request, topic realtime.Topic, kind string, snapshot snapshotFunc) {
	id := r.PathValue("id")
	if !uuidText.MatchString(id) {
		writePlainProblem(w, http.StatusNotFound, kind+" not found")
		return
	}
	if _, _, err := snapshot(r.Context()); err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrBatchNotFound) {
			writePlainProblem(w, http.StatusNotFound, kind+" not found")
			return
		}
		loggerFrom(r.Context(), h.logger).LogAttrs(r.Context(), slog.LevelError, "websocket snapshot", slog.String("error", err.Error()))
		writePlainProblem(w, http.StatusInternalServerError, "unexpected error")
		return
	}
	gen, live := h.deps.Hub.Live()
	if !live {
		w.Header().Set("Retry-After", "1")
		writePlainProblem(w, http.StatusServiceUnavailable, "status listener is reconnecting")
		return
	}

	// The server's read and write timeouts stay on a hijacked connection
	// (net/http Hijacker documentation); a WebSocket lives longer than one
	// request, so they are cleared before the upgrade.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has written the error response
	}
	// The connection's own context: cancelling it cuts the connection even
	// during a close handshake, which is how shutdown ends a silent client.
	ctx, cut := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cut()
	if !h.deps.Conns.add(conn, cut) {
		_ = conn.CloseNow()
		return
	}
	defer h.deps.Conns.done(conn)
	defer func() { _ = conn.CloseNow() }()

	// CloseRead answers ping, pong and close frames; its context ends when
	// the peer closes or the connection is cut (K25).
	readCtx := conn.CloseRead(ctx)

	if f := h.deps.hooks.afterUpgrade; f != nil {
		f(r)
	}
	sub, err := h.deps.Hub.Subscribe(topic, gen)
	if err != nil {
		_ = conn.Close(closeListenerDown, "status listener reconnecting")
		return
	}
	defer sub.Close()

	if f := h.deps.hooks.beforeSnapshot; f != nil {
		f(r)
	}
	first, seeds, err := snapshot(ctx)
	if err != nil {
		loggerFrom(r.Context(), h.logger).LogAttrs(r.Context(), slog.LevelError, "websocket snapshot", slog.String("error", err.Error()))
		_ = conn.Close(websocket.StatusInternalError, "snapshot failed")
		return
	}
	filter := realtime.NewFilter()
	for _, ev := range seeds {
		filter.Seed(ev)
	}

	if f := h.deps.hooks.beforeFirstWrite; f != nil {
		f(sub.Context(), r)
	}
	if h.ended(conn, sub) || h.write(conn, sub, first) != nil {
		return
	}
	for {
		select {
		case <-readCtx.Done():
			return
		case <-h.deps.Conns.closing:
			_ = conn.Close(closeGoingAway, "server shutting down")
			return
		case <-sub.Done():
			h.ended(conn, sub)
			return
		case ev := <-sub.Events():
			if !filter.Allow(ev) {
				continue
			}
			if h.ended(conn, sub) || h.write(conn, sub, statusMessage("update", ev)) != nil {
				return
			}
		}
	}
}

// ended closes the connection with the subscription's reason if it has ended.
func (h *wsHandler) ended(conn *websocket.Conn, sub *realtime.Subscription) bool {
	switch err := sub.Err(); {
	case err == nil:
		return false
	case errors.Is(err, realtime.ErrTooSlow):
		_ = conn.Close(closeTooSlow, "client too slow")
	default:
		_ = conn.Close(closeListenerDown, "status listener reconnecting")
	}
	return true
}

// write sends one message within the write timeout. The write is also bound
// to the subscription, so dropping a slow subscriber stops a stuck write.
func (h *wsHandler) write(conn *websocket.Conn, sub *realtime.Subscription, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	ctx, cancel := context.WithTimeout(sub.Context(), h.deps.WriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

func writePlainProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
	_, _ = w.Write(b)
}

// WebSockets tracks the open WebSocket connections. http.Server.Shutdown
// neither closes nor waits for hijacked connections (K25): StartClose, run by
// RegisterOnShutdown, tells every connection to close with status 1001, and
// Wait waits for them with its own limit.
type WebSockets struct {
	closing   chan struct{}
	closeOnce sync.Once

	mu    sync.Mutex
	conns map[*websocket.Conn]context.CancelFunc
	wg    sync.WaitGroup
}

// NewWebSockets returns an empty tracker.
func NewWebSockets() *WebSockets {
	return &WebSockets{closing: make(chan struct{}), conns: make(map[*websocket.Conn]context.CancelFunc)}
}

// StartClose tells every connection's writer to send close status 1001 and
// returns at once: net/http runs it in its own goroutine and says it should
// not wait for shutdown to complete. Connections that arrive later are
// refused.
func (s *WebSockets) StartClose() {
	s.closeOnce.Do(func() { close(s.closing) })
}

// Wait waits until every connection has ended or ctx ends; then it cuts the
// connections still open and waits for their handlers to return.
func (s *WebSockets) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	s.mu.Lock()
	for _, cut := range s.conns {
		cut()
	}
	s.mu.Unlock()
	<-done
}

// Open returns how many connections are open.
func (s *WebSockets) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *WebSockets) add(c *websocket.Conn, cut context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closing:
		return false
	default:
	}
	s.conns[c] = cut
	s.wg.Add(1)
	return true
}

func (s *WebSockets) done(c *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	s.wg.Done()
}
