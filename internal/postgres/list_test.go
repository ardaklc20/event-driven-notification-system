package postgres_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

var (
	statuses = []domain.Status{domain.StatusQueued, domain.StatusProcessing, domain.StatusSent,
		domain.StatusFailed, domain.StatusCancelled}
	listBase = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
)

// fixtureRow is one row of the listing fixture.
type fixtureRow struct {
	id        string
	status    domain.Status
	channel   domain.Channel
	createdAt time.Time
}

// insertAt inserts one notification with the given status, channel and
// created_at and returns its ID.
func insertAt(t *testing.T, pool *pgxpool.Pool, status domain.Status, ch domain.Channel, createdAt time.Time) string {
	t.Helper()
	var id string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, created_at,
		                            lease_token, lease_expires_at)
		 VALUES ($1, $2, 'list', 'normal', 'corr', $3::text::notification_status, $4,
		         CASE WHEN $3 = 'processing' THEN gen_random_uuid() END,
		         CASE WHEN $3 = 'processing' THEN now() + interval '30 seconds' END)
		 RETURNING id::text`, recipients[ch], string(ch), string(status), createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// listFixture inserts 30 rows: every status, every channel, over three days,
// each with its own created_at. Every status and channel pair appears twice.
func listFixture(t *testing.T, pool *pgxpool.Pool) []fixtureRow {
	t.Helper()
	rows := make([]fixtureRow, 30)
	for i := range rows {
		r := fixtureRow{
			status:    statuses[i%5],
			channel:   domain.Channels[i%3],
			createdAt: listBase.Add(time.Duration(i/10)*24*time.Hour + time.Duration(i)*time.Minute),
		}
		r.id = insertAt(t, pool, r.status, r.channel, r.createdAt)
		rows[i] = r
	}
	return rows
}

// expect returns the IDs of the fixture rows that match f, newest first.
func expect(rows []fixtureRow, f domain.ListFilter) []string {
	var match []fixtureRow
	for _, r := range rows {
		if (f.Status == "" || r.status == f.Status) && (f.Channel == "" || r.channel == f.Channel) &&
			(f.From == nil || !r.createdAt.Before(*f.From)) && (f.To == nil || r.createdAt.Before(*f.To)) {
			match = append(match, r)
		}
	}
	slices.SortFunc(match, func(a, b fixtureRow) int { return b.createdAt.Compare(a.createdAt) })
	ids := make([]string, len(match))
	for i, r := range match {
		ids[i] = r.id
	}
	return ids
}

func ids(ns []domain.Notification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

func at(t time.Time) *time.Time { return &t }

// listCases are the filters TestListFilters and the HTTP test check.
func listCases() map[string]domain.ListFilter {
	day2 := listBase.Add(24 * time.Hour)
	day3 := listBase.Add(48 * time.Hour)
	return map[string]domain.ListFilter{
		"none":          {},
		"status":        {Status: domain.StatusSent},
		"channel":       {Channel: domain.ChannelEmail},
		"from":          {From: at(day2)},
		"to":            {To: at(day2)},
		"from and to":   {From: at(day2), To: at(day3)},
		"all combined":  {Status: domain.StatusFailed, Channel: domain.ChannelSMS, From: at(listBase), To: at(day3)},
		"nothing found": {Status: domain.StatusQueued, Channel: domain.ChannelPush, From: at(day3)},
	}
}

// TestListFilters: each filter alone, and all combined, return exactly the
// matching rows, newest first (R6, K13).
func TestListFilters(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	rows := listFixture(t, pool)
	for name, f := range listCases() {
		f.Limit = 100
		got, err := store.ListNotifications(t.Context(), f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := expect(rows, f); !slices.Equal(ids(got), want) {
			t.Errorf("%s: got %d rows %v, want %d %v", name, len(got), ids(got), len(want), want)
		}
	}
}

// TestListDateRange: a row exactly at from is included, one exactly at to
// is not (K13).
func TestListDateRange(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	from := listBase.Add(500 * time.Microsecond)
	to := from.Add(time.Hour)
	atFrom := insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, from)
	inside := insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, from.Add(time.Minute))
	insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, to)
	insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, from.Add(-time.Microsecond))

	got, err := store.ListNotifications(t.Context(), domain.ListFilter{From: &from, To: &to, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{inside, atFrom}; !slices.Equal(ids(got), want) {
		t.Fatalf("got %v, want %v: the row at from included, the row at to excluded", ids(got), want)
	}
}

// walk reads every page of size n and returns the IDs page by page; between
// pages it calls between with the page number.
func walk(t *testing.T, store *postgres.Store, n int, between func(page int)) [][]string {
	t.Helper()
	var (
		pages [][]string
		after *domain.Cursor
	)
	for page := 0; ; page++ {
		got, err := store.ListNotifications(t.Context(), domain.ListFilter{After: after, Limit: n})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			return pages
		}
		pages = append(pages, ids(got))
		last := got[len(got)-1]
		after = &domain.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if between != nil {
			between(page)
		}
	}
}

// TestListCursorWithInserts: 25 rows read in pages of 7, with 5 newer rows
// inserted between page reads. Every original row appears exactly once, and
// no new row appears after the first page (R6, K13).
func TestListCursorWithInserts(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	original := map[string]bool{}
	for i := range 25 {
		original[insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, listBase.Add(time.Duration(i)*time.Second))] = true
	}
	newest := listBase.Add(24 * time.Second)
	added := map[string]bool{}
	pages := walk(t, store, 7, func(page int) {
		for i := range 5 {
			id := insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS,
				newest.Add(time.Duration(page*5+i+1)*time.Second))
			added[id] = true
		}
	})

	seen := map[string]int{}
	for p, page := range pages {
		for _, id := range page {
			seen[id]++
			if added[id] && p > 0 {
				t.Errorf("page %d shows a row inserted after the first page", p)
			}
		}
	}
	for id := range original {
		if seen[id] != 1 {
			t.Errorf("original row %s appeared %d times, want once", id, seen[id])
		}
	}
	t.Logf("%d pages of up to 7; %d rows inserted between pages", len(pages), len(added))
}

// TestListCursorTieBreak: 10 rows share one created_at and a page of 4 ends
// among them. Walking the pages returns each once, in descending ID order
// across the page boundary (K13).
func TestListCursorTieBreak(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, listBase.Add(time.Hour)) // pushes the boundary into the tie
	for range 10 {
		insertAt(t, pool, domain.StatusQueued, domain.ChannelSMS, listBase)
	}
	rows, err := pool.Query(t.Context(), `SELECT id::text FROM notifications WHERE created_at = $1 ORDER BY id DESC`, listBase)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}

	pages := walk(t, store, 4, nil)
	got := slices.Concat(pages...)[1:] // the first row is the newer one
	if len(pages) != 3 || !slices.Equal(got, want) {
		t.Fatalf("pages %v, want the 10 tied rows once each in descending ID order %v", pages, want)
	}
}

// TestListQueryPlan: with 10,000 rows and fresh statistics, the unfiltered
// first page reads notifications_list_idx. The filtered plan is logged for
// the pull request (K13).
func TestListQueryPlan(t *testing.T) {
	pool := testdb.New(t)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, created_at)
		 SELECT 'r', (ARRAY['sms','email','push'])[1 + g % 3], 'c', 'normal', 'corr',
		        (ARRAY['queued','sent','failed','cancelled'])[1 + (g / 3) % 4]::notification_status,
		        clock_timestamp() - g * interval '1 second'
		 FROM generate_series(1, 10000) AS g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}
	explain := func(f domain.ListFilter) string {
		q, args := postgres.ListQuery(f)
		rows, err := pool.Query(t.Context(), "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "\n")
	}
	var middle domain.Cursor
	if err := pool.QueryRow(t.Context(),
		`SELECT created_at, id::text FROM notifications ORDER BY created_at DESC, id DESC OFFSET 5000 LIMIT 1`).Scan(
		&middle.CreatedAt, &middle.ID); err != nil {
		t.Fatal(err)
	}
	plain := explain(domain.ListFilter{Limit: 51})
	filtered := explain(domain.ListFilter{Status: domain.StatusSent, Channel: domain.ChannelSMS, Limit: 51})
	later := explain(domain.ListFilter{After: &middle, Limit: 51})
	t.Logf("first page, no filter:\n%s", plain)
	t.Logf("first page, status=sent and channel=sms:\n%s", filtered)
	t.Logf("a page in the middle, after a cursor:\n%s", later)
	for name, plan := range map[string]string{"first page": plain, "page after a cursor": later} {
		if !strings.Contains(plan, "Index Scan using notifications_list_idx") || strings.Contains(plan, "Sort") {
			t.Errorf("%s: the plan does not read notifications_list_idx in its order without a sort", name)
		}
	}
	if !strings.Contains(later, "Index Cond") {
		t.Error("page after a cursor: the cursor is not an index condition, so the scan would read every row above it")
	}
}
