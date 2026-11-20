package postgres_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func batchCommand(key string, contents ...string) domain.BatchCommand {
	items := make([]domain.NewNotification, len(contents))
	for i, c := range contents {
		items[i] = domain.NewNotification{
			Recipient: "+905551234567", Channel: domain.ChannelSMS, Content: c, Priority: domain.PriorityNormal,
		}
	}
	return domain.BatchCommand{
		Items:         items,
		CorrelationID: "batch-test",
		Key:           key,
		RequestHash:   domain.RequestHash(domain.OperationBatch, items),
		Respond: func(batchID string, ids []string) ([]byte, error) {
			return json.Marshal(map[string]any{"batch_id": batchID, "notification_ids": ids})
		},
	}
}

// TestBatchInsertFailureLeavesNothing: a trigger in the test's own database
// raises for one marked item of ten. The transaction fails and leaves no
// batch row, no notification and no key; once the trigger is gone, the same
// key saves the same batch (K12, K17).
func TestBatchInsertFailureLeavesNothing(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION fail_marked() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.content = 'marked' THEN
				RAISE EXCEPTION 'marked item refused by the test trigger';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER fail_marked BEFORE INSERT ON notifications
			FOR EACH ROW EXECUTE FUNCTION fail_marked();`); err != nil {
		t.Fatal(err)
	}

	contents := make([]string, 10)
	for i := range contents {
		contents[i] = fmt.Sprintf("item %d", i)
	}
	contents[6] = "marked"
	cmd := batchCommand("batch-atomic", contents...)

	_, err := store.CreateBatch(t.Context(), cmd)
	if err == nil {
		t.Fatal("CreateBatch succeeded although item 6 was refused")
	}
	t.Logf("CreateBatch error: %v", err)
	for _, table := range []string{"batches", "notifications", "idempotency_keys"} {
		if n := count(t.Context(), t, pool, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s has %d rows after the failed batch, want 0", table, n)
		}
	}

	if _, err := pool.Exec(t.Context(), `DROP TRIGGER fail_marked ON notifications`); err != nil {
		t.Fatal(err)
	}
	res, err := store.CreateBatch(t.Context(), cmd)
	if err != nil || res.Replayed {
		t.Fatalf("the same key after the failure: %+v err=%v, want a new batch", res, err)
	}
	if n := count(t.Context(), t, pool, "SELECT count(*) FROM notifications WHERE batch_id IS NOT NULL"); n != 10 {
		t.Errorf("%d notifications saved, want 10", n)
	}
}

// TestBatchPageQueryPlan: with two batches of 1,000 among 5,000 single
// notifications, a batch page reads notifications_batch_idx in its order, the
// cursor included, without a sort (K12, K13).
func TestBatchPageQueryPlan(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 SELECT '+905551234567', 'sms', 'single', 'normal', 'corr' FROM generate_series(1, 5000)`); err != nil {
		t.Fatal(err)
	}
	contents := make([]string, domain.MaxBatchItems)
	for i := range contents {
		contents[i] = fmt.Sprintf("item %d", i)
	}
	var batchID string
	for range 2 {
		res, err := store.CreateBatch(t.Context(), batchCommand("", contents...))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			BatchID string `json:"batch_id"`
		}
		if err := json.Unmarshal(res.Body, &body); err != nil {
			t.Fatal(err)
		}
		batchID = body.BatchID
	}
	if _, err := pool.Exec(t.Context(), `ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}
	var middle domain.Cursor
	if err := pool.QueryRow(t.Context(),
		`SELECT created_at, id::text FROM notifications WHERE batch_id = $1::text::uuid
		 ORDER BY created_at DESC, id DESC OFFSET 500 LIMIT 1`, batchID).Scan(&middle.CreatedAt, &middle.ID); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]domain.ListFilter{
		"first page":          {BatchID: batchID, Limit: 51},
		"page after a cursor": {BatchID: batchID, After: &middle, Limit: 51},
	} {
		q, args := postgres.ListQuery(f)
		var lines []string
		rows, err := pool.Query(t.Context(), "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(lines, "\n")
		t.Logf("%s:\n%s", name, plan)
		if !strings.Contains(plan, "Index Scan using notifications_batch_idx") || strings.Contains(plan, "Sort") {
			t.Errorf("%s: the plan does not read notifications_batch_idx in its order without a sort", name)
		}
	}
}
