package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

var listBase = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

type listRow struct {
	id        string
	status    string
	channel   string
	createdAt time.Time
}

// listFixture inserts the 30 rows of the repository test: every status, every
// channel, over three days, each with its own created_at.
func listFixture(t *testing.T, pool *pgxpool.Pool) []listRow {
	t.Helper()
	statuses := []string{"queued", "processing", "sent", "failed", "cancelled"}
	channels := []string{"sms", "email", "push"}
	rows := make([]listRow, 30)
	for i := range rows {
		r := listRow{
			status:    statuses[i%5],
			channel:   channels[i%3],
			createdAt: listBase.Add(time.Duration(i/10)*24*time.Hour + time.Duration(i)*time.Minute),
		}
		if err := pool.QueryRow(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, created_at,
			                            lease_token, lease_expires_at)
			 VALUES ('r', $1, 'list', 'normal', 'corr', $2::text::notification_status, $3,
			         CASE WHEN $2 = 'processing' THEN gen_random_uuid() END,
			         CASE WHEN $2 = 'processing' THEN now() + interval '30 seconds' END)
			 RETURNING id::text`, r.channel, r.status, r.createdAt).Scan(&r.id); err != nil {
			t.Fatal(err)
		}
		rows[i] = r
	}
	return rows
}

// list calls GET /notifications with query and decodes a 200 page.
func list(t *testing.T, base string, query url.Values) (int, httpapi.ListBody, []byte) {
	t.Helper()
	resp, body := do(t, http.MethodGet, base+"/notifications?"+query.Encode())
	var page httpapi.ListBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return resp.StatusCode, page, body
}

func pageIDs(p httpapi.ListBody) []string {
	out := make([]string, len(p.Items))
	for i, n := range p.Items {
		out[i] = n.ID
	}
	return out
}

// TestListFiltersHTTP: the repository test's filters give the same rows
// through GET /notifications (R6, K13).
func TestListFiltersHTTP(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	rows := listFixture(t, pool)
	day2, day3 := listBase.Add(24*time.Hour), listBase.Add(48*time.Hour)
	rfc := func(t time.Time) string { return t.Format(time.RFC3339Nano) }

	for name, tt := range map[string]struct {
		query url.Values
		match func(listRow) bool
	}{
		"none":        {url.Values{}, func(listRow) bool { return true }},
		"status":      {url.Values{"status": {"sent"}}, func(r listRow) bool { return r.status == "sent" }},
		"channel":     {url.Values{"channel": {"email"}}, func(r listRow) bool { return r.channel == "email" }},
		"from":        {url.Values{"from": {rfc(day2)}}, func(r listRow) bool { return !r.createdAt.Before(day2) }},
		"to":          {url.Values{"to": {rfc(day2)}}, func(r listRow) bool { return r.createdAt.Before(day2) }},
		"from and to": {url.Values{"from": {rfc(day2)}, "to": {rfc(day3)}}, func(r listRow) bool { return !r.createdAt.Before(day2) && r.createdAt.Before(day3) }},
		"all combined": {
			url.Values{"status": {"failed"}, "channel": {"sms"}, "from": {rfc(listBase)}, "to": {rfc(day3)}},
			func(r listRow) bool {
				return r.status == "failed" && r.channel == "sms" && !r.createdAt.Before(listBase) && r.createdAt.Before(day3)
			},
		},
	} {
		var want []listRow
		for _, r := range rows {
			if tt.match(r) {
				want = append(want, r)
			}
		}
		slices.SortFunc(want, func(a, b listRow) int { return b.createdAt.Compare(a.createdAt) })
		wantIDs := make([]string, len(want))
		for i, r := range want {
			wantIDs[i] = r.id
		}
		code, page, body := list(t, base, tt.query)
		if code != http.StatusOK || !slices.Equal(pageIDs(page), wantIDs) || page.NextCursor != "" {
			t.Errorf("%s: status %d, %d items %v, next_cursor %q; want %d items %v and no next page (body %s)",
				name, code, len(page.Items), pageIDs(page), page.NextCursor, len(wantIDs), wantIDs, body)
		}
	}
}

// TestListMalformedCursor: a cursor that is not from a previous page gets
// 400, naming the cursor (K13).
func TestListMalformedCursor(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	code, _, body := list(t, base, url.Values{"cursor": {"not-a-cursor"}})
	if code != http.StatusBadRequest || !decodeProblem(t, body).has("query.cursor") {
		t.Fatalf("status %d, body %s; want 400 naming query.cursor", code, body)
	}
}

// TestListLimit: 50 rows without limit and with an empty limit, 200 with
// limit=200, 422 naming limit with 0 and 201; following next_cursor reads
// all 250 once (K44).
func TestListLimit(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, created_at)
		 SELECT 'r', 'sms', 'c', 'normal', 'corr', now() - g * interval '1 second' FROM generate_series(1, 250) AS g`); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		raw   string
		count int
	}{
		"no limit":    {"", 50},
		"empty limit": {"limit=", 50},
		"limit 200":   {"limit=200", 200},
	} {
		resp, body := do(t, http.MethodGet, base+"/notifications?"+tt.raw)
		var page httpapi.ListBody
		if err := json.Unmarshal(body, &page); err != nil || resp.StatusCode != http.StatusOK ||
			len(page.Items) != tt.count || page.NextCursor == "" {
			t.Errorf("%s: status %d, %d items, next_cursor %q; want %d items and a next page", name,
				resp.StatusCode, len(page.Items), page.NextCursor, tt.count)
		}
	}
	for _, raw := range []string{"limit=0", "limit=201"} {
		resp, body := do(t, http.MethodGet, base+"/notifications?"+raw)
		if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("query.limit") {
			t.Errorf("%s: status %d, body %s; want 422 naming query.limit", raw, resp.StatusCode, body)
		}
	}

	seen := map[string]bool{}
	query := url.Values{"limit": {"100"}}
	for pages := 1; ; pages++ {
		code, page, body := list(t, base, query)
		if code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", pages, code, body)
		}
		for _, id := range pageIDs(page) {
			if seen[id] {
				t.Fatalf("page %d repeats %s", pages, id)
			}
			seen[id] = true
		}
		if page.NextCursor == "" {
			if pages != 3 || len(seen) != 250 {
				t.Fatalf("%d pages, %d rows; want 3 pages with all 250", pages, len(seen))
			}
			break
		}
		query.Set("cursor", page.NextCursor)
	}
}
