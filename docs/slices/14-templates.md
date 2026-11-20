# Slice 14 — Templates

[Delivery plan](../PLAN.md) · Branch `feat/templates` · Optional ([B3]) · Depends on: slices 8 and 10

## Goal

Clients store immutable templates with `{{name}}` placeholders and create single or batch notifications from them. The API renders the content once, checks the result with the same [K11] rules as any content, and stores only the rendered content and the template ID; the worker sends that content unchanged on every attempt ([K33]).

## What this slice delivers

| ID | Delivered here |
|---|---|
| [B3] | Message templates with variable substitution ([K33]) |

## Preconditions

- Slices 8 and 10 merged.
- Template details follow [K47]; `GET /templates` takes [K44]'s `limit` and [K13]'s cursor.

## Files

| Path | Purpose |
|---|---|
| `migrations/00011_templates.sql` | Schema below |
| `internal/domain/` | Placeholder parsing and the one-pass renderer; no database |
| `internal/service/` | Template create, read and paged list; template requests in single and batch create |
| `internal/postgres/` | Template queries; the template lookup inside the create and batch transactions |
| `internal/httpapi/` | `POST /templates`, `GET /templates/{id}`, `GET /templates`; the new request fields |
| `api/openapi.yaml` | Regenerated |

## Schema: `00011_templates`

| Object | Content |
|---|---|
| `templates` table | `id uuid` primary key, default `uuidv7()` ([K36]); `name text` not null, not unique; `channel` (same type as `notifications.channel`) not null; `body text` not null; `created_at timestamptz` not null |
| Index on `templates` | `(created_at DESC, id DESC)`, for the paged list |
| `notifications.template_id` | Null for plain content; references `templates(id)` |

No variables are stored: templates cannot change, so the template ID and the rendered content are enough ([K33]). No update or delete statement for `templates` exists in the code.

## Template rules ([K47])

| Field or endpoint | Rule |
|---|---|
| `name` | Required; 1 to 100 code points; no NUL, which [K11] rejects in any text field because PostgreSQL's text types cannot hold it; not unique, so the list shows `created_at` to tell templates apart |
| `body` | Required; 1 to 10,000 code points; no NUL; valid placeholder syntax (below) |
| `channel` | `sms`, `email` or `push` |
| `POST /templates` | `201` with the stored template; `422` for any rule above, and no row |
| `GET /templates/{id}` | `200`; `404` for an unknown ID; `422` for a malformed ID (`format:"uuid"`, [K36]) |
| `GET /templates` | Newest first, paged with [K13]'s `(created_at, id)` cursor and [K44]'s `limit` |

Code points are counted as in [K11].

## Placeholder syntax ([K33])

| Body | Result |
|---|---|
| `Hello {{first_name}}` | Valid; one placeholder `first_name` |
| `{{a}}{{a}} {{_b2}}` | Valid; placeholders `a` and `_b2` |
| `Code { 42 }` | Valid; single braces are plain text |
| `Hello {{first_name` | `422`: not closed |
| `Hello {{}}` | `422`: empty name |
| `Hello {{1st}}` | `422`: name does not match `[A-Za-z_][A-Za-z0-9_]*` |
| `Hello {{ first_name }}` | `422`: spaces are not part of the name pattern |

## The create path with a template, in order ([K10], [K11], [K33])

1. **Field validation:** exactly one of `content` and `template_id`; `variables` is an object of strings and only appears with `template_id`; the other fields as in slice 2. Any error → `422`, key untouched.
2. **Defaults and request hash:** the hash covers the operation, the normalized `template_id` and the variables (keys sorted), and the other fields.
3. **One transaction:** reserve the key (if any) as in slice 2. A repeat returns the stored first response; nothing is rendered.
4. **Only the key's owner** loads the template and checks: the template exists (`422` with a field error on `template_id` otherwise: it is a reference inside the body, not the resource in the URL; [K47]); its channel equals the request's (`422` otherwise); the variables match the placeholders exactly (`422` for a missing or an extra one).
5. **Render in one pass, with the limit:** each placeholder is replaced by its value as given; a `{{x}}` inside a value is not rendered again. The renderer counts code points as it writes and stops with the over-limit `422` as soon as the output passes the channel's limit, so repeating a placeholder cannot build a large string from a small request ([K47]).
6. **Rendered content through [K11]:** `422` when it is empty or contains NUL; the limit was enforced in step 5.
7. Any error in steps 4–6 **rolls the transaction back**: nothing is stored and the key stays free, as for any validation error.
8. Insert the notification with the rendered `content` and `template_id`; store the `202` bytes on the key; commit.

**Batches:** every item follows steps 1 and 4–6; the templates the batch uses are loaded with one query. If any item fails, the whole batch gets `422` naming that item's index, and nothing is written ([K12]).

## Steps

1. **Migration** `00011`; extend `TestMigrations`.
2. **Parser and renderer** in `internal/domain`: `TestParseTemplate`, `TestRenderTemplate`.
3. **Template operations** with the rules above, the list paged newest first; OpenAPI regenerated: `TestCreateTemplate`, `TestTemplateFieldRules`, `TestListTemplates`.
4. **Single create with a template**, in the order above.
5. **Batch items with templates.**
6. **Worker test** that a retry sends the same content.
7. **README:** template example; the syntax; that a change means a new template; that rendered content follows the same limits ([K33]).

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestParseTemplate` | `internal/domain` | Unit, table | Every row of the syntax table | The listed result for each row |
| `TestRenderTemplate` | `internal/domain` | Unit, table | Values with plain text, `{{x}}` inside a value, a missing and an extra variable, a template that renders empty, a result one code point over the `sms` limit, a value with NUL, and a body that repeats one placeholder 1,000 times with a 100-code-point value | Plain values replaced; `{{x}}` in a value kept as it is; the other rows return the matching error. For the repeated placeholder: the over-limit error, and the renderer's output buffer never holds more than the limit plus one value ([K47]) |
| `TestCreateTemplate` | `internal/httpapi` | HTTP + database | Valid template; malformed syntax | `201` and `GET /templates/{id}` returns the same fields; `422` for the malformed one and no row |
| `TestTemplateFieldRules` | `internal/httpapi` | HTTP + database | `name` empty, of 101 code points and with NUL (`"a\u0000b"`); `body` empty, of 10,001 code points and with NUL; then two valid templates with the same name | `422` with a field error for each invalid one, never a database error, and no row; both same-name templates get `201` with different IDs ([K47]) |
| `TestTemplatesImmutable` | `internal/httpapi` | HTTP + database | `PUT`, `PATCH` and `DELETE` on `/templates/{id}` | `405`; the row is unchanged |
| `TestTemplateRequestRules` | `internal/httpapi` | HTTP + database | Both `content` and `template_id`; neither; channel mismatch; a missing variable; an extra variable | `422` for each; no notification; no key |
| `TestTemplateRenderedContentRules` | `internal/httpapi` | HTTP + database | Renders over the limit; renders empty; then the same key with valid variables | `422` twice with nothing stored; the third request gets `202`, because the key stayed free |
| `TestTemplateNotificationStored` | `internal/httpapi` | HTTP + database | Valid template request | The row holds the rendered `content` and the `template_id`; `GET /notifications/{id}` shows the rendered content |
| `TestTemplateIdempotentRepeat` | `internal/httpapi` | HTTP + database | Same key and variables twice | Same `202` bytes; one notification |
| `TestTemplateKeyConflict` | `internal/httpapi` | HTTP + database | Same key, different variables | `422`; one notification |
| `TestTemplateBatchInvalidItem` | `internal/httpapi` | HTTP + database | Batch of 5 template items; the fourth renders empty | `422` naming index 3; no batch, no notifications, no key |
| `TestTemplateUnknownID` | `internal/httpapi` | HTTP + database | A `template_id` that does not exist, in a single create and in a batch item; `GET /templates/{id}` with an unknown UUID and with `not-a-uuid` | Both bodies get `422` with a field error on `template_id`, nothing stored and the key free; the `GET`s get `404` and `422` ([K47]) |
| `TestListTemplates` | `internal/httpapi` | HTTP + database | 25 templates; pages of 10 | Newest first; pages of 10, 10 and 5; each template once; without `limit` the first page holds all 25; `limit=201` gets `422` ([K44], [K47]) |
| `TestTemplateRetrySameContent` | `internal/worker` | Worker + `httptest` | A template notification; the provider answers `503`, then `202` | Both POST bodies carry the same rendered content |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Syntax and rendering ([B3]) | `go test -count=1 -v -run '^(TestParseTemplate\|TestRenderTemplate)$' ./internal/domain/` | Two `--- PASS` lines, every subtest passing |
| Templates | `go test -count=1 -v -run '^(TestCreateTemplate\|TestTemplateFieldRules\|TestTemplatesImmutable\|TestTemplateUnknownID\|TestListTemplates)$' ./internal/httpapi/` | Five `--- PASS` lines |
| Requests with templates | `go test -count=1 -v -run '^(TestTemplateRequestRules\|TestTemplateRenderedContentRules\|TestTemplateNotificationStored\|TestTemplateIdempotentRepeat\|TestTemplateKeyConflict\|TestTemplateBatchInvalidItem)$' ./internal/httpapi/` | Six `--- PASS` lines |
| Retry content | `go test -count=1 -v -run '^TestTemplateRetrySameContent$' ./internal/worker/` | `--- PASS: TestTemplateRetrySameContent` |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
TPL=$(curl -s -X POST http://localhost:8080/templates -H 'Content-Type: application/json' \
  -d '{"name":"welcome","channel":"sms","body":"Hello {{first_name}}, your code is {{code}}"}' | jq -r .id)
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d "{\"recipient\":\"+905551234567\",\"channel\":\"sms\",\"template_id\":\"$TPL\",\"variables\":{\"first_name\":\"Deniz\",\"code\":\"{{code}}\"}}" | jq -r .id)
for i in $(seq 1 30); do
  STATUS=$(curl -s "http://localhost:8080/notifications/$ID" | jq -r .status)
  [ "$STATUS" = sent ] && break
  sleep 1
done
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, content, template_id}'
```

**Expected observation:** within 30 seconds `status` is `sent`; `content` is `Hello Deniz, your code is {{code}}` (the value was not rendered again); `template_id` equals `$TPL`.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.

## Documentation

- `README.md`: the template endpoints and syntax, an example, the limits on rendered content; the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B3].

[B3]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K10]: ../DECISIONS.md#k10--idempotency-key-an-optional-idempotency-key-header-one-key-per-create-request
[K11]: ../DECISIONS.md#k11--validation-one-set-of-rules-at-the-api-limits-counted-in-unicode-code-points
[K12]: ../DECISIONS.md#k12--batches-all-or-nothing-with-mixed-channels-and-priorities
[K13]: ../DECISIONS.md#k13--listing-filter-dates-on-created_at-paginate-with-a-created_at-id-cursor
[K33]: ../DECISIONS.md#k33--templates-b3-immutable-stored-templates-with-name-placeholders-rendered-at-the-api
[K36]: ../DECISIONS.md#k36--notification-ids-uuidv7-generated-by-postgresql
[K44]: ../DECISIONS.md#k44--page-size-50-by-default-1-to-200-422-outside
[K47]: ../DECISIONS.md#k47--template-details-a-paged-list-422-for-an-unknown-template-in-a-body
