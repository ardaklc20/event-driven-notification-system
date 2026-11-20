package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/realtime"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// clientReadLimit is the read limit cmd/wswatch sets: 1 MiB, about five
// times the largest snapshot.
const clientReadLimit = 1 << 20

// wsOptions shape one test's API.
type wsOptions struct {
	queue int // WS_SEND_QUEUE; 16 when 0
	hooks httpapi.WSHooks
}

// wsEnv is one test's API with a real PostgreSQL, the status listener, the
// hub and an httptest server. It takes goleak.IgnoreCurrent() before it opens
// anything and checks for leaked goroutines after everything is closed (K45).
type wsEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *postgres.Store
	hub    *realtime.Hub
	conns  *httpapi.WebSockets
	srv    *httptest.Server
	client *http.Client

	listenMu     sync.Mutex
	stopListener func()
}

func newWSEnv(t *testing.T, o wsOptions) *wsEnv {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	queue := o.queue
	if queue == 0 {
		queue = 16
	}
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	e := &wsEnv{
		t:      t,
		pool:   pool,
		store:  store,
		hub:    realtime.NewHub(queue),
		conns:  httpapi.NewWebSockets(),
		client: &http.Client{Transport: &http.Transport{}},
	}
	deps := httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(store, domain.DefaultLimits()),
		WebSocket: httpapi.WebSocketDeps{
			Hub:          e.hub,
			Statuses:     service.NewStatuses(store),
			Conns:        e.conns,
			WriteTimeout: 5 * time.Second,
		},
	}
	httpapi.SetWSHooks(&deps.WebSocket, o.hooks)
	e.srv = httptest.NewUnstartedServer(httpapi.New(deps))
	e.srv.Start()
	e.startListener()
	t.Cleanup(e.close)
	return e
}

// close ends the sockets, the server and the listener, before the pool is
// closed and the database dropped.
func (e *wsEnv) close() {
	e.conns.StartClose()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.conns.Wait(ctx)
	e.srv.Close()
	e.client.CloseIdleConnections()
	e.stopListen()
}

func (e *wsEnv) startListener() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		postgres.NewListener(e.pool, e.hub, slog.New(slog.DiscardHandler)).Run(ctx)
	}()
	e.listenMu.Lock()
	e.stopListener = func() {
		cancel()
		<-done
	}
	e.listenMu.Unlock()
	e.waitLive(0)
}

func (e *wsEnv) stopListen() {
	e.listenMu.Lock()
	stop := e.stopListener
	e.stopListener = nil
	e.listenMu.Unlock()
	if stop != nil {
		stop()
	}
}

// waitLive waits until a generation newer than after is live.
func (e *wsEnv) waitLive(after uint64) uint64 {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if gen, live := e.hub.Live(); live && gen > after {
			return gen
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("no listener generation after %d became live", after)
	return 0
}

// terminateListener ends the listener's backend, as a dropped connection
// would.
func (e *wsEnv) terminateListener() {
	e.t.Helper()
	var ended bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND query = 'LISTEN notification_status'`).Scan(&ended); err != nil || !ended {
		e.t.Fatalf("terminate the listener's backend: ended=%v err=%v", ended, err)
	}
}

func (e *wsEnv) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path
}

// dialResp dials path; when the server refuses the upgrade, the reply holds
// its status and headers.
func (e *wsEnv) dialResp(path string, header http.Header) (*websocket.Conn, reply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, e.wsURL(path), &websocket.DialOptions{HTTPClient: e.client, HTTPHeader: header})
	var r reply
	if resp != nil {
		r = reply{StatusCode: resp.StatusCode, Header: resp.Header}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if c != nil {
		c.SetReadLimit(clientReadLimit)
		e.t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, r, err
}

func (e *wsEnv) dial(path string) *websocket.Conn {
	e.t.Helper()
	c, _, err := e.dialResp(path, nil)
	if err != nil {
		e.t.Fatalf("dial %s: %v", path, err)
	}
	return c
}

// read reads one text message within d.
func read(t *testing.T, c *websocket.Conn, d time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type %v, want text", typ)
	}
	return data
}

func readStatus(t *testing.T, c *websocket.Conn) httpapi.StatusMessage {
	t.Helper()
	var m httpapi.StatusMessage
	if err := json.Unmarshal(read(t, c, 5*time.Second), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func readBatch(t *testing.T, c *websocket.Conn) httpapi.BatchSnapshot {
	t.Helper()
	var m httpapi.BatchSnapshot
	if err := json.Unmarshal(read(t, c, 5*time.Second), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// closeStatus reads until the connection ends and returns its close status,
// or -1 when it ended without a close frame.
func closeStatus(t *testing.T, c *websocket.Conn, d time.Duration) (websocket.StatusCode, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	messages := 0
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err), messages
		}
		messages++
	}
}

func insertNotification(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 VALUES ('+905551234567', 'sms', 'hello', 'normal', 'ws-test') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// insertBatch inserts a batch of n and returns its ID and its items' IDs in
// batch order.
func insertBatch(t *testing.T, pool *pgxpool.Pool, n int) (string, []string) {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`WITH b AS (INSERT INTO batches (item_count, correlation_id) VALUES ($1, 'ws-test') RETURNING id)
		 INSERT INTO notifications (recipient, channel, content, priority, correlation_id, batch_id, batch_position)
		 SELECT '+905551234567', 'sms', 'hello', 'normal', 'ws-test', b.id, g - 1
		 FROM b, generate_series(1, $1) AS g
		 RETURNING batch_id::text, id::text, batch_position`, n)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var batchID string
	ids := make([]string, n)
	for rows.Next() {
		var (
			id  string
			pos int
		)
		if err := rows.Scan(&batchID, &id, &pos); err != nil {
			t.Fatal(err)
		}
		ids[pos] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return batchID, ids
}

// setStatus commits a status change, with the lease the schema asks of a
// processing row.
func setStatus(t *testing.T, pool *pgxpool.Pool, id, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE notifications SET
		    status           = $2::text::notification_status,
		    lease_token      = CASE WHEN $2::text = 'processing' THEN gen_random_uuid() END,
		    lease_expires_at = CASE WHEN $2::text = 'processing' THEN now() + interval '1 minute' END,
		    updated_at       = clock_timestamp()
		 WHERE id = $1::uuid`, id, status); err != nil {
		t.Errorf("set status of %s to %s: %v", id, status, err)
	}
}

// flip changes a queued row's status n times, ending processing when n is odd.
func flip(t *testing.T, pool *pgxpool.Pool, id string, n int) {
	t.Helper()
	for i := range n {
		if i%2 == 0 {
			setStatus(t, pool, id, "processing")
		} else {
			setStatus(t, pool, id, "queued")
		}
	}
}

func event(id string, batchID *string, version int64) domain.StatusEvent {
	return domain.StatusEvent{
		NotificationID: id, BatchID: batchID, Status: domain.StatusQueued,
		Version: version, UpdatedAt: time.Now().UTC(),
	}
}

// TestWebSocketUnknownID: an unknown or malformed ID gets 404 before any
// upgrade (K25).
func TestWebSocketUnknownID(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	for _, path := range []string{
		"/ws/notifications/01900000-0000-7000-8000-000000000000",
		"/ws/batches/01900000-0000-7000-8000-000000000000",
		"/ws/notifications/not-a-uuid",
		"/ws/batches/not-a-uuid",
	} {
		c, resp, err := e.dialResp(path, nil)
		if err == nil || c != nil {
			t.Fatalf("%s: upgraded", path)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestWebSocketSnapshotThenUpdate: the first message is the snapshot; a
// committed status change follows with a higher version (B4).
func TestWebSocketSnapshotThenUpdate(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	id := insertNotification(t, e.pool)
	c := e.dial("/ws/notifications/" + id)

	snap := readStatus(t, c)
	if snap.Type != "snapshot" || snap.NotificationID != id || snap.Status != "queued" || snap.Version != 0 ||
		snap.BatchID != nil || snap.UpdatedAt.IsZero() {
		t.Fatalf("snapshot %+v", snap)
	}
	setStatus(t, e.pool, id, "processing")
	up := readStatus(t, c)
	if up.Type != "update" || up.NotificationID != id || up.Status != "processing" || up.Version != 1 {
		t.Fatalf("update %+v, want processing version 1", up)
	}
}

// TestSnapshotNoGap: a change committed after the registration and before
// the snapshot read is not lost: the snapshot holds it, its buffered event is
// not sent again, and the next change follows (K24).
func TestSnapshotNoGap(t *testing.T) {
	// The hook runs on the server's goroutine; the test hands it the pool
	// and the ID through atomics.
	var (
		pool atomic.Pointer[pgxpool.Pool]
		idv  atomic.Value
	)
	e := newWSEnv(t, wsOptions{hooks: httpapi.WSHooks{
		BeforeSnapshot: func(*http.Request) { setStatus(t, pool.Load(), idv.Load().(string), "processing") },
	}})
	pool.Store(e.pool)
	id := insertNotification(t, e.pool)
	idv.Store(id)
	c := e.dial("/ws/notifications/" + id)

	first := readStatus(t, c)
	var last httpapi.StatusMessage
	switch {
	case first.Status == "processing" && first.Version == 1:
		last = first
	case first.Status == "queued" && first.Version == 0:
		last = readStatus(t, c)
	default:
		t.Fatalf("snapshot %+v", first)
	}
	if last.Status != "processing" || last.Version != 1 {
		t.Fatalf("newest status %+v, want processing version 1", last)
	}
	setStatus(t, e.pool, id, "queued")
	if next := readStatus(t, c); next.Type != "update" || next.Version != 2 || next.Status != "queued" {
		t.Fatalf("next message %+v, want the update to version 2 (no repeat of version 1)", next)
	}
}

// TestOlderEventsDropped: events buffered before the snapshot whose version
// is at or below the snapshot's are not sent (K24).
func TestOlderEventsDropped(t *testing.T) {
	var (
		hub atomic.Pointer[realtime.Hub]
		idv atomic.Value
	)
	e := newWSEnv(t, wsOptions{hooks: httpapi.WSHooks{
		BeforeSnapshot: func(*http.Request) {
			for v := range int64(3) {
				hub.Load().Publish(event(idv.Load().(string), nil, v))
			}
		},
	}})
	hub.Store(e.hub)
	id := insertNotification(t, e.pool)
	idv.Store(id)
	flip(t, e.pool, id, 2) // version 2, queued

	c := e.dial("/ws/notifications/" + id)
	if snap := readStatus(t, c); snap.Version != 2 || snap.Status != "queued" {
		t.Fatalf("snapshot %+v, want queued version 2", snap)
	}
	setStatus(t, e.pool, id, "processing")
	if next := readStatus(t, c); next.Version != 3 || next.Status != "processing" {
		t.Fatalf("message after the snapshot %+v, want version 3; versions 0 to 2 must be dropped", next)
	}
}

// TestBatchWebSocket: the snapshot lists every item; a change of one item
// arrives as one update (B4).
func TestBatchWebSocket(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	batchID, ids := insertBatch(t, e.pool, 3)
	c := e.dial("/ws/batches/" + batchID)

	snap := readBatch(t, c)
	if snap.Type != "snapshot" || snap.BatchID != batchID || len(snap.Notifications) != 3 {
		t.Fatalf("snapshot type %q batch %s with %d items", snap.Type, snap.BatchID, len(snap.Notifications))
	}
	for i, n := range snap.Notifications {
		if n.NotificationID != ids[i] || n.Status != "queued" || n.Version != 0 || n.BatchID == nil || *n.BatchID != batchID {
			t.Fatalf("item %d: %+v", i, n)
		}
	}
	setStatus(t, e.pool, ids[1], "cancelled")
	up := readStatus(t, c)
	if up.Type != "update" || up.NotificationID != ids[1] || up.Status != "cancelled" || up.Version != 1 ||
		up.BatchID == nil || *up.BatchID != batchID {
		t.Fatalf("update %+v", up)
	}
}

// TestBatchSnapshotMaxSize: a batch of 1,000, the largest allowed (K12),
// arrives as one message well below the client's 1 MiB read limit.
func TestBatchSnapshotMaxSize(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	batchID, _ := insertBatch(t, e.pool, 1000)
	c := e.dial("/ws/batches/" + batchID)

	data := read(t, c, 10*time.Second)
	var snap httpapi.BatchSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Notifications) != 1000 {
		t.Fatalf("snapshot has %d items, want 1000", len(snap.Notifications))
	}
	t.Logf("snapshot of a 1,000-item batch: %d bytes", len(data))
	if len(data) >= clientReadLimit/2 {
		t.Fatalf("snapshot is %d bytes, want below %d (half the client's read limit)", len(data), clientReadLimit/2)
	}
}

// TestBatchVersionPerNotification: each event is compared with its own
// notification's version, never with another's (K24).
func TestBatchVersionPerNotification(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	batchID, ids := insertBatch(t, e.pool, 2)
	a, b := ids[0], ids[1]
	flip(t, e.pool, a, 5)
	flip(t, e.pool, b, 1)

	c := e.dial("/ws/batches/" + batchID)
	snap := readBatch(t, c)
	if snap.Notifications[0].Version != 5 || snap.Notifications[1].Version != 1 {
		t.Fatalf("snapshot versions A %d B %d, want 5 and 1", snap.Notifications[0].Version, snap.Notifications[1].Version)
	}
	e.hub.Publish(event(a, &batchID, 4))
	e.hub.Publish(event(b, &batchID, 2))
	e.hub.Publish(event(b, &batchID, 3)) // a marker: nothing may come between
	for _, want := range []int64{2, 3} {
		got := readStatus(t, c)
		if got.NotificationID != b || got.Version != want {
			t.Fatalf("got %s version %d, want B version %d (A version 4 must be dropped)", got.NotificationID, got.Version, want)
		}
	}
}

// TestListenerDropClosesSockets: when the listener's connection drops, every
// socket of that generation is closed; once it is back, a new client gets a
// fresh snapshot (K24, K46).
func TestListenerDropClosesSockets(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	id := insertNotification(t, e.pool)
	batchID, _ := insertBatch(t, e.pool, 2)
	gen, _ := e.hub.Live()

	one := e.dial("/ws/notifications/" + id)
	two := e.dial("/ws/batches/" + batchID)
	readStatus(t, one)
	readBatch(t, two)

	e.terminateListener()
	for name, c := range map[string]*websocket.Conn{"notification": one, "batch": two} {
		if status, n := closeStatus(t, c, 5*time.Second); status != websocket.StatusTryAgainLater || n != 0 {
			t.Fatalf("%s socket: close status %d after %d messages, want 1013 with none", name, status, n)
		}
	}

	e.waitLive(gen)
	setStatus(t, e.pool, id, "processing")
	again := e.dial("/ws/notifications/" + id)
	if snap := readStatus(t, again); snap.Type != "snapshot" || snap.Status != "processing" || snap.Version != 1 {
		t.Fatalf("snapshot after reconnect %+v", snap)
	}
}

// TestWebSocketWhileListenerDown: while no listener generation is live, both
// endpoints answer 503 with Retry-After: 1 and do not upgrade (K46).
func TestWebSocketWhileListenerDown(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	id := insertNotification(t, e.pool)
	batchID, _ := insertBatch(t, e.pool, 1)
	e.stopListen()
	if _, live := e.hub.Live(); live {
		t.Fatal("hub still live after the listener stopped")
	}

	for _, path := range []string{"/ws/notifications/" + id, "/ws/batches/" + batchID} {
		c, resp, err := e.dialResp(path, nil)
		if err == nil || c != nil {
			t.Fatalf("%s: upgraded", path)
		}
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
			t.Fatalf("%s: status %d Retry-After %q, want 503 with 1", path, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
	}
}

// TestListenerDropDuringUpgrade: a listener drop between the upgrade and the
// registration refuses the registration, even though a newer generation may
// already be live; the client gets a close frame and no snapshot (K46).
func TestListenerDropDuringUpgrade(t *testing.T) {
	var (
		calls   atomic.Int32
		reached = make(chan struct{})
		release = make(chan struct{})
	)
	e := newWSEnv(t, wsOptions{hooks: httpapi.WSHooks{
		AfterUpgrade: func(*http.Request) {
			if calls.Add(1) == 1 {
				close(reached)
				<-release
			}
		},
	}})
	id := insertNotification(t, e.pool)
	gen, _ := e.hub.Live()

	c := e.dial("/ws/notifications/" + id)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not reach the hook")
	}
	e.terminateListener()
	// The drop is handled: the generation the request saw has ended.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if g, live := e.hub.Live(); !live || g != gen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener drop was not handled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	if status, n := closeStatus(t, c, 5*time.Second); status != websocket.StatusTryAgainLater || n != 0 {
		t.Fatalf("close status %d after %d messages, want 1013 with no snapshot", status, n)
	}
	if n := e.hub.Subscriptions(); n != 0 {
		t.Fatalf("hub holds %d subscriptions, want none", n)
	}

	e.waitLive(gen)
	again := e.dial("/ws/notifications/" + id)
	if snap := readStatus(t, again); snap.Type != "snapshot" {
		t.Fatalf("new client got %+v, want a snapshot", snap)
	}
}

// TestSlowClientDisconnected: a client whose writer cannot keep up is closed
// when its queue fills; another client of the same batch gets every update
// within a second of its commit (K25).
func TestSlowClientDisconnected(t *testing.T) {
	e := newWSEnv(t, wsOptions{queue: 16, hooks: httpapi.WSHooks{
		// The slow connection's writer stops before its first write, so its
		// queue really fills; small messages alone could sit in TCP buffers.
		BeforeFirstWrite: func(ctx context.Context, r *http.Request) {
			if r.Header.Get("X-Test-Client") == "slow" {
				<-ctx.Done()
			}
		},
	}})
	batchID, ids := insertBatch(t, e.pool, 1)
	id := ids[0]

	slow, _, err := e.dialResp("/ws/batches/"+batchID, http.Header{"X-Test-Client": {"slow"}})
	if err != nil {
		t.Fatal(err)
	}
	fast := e.dial("/ws/batches/" + batchID)
	readBatch(t, fast)
	deadline := time.Now().Add(5 * time.Second)
	for e.hub.Subscriptions() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d subscriptions, want 2", e.hub.Subscriptions())
		}
		time.Sleep(5 * time.Millisecond)
	}

	for i := 1; i <= 20; i++ {
		status := "processing"
		if i%2 == 0 {
			status = "queued"
		}
		setStatus(t, e.pool, id, status)
		committed := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, data, err := fast.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("update %d: not received within 1s of its commit: %v", i, err)
		}
		var up httpapi.StatusMessage
		if err := json.Unmarshal(data, &up); err != nil || up.Version != int64(i) || up.Status != status {
			t.Fatalf("update %d: %s (err %v)", i, data, err)
		}
		if lag := time.Since(committed); lag > time.Second {
			t.Fatalf("update %d arrived %s after its commit", i, lag)
		}
	}

	if status, n := closeStatus(t, slow, 5*time.Second); status != websocket.StatusPolicyViolation || n != 0 {
		t.Fatalf("slow client: close status %d after %d messages, want 1008 with none", status, n)
	}
}

// TestControlFrames: the server reads the connection, so a ping gets its
// pong (K25). The client's own reader runs too, since the library answers
// control frames only while a connection is read.
func TestControlFrames(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	id := insertNotification(t, e.pool)
	c := e.dial("/ws/notifications/" + id)
	readStatus(t, c)
	c.CloseRead(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// TestWebSocketShutdown: the API's shutdown sequence, http.Server.Shutdown
// then the bounded WebSocket wait, closes every open socket with 1001 (K25).
func TestWebSocketShutdown(t *testing.T) {
	const closeTimeout = time.Second

	run := func(t *testing.T, silent bool) {
		e := newWSEnv(t, wsOptions{})
		id := insertNotification(t, e.pool)
		returned := make(chan time.Time, 1)
		e.srv.Config.RegisterOnShutdown(func() {
			e.conns.StartClose()
			returned <- time.Now()
		})

		type result struct {
			status websocket.StatusCode
			at     time.Time
		}
		results := make(chan result, 3)
		for range 3 {
			c := e.dial("/ws/notifications/" + id)
			readStatus(t, c)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _, err := c.Read(ctx)
				results <- result{websocket.CloseStatus(err), time.Now()}
			}()
		}
		if silent {
			// This client never reads again, so it never answers the close.
			quiet := e.dial("/ws/notifications/" + id)
			readStatus(t, quiet)
		}

		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.srv.Config.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, closeTimeout)
		e.conns.Wait(waitCtx)
		waitCancel()
		took := time.Since(start)

		returnedAt := <-returned
		for range 3 {
			r := <-results
			if r.status != websocket.StatusGoingAway {
				t.Fatalf("client close status %d, want 1001", r.status)
			}
			if !returnedAt.Before(r.at) {
				t.Fatal("the registered function returned after a client received its close")
			}
		}
		if took > closeTimeout+time.Second {
			t.Fatalf("shutdown sequence took %s, want at most %s", took, closeTimeout+time.Second)
		}
		if n := e.conns.Open(); n != 0 {
			t.Fatalf("%d connections still open after the wait", n)
		}
		t.Logf("shutdown sequence took %s", took.Round(time.Millisecond))
	}

	t.Run("every client answers", func(t *testing.T) { run(t, false) })
	t.Run("one client never answers", func(t *testing.T) { run(t, true) })
}

// TestWebSocketNoLeak: 50 connections opened and closed leave no goroutine
// once the sockets, the listener and the server are closed (K45). newWSEnv
// runs goleak.VerifyNone after its cleanup.
func TestWebSocketNoLeak(t *testing.T) {
	e := newWSEnv(t, wsOptions{})
	id := insertNotification(t, e.pool)
	for range 50 {
		c := e.dial("/ws/notifications/" + id)
		readStatus(t, c)
		if err := c.Close(websocket.StatusNormalClosure, ""); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("close: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.conns.Open() != 0 || e.hub.Subscriptions() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections and %d subscriptions left", e.conns.Open(), e.hub.Subscriptions())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
