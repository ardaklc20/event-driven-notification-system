package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// StatusChannel is the channel the status trigger notifies (K24).
const StatusChannel = "notification_status"

const (
	listenerMinPause = 100 * time.Millisecond
	listenerMaxPause = 5 * time.Second
	listenerClose    = time.Second
)

// StatusSink receives the listener's state and its events. realtime.Hub
// implements it; this package does not import it (K23).
type StatusSink interface {
	// Connected starts a new generation once LISTEN has succeeded.
	Connected() uint64
	// Disconnected ends it when the connection drops or the listener stops.
	Disconnected()
	Publish(ev domain.StatusEvent)
}

// Listener keeps one connection out of the pool that runs LISTEN and waits
// for status events (K24). It never opens a transaction, so it cannot hold
// back cleanup of PostgreSQL's notification queue.
type Listener struct {
	pool   *pgxpool.Pool
	sink   StatusSink
	logger *slog.Logger
}

// NewListener returns a listener that passes events to sink.
func NewListener(pool *pgxpool.Pool, sink StatusSink, logger *slog.Logger) *Listener {
	return &Listener{pool: pool, sink: sink, logger: logger}
}

// Run listens until ctx ends. When the connection drops it reconnects after a
// pause that doubles from 100 ms up to 5 s and starts again after a
// successful LISTEN; PostgreSQL does not replay what was sent meanwhile, so
// the sink closes the subscriptions of the ended generation (K46).
func (l *Listener) Run(ctx context.Context) {
	pause := listenerMinPause
	for {
		err := l.listen(ctx, func() { pause = listenerMinPause })
		if ctx.Err() != nil {
			return
		}
		l.logger.LogAttrs(ctx, slog.LevelWarn, "status listener down",
			slog.String("error", err.Error()),
			slog.String("retry_in", pause.String()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, listenerMaxPause)
	}
}

func (l *Listener) listen(ctx context.Context, connected func()) error {
	pc, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	// The connection leaves the pool for good: the pool neither resets it
	// nor hands it to a query while it listens.
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), listenerClose)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+StatusChannel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	gen := l.sink.Connected()
	defer l.sink.Disconnected()
	connected()
	l.logger.LogAttrs(ctx, slog.LevelInfo, "status listener up", slog.Uint64("generation", gen))

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		ev, err := decodeStatusEvent(n.Payload)
		if err != nil {
			l.logger.LogAttrs(ctx, slog.LevelError, "status event dropped", slog.String("error", err.Error()))
			continue
		}
		l.sink.Publish(ev)
	}
}

// statusPayload is the trigger's JSON (00010_status_version).
type statusPayload struct {
	NotificationID string    `json:"notification_id"`
	BatchID        *string   `json:"batch_id"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	Version        int64     `json:"version"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func decodeStatusEvent(payload string) (domain.StatusEvent, error) {
	var p statusPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return domain.StatusEvent{}, fmt.Errorf("decode payload: %w", err)
	}
	if p.NotificationID == "" {
		return domain.StatusEvent{}, errors.New("decode payload: no notification_id")
	}
	return domain.StatusEvent{
		NotificationID: p.NotificationID,
		BatchID:        p.BatchID,
		Status:         domain.Status(p.Status),
		Attempts:       p.Attempts,
		Version:        p.Version,
		UpdatedAt:      p.UpdatedAt.UTC(),
	}, nil
}

const statusColumns = `id::text, batch_id::text, status::text, attempts, version, updated_at`

const notificationStatusSQL = `SELECT ` + statusColumns + ` FROM notifications WHERE id = $1`

// A batch is created with all its items in one transaction (K12), so a batch
// without rows does not exist.
const batchStatusesSQL = `SELECT ` + statusColumns + `
	FROM notifications WHERE batch_id = $1 ORDER BY batch_position`

// NotificationStatus reads one notification's snapshot for a subscription;
// domain.ErrNotFound when it does not exist.
func (s *Store) NotificationStatus(ctx context.Context, id string) (domain.StatusEvent, error) {
	rows, err := s.pool.Query(ctx, notificationStatusSQL, id)
	if err != nil {
		return domain.StatusEvent{}, fmt.Errorf("notification status: %w", err)
	}
	events, err := scanStatusEvents(rows)
	if err != nil {
		return domain.StatusEvent{}, fmt.Errorf("notification status: %w", err)
	}
	if len(events) == 0 {
		return domain.StatusEvent{}, domain.ErrNotFound
	}
	return events[0], nil
}

// BatchStatuses reads every notification of a batch, in batch order, in one
// statement; domain.ErrBatchNotFound when the batch does not exist.
func (s *Store) BatchStatuses(ctx context.Context, id string) ([]domain.StatusEvent, error) {
	rows, err := s.pool.Query(ctx, batchStatusesSQL, id)
	if err != nil {
		return nil, fmt.Errorf("batch statuses: %w", err)
	}
	events, err := scanStatusEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("batch statuses: %w", err)
	}
	if len(events) == 0 {
		return nil, domain.ErrBatchNotFound
	}
	return events, nil
}

func scanStatusEvents(rows pgx.Rows) ([]domain.StatusEvent, error) {
	var (
		events []domain.StatusEvent
		ev     domain.StatusEvent
		status string
	)
	_, err := pgx.ForEachRow(rows, []any{&ev.NotificationID, &ev.BatchID, &status, &ev.Attempts, &ev.Version, &ev.UpdatedAt}, func() error {
		ev.Status = domain.Status(status)
		ev.UpdatedAt = ev.UpdatedAt.UTC()
		events = append(events, ev)
		ev.BatchID = nil
		return nil
	})
	return events, err
}
