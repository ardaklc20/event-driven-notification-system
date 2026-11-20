package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

const unknownUUID = "01900000-0000-7000-8000-000000000000"

func templateServer(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	store := postgres.NewStore(pool)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(store, domain.DefaultLimits()),
		Templates:     service.NewTemplates(store),
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// send makes one request with a JSON body ("" for none).
func send(t *testing.T, method, rawURL, body string, header http.Header) (reply, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// createTemplate posts a valid template and returns it.
func createTemplate(t *testing.T, base, name, channel, body string) httpapi.TemplateBody {
	t.Helper()
	resp, raw := send(t, http.MethodPost, base+"/templates",
		jsonOf(t, httpapi.TemplateRequest{Name: name, Channel: channel, Body: body}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create template: status %d %s, want 201", resp.StatusCode, raw)
	}
	var tpl httpapi.TemplateBody
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	return tpl
}

// templateRequest is a create body that uses a template.
func templateRequest(t *testing.T, templateID string, vars map[string]string) string {
	t.Helper()
	return jsonOf(t, httpapi.NotificationRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: templateID, Variables: vars})
}

func wantNothingStored(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"notifications", "idempotency_keys", "batches"} {
		if n := rows(t, pool, table); n != 0 {
			t.Fatalf("%s has %d rows, want 0", table, n)
		}
	}
}

// TestCreateTemplate: a valid template gets 201 and reads back the same; a
// body with malformed syntax gets 422 and no row; an unknown ID gets 404
// (K33, K47).
func TestCreateTemplate(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)

	created := createTemplate(t, base, "welcome", "sms", "Hello {{first_name}}, your code is {{code}}")
	if created.ID == "" || created.Name != "welcome" || created.Channel != "sms" ||
		created.Body != "Hello {{first_name}}, your code is {{code}}" || created.CreatedAt.IsZero() {
		t.Fatalf("created %+v", created)
	}
	resp, raw := send(t, http.MethodGet, base+"/templates/"+created.ID, "", nil)
	var got httpapi.TemplateBody
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("GET: status %d %s", resp.StatusCode, raw)
	}
	if got.ID != created.ID || got.Name != created.Name || got.Channel != created.Channel ||
		got.Body != created.Body || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("GET returned %+v, want %+v", got, created)
	}

	resp, raw = send(t, http.MethodPost, base+"/templates",
		jsonOf(t, httpapi.TemplateRequest{Name: "broken", Channel: "sms", Body: "Hello {{first_name"}), nil)
	if p := decodeProblem(t, raw); resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.body") {
		t.Fatalf("malformed syntax: status %d %s, want 422 on body.body", resp.StatusCode, raw)
	}
	if n := rows(t, pool, "templates"); n != 1 {
		t.Fatalf("%d templates, want 1", n)
	}

	if resp, raw := send(t, http.MethodGet, base+"/templates/"+unknownUUID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown ID: status %d %s, want 404", resp.StatusCode, raw)
	}
}

// TestTemplateFieldRules: each invalid name or body gets 422 with a field
// error, never a database error, and no row; two templates may share a name
// (K47).
func TestTemplateFieldRules(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)

	for name, tc := range map[string]struct {
		req   httpapi.TemplateRequest
		field string
	}{
		"empty name":           {httpapi.TemplateRequest{Name: "", Channel: "sms", Body: "x"}, "body.name"},
		"name of 101":          {httpapi.TemplateRequest{Name: strings.Repeat("ş", 101), Channel: "sms", Body: "x"}, "body.name"},
		"name with NUL":        {httpapi.TemplateRequest{Name: "a\u0000b", Channel: "sms", Body: "x"}, "body.name"},
		"empty body":           {httpapi.TemplateRequest{Name: "n", Channel: "sms", Body: ""}, "body.body"},
		"body of 10,001":       {httpapi.TemplateRequest{Name: "n", Channel: "sms", Body: strings.Repeat("ş", 10001)}, "body.body"},
		"body with NUL":        {httpapi.TemplateRequest{Name: "n", Channel: "sms", Body: "a\u0000b"}, "body.body"},
		"unknown channel":      {httpapi.TemplateRequest{Name: "n", Channel: "fax", Body: "x"}, "body.channel"},
		"placeholder w/ space": {httpapi.TemplateRequest{Name: "n", Channel: "sms", Body: "{{ a }}"}, "body.body"},
	} {
		resp, raw := send(t, http.MethodPost, base+"/templates", jsonOf(t, tc.req), nil)
		p := decodeProblem(t, raw)
		if resp.StatusCode != http.StatusUnprocessableEntity || !p.has(tc.field) {
			t.Fatalf("%s: status %d %s, want 422 on %s", name, resp.StatusCode, raw, tc.field)
		}
	}
	if n := rows(t, pool, "templates"); n != 0 {
		t.Fatalf("%d templates after invalid requests, want 0", n)
	}

	exact := createTemplate(t, base, strings.Repeat("ş", 100), "sms", strings.Repeat("ş", 10000))
	a := createTemplate(t, base, "welcome", "sms", "Hello")
	b := createTemplate(t, base, "welcome", "sms", "Hello again")
	if a.ID == b.ID || exact.ID == "" {
		t.Fatalf("same-name templates got IDs %s and %s, want two", a.ID, b.ID)
	}
}

// TestTemplatesImmutable: templates cannot be changed or deleted (K33).
func TestTemplatesImmutable(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "welcome", "sms", "Hello {{n}}")

	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		resp, raw := send(t, method, base+"/templates/"+tpl.ID, `{"name":"x","channel":"sms","body":"changed"}`, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status %d %s, want 405", method, resp.StatusCode, raw)
		}
	}
	var name, body string
	if err := pool.QueryRow(t.Context(), `SELECT name, body FROM templates WHERE id = $1::uuid`, tpl.ID).Scan(&name, &body); err != nil {
		t.Fatal(err)
	}
	if name != "welcome" || body != "Hello {{n}}" {
		t.Fatalf("row is %q %q, want it unchanged", name, body)
	}
}

// TestTemplateRequestRules: content and template_id are exclusive and one is
// required; the template's channel must match; the variables must match the
// placeholders. Each gets 422 and stores nothing (K33).
func TestTemplateRequestRules(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	sms := createTemplate(t, base, "welcome", "sms", "Hello {{first_name}}")
	email := createTemplate(t, base, "welcome", "email", "Hello {{first_name}}")

	for i, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"both", jsonOf(t, map[string]any{"recipient": "+905551234567", "channel": "sms", "content": "hi",
			"template_id": sms.ID, "variables": map[string]string{"first_name": "D"}}), "body.content"},
		{"neither", `{"recipient":"+905551234567","channel":"sms"}`, "body.content"},
		{"variables without a template", `{"recipient":"+905551234567","channel":"sms","content":"hi","variables":{"a":"b"}}`, "body.variables"},
		{"channel mismatch", templateRequest(t, email.ID, map[string]string{"first_name": "D"}), "body.channel"},
		{"missing variable", templateRequest(t, sms.ID, map[string]string{}), "body.variables"},
		{"extra variable", templateRequest(t, sms.ID, map[string]string{"first_name": "D", "last_name": "K"}), "body.variables"},
	} {
		resp, raw := post(t, base, tc.body, withKey(fmt.Sprintf("rules-%d", i)))
		p := decodeProblem(t, raw)
		if resp.StatusCode != http.StatusUnprocessableEntity || !p.has(tc.field) {
			t.Fatalf("%s: status %d %s, want 422 on %s", tc.name, resp.StatusCode, raw, tc.field)
		}
	}
	wantNothingStored(t, pool)
}

// TestTemplateRenderedContentRules: rendered content over the limit or empty
// gets 422 and stores nothing; the key stays free, so the same key with valid
// variables is accepted (K10, K33).
func TestTemplateRenderedContentRules(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "code", "sms", "{{code}}")
	key := withKey("rendered-rules")

	resp, raw := post(t, base, templateRequest(t, tpl.ID, map[string]string{"code": strings.Repeat("ş", 161)}), key)
	if p := decodeProblem(t, raw); resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.content") {
		t.Fatalf("over the limit: status %d %s, want 422 on body.content", resp.StatusCode, raw)
	}
	resp, raw = post(t, base, templateRequest(t, tpl.ID, map[string]string{"code": ""}), key)
	if p := decodeProblem(t, raw); resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.content") {
		t.Fatalf("empty: status %d %s, want 422 on body.content", resp.StatusCode, raw)
	}
	wantNothingStored(t, pool)

	resp, raw = post(t, base, templateRequest(t, tpl.ID, map[string]string{"code": "4821"}), key)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid variables with the same key: status %d %s, want 202", resp.StatusCode, raw)
	}
}

// TestTemplateNotificationStored: the row holds the rendered content and the
// template ID, and GET shows them (K33).
func TestTemplateNotificationStored(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "welcome", "sms", "Hello {{first_name}}, your code is {{code}}")

	resp, raw := post(t, base, templateRequest(t, strings.ToUpper(tpl.ID), map[string]string{"first_name": "Deniz", "code": "{{code}}"}), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d %s, want 202", resp.StatusCode, raw)
	}
	id := decodeAccepted(t, raw).ID
	const want = "Hello Deniz, your code is {{code}}"
	var (
		content    string
		templateID *string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT content, template_id::text FROM notifications WHERE id = $1::uuid`, id).Scan(&content, &templateID); err != nil {
		t.Fatal(err)
	}
	if content != want || templateID == nil || *templateID != tpl.ID {
		t.Fatalf("row content %q template %v, want %q and %s", content, templateID, want, tpl.ID)
	}
	resp, raw = send(t, http.MethodGet, base+"/notifications/"+id, "", nil)
	var n httpapi.NotificationBody
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &n) != nil {
		t.Fatalf("GET: status %d %s", resp.StatusCode, raw)
	}
	if n.Content != want || n.TemplateID == nil || *n.TemplateID != tpl.ID {
		t.Fatalf("GET content %q template %v", n.Content, n.TemplateID)
	}
}

// TestTemplateIdempotentRepeat: the same key and variables return the first
// 202 byte for byte and save one notification (K10, K33).
func TestTemplateIdempotentRepeat(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "welcome", "sms", "Hello {{first_name}}")
	body := templateRequest(t, tpl.ID, map[string]string{"first_name": "Deniz"})

	r1, b1 := post(t, base, body, withKey("tpl-repeat"))
	r2, b2 := post(t, base, body, withKey("tpl-repeat"))
	if r1.StatusCode != http.StatusAccepted || r2.StatusCode != http.StatusAccepted || !bytes.Equal(b1, b2) {
		t.Fatalf("first %d %s, repeat %d %s; want the same 202 bytes", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
}

// TestTemplateKeyConflict: the same key with other variables is another
// request and gets 422 (K10, K33).
func TestTemplateKeyConflict(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "welcome", "sms", "Hello {{first_name}}")

	if r, b := post(t, base, templateRequest(t, tpl.ID, map[string]string{"first_name": "Deniz"}), withKey("tpl-conflict")); r.StatusCode != http.StatusAccepted {
		t.Fatalf("first: status %d %s", r.StatusCode, b)
	}
	r, b := post(t, base, templateRequest(t, tpl.ID, map[string]string{"first_name": "Ece"}), withKey("tpl-conflict"))
	if p := decodeProblem(t, b); r.StatusCode != http.StatusUnprocessableEntity || !p.has("header."+httpapi.IdempotencyKeyHeader) {
		t.Fatalf("other variables: status %d %s, want 422 on the key", r.StatusCode, b)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
}

// TestTemplateBatchInvalidItem: one item of five that renders empty rejects
// the whole batch with its index; nothing is written (K12, K33).
func TestTemplateBatchInvalidItem(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	tpl := createTemplate(t, base, "code", "sms", "{{code}}")

	items := make([]httpapi.NotificationRequest, 5)
	for i := range items {
		code := fmt.Sprintf("code-%d", i)
		if i == 3 {
			code = ""
		}
		items[i] = httpapi.NotificationRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: tpl.ID,
			Variables: map[string]string{"code": code}}
	}
	resp, raw := postBatch(t, base, batchJSON(t, items), withKey("tpl-batch"))
	p := decodeProblem(t, raw)
	if resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.notifications[3].content") || len(p.Errors) != 1 {
		t.Fatalf("status %d %s, want 422 naming only body.notifications[3].content", resp.StatusCode, raw)
	}
	nothingWritten(t, pool)

	items[3].Variables = map[string]string{"code": "code-3"}
	resp, raw = postBatch(t, base, batchJSON(t, items), withKey("tpl-batch"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fixed batch with the same key: status %d %s, want 202", resp.StatusCode, raw)
	}
	var contents []string
	r, err := pool.Query(t.Context(), `SELECT content FROM notifications ORDER BY batch_position`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var c string
		if err := r.Scan(&c); err != nil {
			t.Fatal(err)
		}
		contents = append(contents, c)
	}
	if r.Err() != nil || strings.Join(contents, ",") != "code-0,code-1,code-2,code-3,code-4" {
		t.Fatalf("contents %v err %v", contents, r.Err())
	}
}

// TestTemplateUnknownID: an unknown template_id in a body is a field error,
// not a missing resource; GET of an unknown or malformed ID gets 404 or 422
// (K47).
func TestTemplateUnknownID(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)

	resp, raw := post(t, base, templateRequest(t, unknownUUID, map[string]string{}), withKey("tpl-unknown"))
	if p := decodeProblem(t, raw); resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.template_id") {
		t.Fatalf("single: status %d %s, want 422 on body.template_id", resp.StatusCode, raw)
	}
	items := []httpapi.NotificationRequest{{Recipient: "+905551234567", Channel: "sms", TemplateID: unknownUUID}}
	resp, raw = postBatch(t, base, batchJSON(t, items), withKey("tpl-unknown-batch"))
	if p := decodeProblem(t, raw); resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.notifications[0].template_id") {
		t.Fatalf("batch: status %d %s, want 422 on body.notifications[0].template_id", resp.StatusCode, raw)
	}
	wantNothingStored(t, pool)

	if resp, raw := send(t, http.MethodGet, base+"/templates/"+unknownUUID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown: status %d %s, want 404", resp.StatusCode, raw)
	}
	if resp, raw := send(t, http.MethodGet, base+"/templates/not-a-uuid", "", nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("GET malformed: status %d %s, want 422", resp.StatusCode, raw)
	}
}

func listTemplates(t *testing.T, base string, query url.Values) (int, httpapi.TemplateListBody, []byte) {
	t.Helper()
	resp, raw := send(t, http.MethodGet, base+"/templates?"+query.Encode(), "", nil)
	var page httpapi.TemplateListBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, page, raw
}

// TestListTemplates: newest first in pages of 10, 10 and 5, each template
// once; the default page holds all 25; limit 201 gets 422 (K13, K44, K47).
func TestListTemplates(t *testing.T) {
	pool := testdb.New(t)
	base := templateServer(t, pool)
	for i := range 25 {
		createTemplate(t, base, fmt.Sprintf("t%02d", i), "sms", "Hello {{n}}")
	}
	var want []string
	r, err := pool.Query(t.Context(), `SELECT id::text FROM templates ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var id string
		if err := r.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}

	var (
		got    []string
		sizes  []int
		cursor string
	)
	for {
		q := url.Values{"limit": {"10"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		status, page, raw := listTemplates(t, base, q)
		if status != http.StatusOK {
			t.Fatalf("status %d %s", status, raw)
		}
		sizes = append(sizes, len(page.Items))
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if fmt.Sprint(sizes) != "[10 10 5]" || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages %v; got %d IDs, want the 25 newest first, each once", sizes, len(got))
	}

	status, page, raw := listTemplates(t, base, url.Values{})
	if status != http.StatusOK || len(page.Items) != 25 || page.NextCursor != "" {
		t.Fatalf("default page: status %d with %d items %s", status, len(page.Items), raw)
	}
	if status, _, raw := listTemplates(t, base, url.Values{"limit": {"201"}}); status != http.StatusUnprocessableEntity {
		t.Fatalf("limit 201: status %d %s, want 422", status, raw)
	}
}
