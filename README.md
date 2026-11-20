# Event-Driven Notification System

*Türkçe sürüm aşağıda — [Türkçe](#olay-güdümlü-bildirim-sistemi).*

A notification service in Go that queues SMS, email and push notifications in PostgreSQL and delivers them to an external provider under a per-channel rate limit. It is built in slices; see [`docs/PLAN.md`](docs/PLAN.md) for the delivery order and status.

**Current state (slice 4):** `docker compose up` starts PostgreSQL, applies the migrations, and runs the API, a worker and a local mock provider. A created notification is delivered by the worker and reaches `sent`. Temporary failures are retried with backoff up to five attempts, permanent ones stop at once, and a crashed worker's notification is taken back when its lease expires.

## Requirements

- Docker with Compose v2, running (Docker Desktop or any other runtime).
- For running the tests and tools on the host: Go 1.27.4 and golangci-lint v2.15.0.
- For the example commands: `curl` and `jq`.

## Quick start

```bash
docker compose up
```

`docker-compose up` works the same. The services start in this order: `postgres` becomes healthy, `migrate` applies the migrations and exits with 0, then `api` and `worker` start; `mockprovider` starts before the worker. To start in the background and wait until the API is healthy:

```bash
docker compose up -d --build
scripts/wait-health.sh api healthy 60
```

Then:

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
```

| URL | What it is |
|---|---|
| `http://localhost:8080/healthz` | Liveness: `200` whenever the process can answer; checks no dependency |
| `http://localhost:8080/readyz` | Readiness: pings PostgreSQL; `200` with each check's result, `503` naming the failing check, `503` during shutdown |
| `http://localhost:8080/openapi.yaml` | The OpenAPI 3.1 document (also `/openapi.json`) |
| `http://localhost:8080/docs` | Interactive API docs (loads its viewer from a CDN) |
| `http://localhost:8081/stats` | The mock provider's arrival counts per channel ([Mock provider](#mock-provider)) |

Stop with `docker compose down`; `docker compose down -v` also deletes the database volume, and the next start rebuilds the schema from the migrations.

## How it works

```
client ──POST /notifications──► api ──INSERT──► notifications table ◄──claim, result── worker ──POST──► provider
                                 202 at once     (PostgreSQL, the queue)
```

- The API validates the request, saves the notification as `queued` and answers `202`. It never calls the provider, so its response time does not depend on the provider.
- The `notifications` table is the queue. A worker claims the oldest due `queued` row with `FOR UPDATE SKIP LOCKED` and, in the same statement, puts it under a lease: `processing`, a new `lease_token`, `lease_expires_at` 30 seconds ahead and `attempts + 1`. Several workers can run against the same table (`docker compose up --scale worker=2`); a notification has one valid lease at a time.
- The worker posts `{to, channel, content}` to `PROVIDER_URL` with the notification's `X-Correlation-ID`, a 5-second timeout and no redirects. The call ends at least `RESULT_BUDGET` before the lease does.
- The result is written only while the row is still `processing` under the same token and the lease has not expired by the database clock. A `202` ends as `sent` with `provider_message_id`, `provider_timestamp` and `sent_at`. A `202` whose body is broken, or never finishes arriving, still ends as `sent`, with those fields empty and a warning in the log: the provider accepted the notification, and a second POST could deliver it twice.
- A temporary failure goes back to `queued` with a later `available_at`; a permanent one ends as `failed` ([Retries and failures](#retries-and-failures)).
- Every worker also takes back leases that expired because their worker crashed or stalled.
- On `SIGTERM` the worker stops claiming at once, lets the calls in flight finish and write their results, and exits within 10 seconds.

### Follow a notification to `sent`

```bash
ID=$(curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"hello"}' | jq -r .id)
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, attempts, provider_message_id, sent_at}'
docker compose logs --no-log-prefix worker | grep '"correlation_id":"demo-1"'
```

The worker writes two JSON lines per attempt: `provider call started` before the POST, and `provider call finished` after it with `status_code`, `duration_ms`, `class` and `outcome` (`sent`, `queued` for a retry, or `failed`). Both carry `worker_id`, `correlation_id`, `notification_id`, `channel` and `attempt`. Recipients and content are never logged.

## Retries and failures

Each attempt's result is classified, and the notification's next state follows from the class:

| Provider answer | Class | What happens |
|---|---|---|
| `202` | accepted | `sent`, on any attempt, the fifth included |
| Network error, timeout, `408`, `500`, `502`, `503`, `504` | temporary | Back to `queued` with a later `available_at`; `failed` after the fifth attempt |
| `429` | temporary under `standard`; permanent under `webhook_site_free` | A free webhook.site URL answers `429` only when its request quota is used up, and waiting does not reopen it, so that profile stops at once |
| `400`, `501` | permanent | `failed` on this attempt, whatever attempts are left |
| Any other code: another `2xx`, a `3xx` (redirects are never followed), another `4xx` or `5xx` | terminal | `failed` on this attempt and never retried: an unexpected `2xx` may mean the provider processed the request |

- **Backoff:** the n-th retry waits a random time between 0 and min(60 s, 2 s × 2^(n−1)) (full jitter). A valid `Retry-After`, in seconds or as an HTTP date, that asks for longer wins, even beyond 60 s. The wait is written to `available_at` in PostgreSQL, so a waiting retry holds no worker and survives a restart.
- **`last_error`** reads `<class>: <detail>`, for example `temporary: status 503`, `permanent: status 400` or `terminal: unexpected status 200`.
- **Attempts** are counted when a worker claims the notification, so an attempt counts even if the worker crashes right after its POST. At most five attempts are made.
- **Crash recovery:** every `RECOVERY_INTERVAL` (5 s) each worker takes back at most `RECOVERY_BATCH` (100) notifications whose lease has expired by the database clock: back to `queued`, or `failed` if that was the fifth attempt, with `last_error` `temporary: lease expired before a result was written`. Workers recovering at the same time never take the same row. With a 30-second lease, a crashed worker's notification is expected back within about 35 seconds while any worker runs; nothing is recovered while no worker runs. A worker whose lease has ended can never change the row, and it does not start a POST it could not finish within its lease.
- **What `failed` means:** the attempts ran out or the error was permanent. It does not prove the notification was never delivered: an earlier attempt may have timed out after the provider had already accepted it.
- **Duplicates are possible:** this is not exactly-once delivery. When the outcome of an attempt is unclear, a retry can become a second send: for example, the provider accepts the request but its answer is lost or arrives after the timeout, or the worker receives `202` and crashes before writing it. webhook.site documents no deduplication. Repeated **API requests** never create a second notification when they carry the same `Idempotency-Key`.

## API

### Create a notification

```bash
curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1234-sms' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 123456","priority":"high"}'
```

```json
{"id":"01a1f8ad-6307-7877-9585-950a91640e40","status":"queued"}
```

The API answers `202 Accepted` as soon as the notification is saved; delivery happens later in a worker. `priority` is optional (`high`, `normal` or `low`; default `normal`). Every response carries an `X-Correlation-ID` header: the client's value if it is at most 64 characters from `A–Z a–z 0–9 - _ .`, otherwise a generated one. The ID is stored on the notification.

### Read a notification

```bash
curl -s http://localhost:8080/notifications/01a1f8ad-6307-7877-9585-950a91640e40
```

Returns `id`, `recipient`, `channel`, `content`, `priority`, `status`, `attempts`, `last_error`, `provider_message_id`, `provider_timestamp`, `sent_at`, `created_at` and `updated_at`. An unknown ID gets `404`, a malformed one `422`. The three provider fields are filled when the notification is `sent`: `provider_message_id` and `provider_timestamp` come from the provider's `202` body, `sent_at` is when the acceptance was recorded. `provider_message_id` is not unique: a provider with a static response, such as a free webhook.site URL, gives every notification the same ID.

### Idempotency

`Idempotency-Key` is optional. When present it must be sent once and hold 1 to 128 characters from `A–Z a–z 0–9 . _ -`; otherwise the request gets `400` and nothing is saved.

- The same key with the same request returns the **first** `202` response, byte for byte, and creates nothing new. `GET` shows the current state.
- The same key with a different request gets `422`.
- Two requests without a key are two notifications.
- Keys are kept as long as the notification they created, and they are global, because there is no authentication. A production system would expire keys after a documented time and scope them per client.

### Validation

Every rule violation gets `422` with one entry per field (`location` and `message`); malformed JSON gets `400`. Request bodies are never echoed back in errors.

| Field | Rule |
|---|---|
| `recipient` | `sms`: E.164 (`+` and up to 15 digits). `email`: a plain address, no display name. `push`: no whitespace, at most 512 characters |
| `channel` | `sms`, `email` or `push` |
| `content` | Not empty. At most 160 (`sms`), 10,000 (`email`) or 1,000 (`push`) Unicode code points |
| all text | No NUL character (U+0000), which PostgreSQL cannot store |

The content limits are this application's limits, not carrier or platform guarantees: 160 code points fit one SMS segment only when every character is in the GSM-7 set (with other characters a segment holds 70), and 1,000 code points do not guarantee a push payload under the 4 KB that APNs and FCM allow. They are configurable and apply to new notifications only: after a limit is lowered, a replay of an accepted request still returns its first response.

## Mock provider

Compose points the worker at the `mockprovider` service by default, so the stack needs no external URL.

| Request | Answer |
|---|---|
| `POST /` with exactly `{to, channel, content}` and a known channel | `202` with `{"messageId": "<new UUID>", "status": "accepted", "timestamp": "<RFC 3339>"}`, after `MOCK_DELAY` |
| `POST /` with any other body | `400` |
| `GET /stats` | Per channel: `arrivals`, and `max_arrivals_per_second`, the most arrivals in any sliding one-second window |

```bash
curl -s http://localhost:8081/stats | jq .
```

The mock records when requests **arrive**, not when the worker starts them; its counters start from zero whenever the container starts. To slow it down: `MOCK_DELAY=3s docker compose up`. To send to another provider, set `PROVIDER_URL` and `PROVIDER_PROFILE` in the shell before `docker compose up`, for example `PROVIDER_URL=https://webhook.site/<token> PROVIDER_PROFILE=webhook_site_free docker compose up -d`.

## webhook.site check

[`scripts/webhook-check.sh`](scripts/webhook-check.sh) proves the integration against a real webhook.site URL:

```bash
docker compose stop          # the main stack; frees the host ports, keeps its volume
scripts/webhook-check.sh; echo "exit=$?"
docker compose start
```

It creates a free webhook.site URL that answers `202` with `{messageId, status, timestamp}` for this run, starts its own Compose project (empty database, one worker, `PROVIDER_PROFILE=webhook_site_free`), sends one synthetic notification and checks both sides: the notification is `sent` with the run's `messageId` and timestamp, and webhook.site received exactly one `POST` with the expected body and correlation ID. Any failed step prints `FAIL: <step>` and exits with 1; the last line of a good run is `PASS`. The project is removed on every exit. Anyone who knows a free URL can read what it received, so the script prints only the token's first group. webhook.site allows one new free URL per minute; a free URL accepts 100 requests and is deleted after 7 days.

Last run (slice 4):

```
date:           2026-11-20T16:21:44Z
url:            https://webhook.site/a05481b0-…
correlation_id: wh-check-1795191694
payload:        {"channel":"sms","content":"webhook.site check","to":"+905551234567"}
status:         sent
message_id:     msg-wh-check-1795191694
PASS
exit=0
```

## Host ports

Ports are published on `127.0.0.1` only, because the API has no authentication. Each can be changed without editing YAML:

| Variable | Default | Published as |
|---|---|---|
| `API_PORT` | `8080` | API on `127.0.0.1:8080` |
| `MOCK_PORT` | `8081` | Mock provider on `127.0.0.1:8081` |
| `POSTGRES_PORT` | `5433` | PostgreSQL on `127.0.0.1:5433` (not 5432, so it does not collide with a local PostgreSQL) |
| `BIND_ADDRESS` | `127.0.0.1` | The host address every port binds to |

Example: `API_PORT=18080 docker compose up`.

## Configuration

Each program reads typed environment variables and refuses to start with an invalid value, listing every problem at once.

### API settings

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required, not empty) | PostgreSQL connection string |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; health requests are logged at `debug` |
| `SHUTDOWN_TIMEOUT` | `10s` | Budget for in-flight requests after `SIGTERM` |
| `SMS_CONTENT_LIMIT` | `160` | Content limit for `sms`, in Unicode code points |
| `EMAIL_CONTENT_LIMIT` | `10000` | Content limit for `email` |
| `PUSH_CONTENT_LIMIT` | `1000` | Content limit for `push` |

### Worker settings

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required, not empty) | PostgreSQL connection string |
| `PROVIDER_URL` | — (required, not empty) | The one provider URL for every channel, absolute `http` or `https`; the channel travels in the body. Compose: `http://mockprovider:8081/` |
| `PROVIDER_PROFILE` | `standard` | `standard` or `webhook_site_free`; decides whether a `429` is retried ([Retries and failures](#retries-and-failures)) |
| `HTTP_TIMEOUT` | `5s` | Limit of one provider call |
| `RESULT_BUDGET` | `5s` | Part of the lease kept for writing the result; the call ends at least this long before the lease |
| `LEASE_DURATION` | `30s` | How long a claim holds its notification; at least `HTTP_TIMEOUT + RESULT_BUDGET` |
| `IN_FLIGHT` | `20` | Provider calls one worker runs at once |
| `IDLE_WAIT` | `200ms` | Pause after a claim that found no due notification |
| `SHUTDOWN_BUDGET` | `10s` | Wait for calls in flight after `SIGTERM` |
| `MAX_ATTEMPTS` | `5` | Attempts in total, the first one included |
| `BACKOFF_BASE` | `2s` | Backoff of the first retry; it doubles with each retry |
| `BACKOFF_CAP` | `60s` | Longest backoff; a longer `Retry-After` still wins |
| `RECOVERY_INTERVAL` | `5s` | How often the worker takes back expired leases |
| `RECOVERY_BATCH` | `100` | Most expired leases taken back in one round |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

### Mock provider settings

| Variable | Default | Meaning |
|---|---|---|
| `HTTP_ADDR` | `:8081` | Listen address |
| `MOCK_DELAY` | `0s` | Wait before each `202` |
| `LOG_LEVEL` | `info` | `debug` logs every arrival with its channel and correlation ID |

`migrate` reads only `DATABASE_URL`. Compose sets every value explicitly in [`docker-compose.yml`](docker-compose.yml); `PROVIDER_URL`, `PROVIDER_PROFILE` and `MOCK_DELAY` can be set from the shell.

## Tests

```bash
go test -count=1 ./...
```

Docker must be running: the database tests start a PostgreSQL container (`postgres:18.7-trixie`, the same image Compose uses) once per package and give each test its own migrated database. `-count=1` keeps `go test` from printing a cached result. CI also runs the suite with `-race`. The provider is replaced in the tests by `httptest` servers that answer each scenario deterministically; `internal/e2e` runs the API handler, the worker and such a provider against one database.

Lint and formatting:

```bash
golangci-lint run
```

## OpenAPI

The document is generated from the code with Huma and committed as [`api/openapi.yaml`](api/openapi.yaml). After changing an operation, regenerate it:

```bash
go run ./cmd/api openapi > api/openapi.yaml
```

`TestOpenAPIUpToDate` and the CI `openapi` job fail if the committed file differs from the generated one.

## Migrations

Versioned SQL files in [`migrations/`](migrations/), run by goose and embedded in the binary. Compose runs them through the one-off `migrate` service; to run them again by hand:

```bash
docker compose run --rm migrate
```

A second run applies nothing. Each migration runs in its own transaction: if one fails, it is rolled back, the ones before it stay applied, `migrate` exits with 1 and the API does not start.

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every pull request and every push to `main`: golangci-lint, `go test -count=1 -race ./...`, the OpenAPI check and `docker build`. Each job tests the pull request's head commit and logs it as `tested-sha`. There is no delivery step: Compose builds the image locally.

## Project layout

```
cmd/api/                API entry point; subcommands healthcheck and openapi
cmd/worker/             worker entry point
cmd/migrate/            migrate up
cmd/mockprovider/       mock provider entry point
internal/config/        typed environment configuration
internal/domain/        notification types, validation, request hash
internal/service/       create and read rules
internal/httpapi/       router, Huma operations, middleware
internal/postgres/      connection pool and all SQL: create, read, claim, result writes, recovery
internal/worker/        claim loop, lease deadline, retries and backoff, lease recovery
internal/provider/      provider HTTP client, response classification, Retry-After
internal/mockprovider/  mock provider handlers and /stats
internal/e2e/           end-to-end tests: API, worker and a test provider on one database
internal/testdb/        test helper: PostgreSQL container and one database per test
migrations/             SQL migrations and the embed.go that embeds them
api/openapi.yaml        generated OpenAPI document
scripts/                stack check helpers and the webhook.site check
docs/                   requirements, decisions, plan, AI usage
```

## Documentation

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md): the case turned into numbered, testable requirements.
- [`docs/DECISIONS.md`](docs/DECISIONS.md): every design decision, why I made it and what it costs.
- [`docs/PLAN.md`](docs/PLAN.md) and [`docs/slices/`](docs/slices/): the delivery order and each slice's checks.
- [`docs/AI_USAGE.md`](docs/AI_USAGE.md): how I used AI assistants, and what I verified, rejected or corrected.

---

# Olay Güdümlü Bildirim Sistemi

SMS, e-posta ve push bildirimlerini PostgreSQL'de kuyruğa alan ve kanal başına bir hız sınırı altında harici bir provider'a ileten, Go ile yazılmış bir bildirim servisi. Dilimler halinde geliştiriliyor; teslim sırası ve durum için [`docs/PLAN.md`](docs/PLAN.md).

**Şu anki durum (dilim 4):** `docker compose up` PostgreSQL'i başlatır, migration'ları uygular; API'yi, bir worker'ı ve yerel bir mock provider'ı çalıştırır. Oluşturulan bildirimi worker teslim eder ve bildirim `sent` olur. Geçici hatalar backoff ile en fazla beş denemeye kadar tekrar denenir, kalıcı hatalar hemen durur, çöken bir worker'ın bildirimi lease'i dolunca geri alınır.

## Gereksinimler

- Compose v2 ile çalışan bir Docker (Docker Desktop ya da başka bir çalıştırıcı).
- Testleri ve araçları makinede çalıştırmak için: Go 1.27.4 ve golangci-lint v2.15.0.
- Örnek komutlar için: `curl` ve `jq`.

## Hızlı başlangıç

```bash
docker compose up
```

`docker-compose up` da aynı şekilde çalışır. Servisler şu sırayla başlar: `postgres` sağlıklı hale gelir, `migrate` migration'ları uygulayıp 0 ile çıkar, ardından `api` ve `worker` başlar; `mockprovider` worker'dan önce başlar. Arka planda başlatıp API sağlıklı olana kadar beklemek için:

```bash
docker compose up -d --build
scripts/wait-health.sh api healthy 60
```

Sonra:

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
```

| URL | Ne olduğu |
|---|---|
| `http://localhost:8080/healthz` | Liveness: süreç cevap verebildiği sürece `200`; hiçbir bağımlılığı kontrol etmez |
| `http://localhost:8080/readyz` | Readiness: PostgreSQL'e ping atar; her kontrolün sonucuyla `200`, başarısız kontrolü adlandıran `503`, kapanış sırasında `503` |
| `http://localhost:8080/openapi.yaml` | OpenAPI 3.1 dokümanı (ayrıca `/openapi.json`) |
| `http://localhost:8080/docs` | Etkileşimli API dokümanı (görüntüleyicisini bir CDN'den yükler) |
| `http://localhost:8081/stats` | Mock provider'ın kanal başına geliş sayıları ([Mock provider](#mock-provider-1)) |

`docker compose down` ile durdurulur; `docker compose down -v` veritabanı volume'ünü de siler ve bir sonraki başlangıçta şema migration'lardan yeniden kurulur.

## Nasıl çalışır

```
istemci ──POST /notifications──► api ──INSERT──► notifications tablosu ◄──claim, sonuç── worker ──POST──► provider
                                  hemen 202       (PostgreSQL, kuyruk)
```

- API isteği doğrular, bildirimi `queued` olarak kaydeder ve `202` döner. Provider'ı hiç çağırmaz; bu yüzden yanıt süresi provider'a bağlı değildir.
- Kuyruk `notifications` tablosunun kendisidir. Worker, zamanı gelmiş en eski `queued` satırı `FOR UPDATE SKIP LOCKED` ile alır ve aynı ifadede onu bir lease altına koyar: `processing`, yeni bir `lease_token`, 30 saniye sonrası için `lease_expires_at` ve `attempts + 1`. Aynı tabloya karşı birden fazla worker çalışabilir (`docker compose up --scale worker=2`); bir bildirimin aynı anda tek bir geçerli lease'i olur.
- Worker `{to, channel, content}` gövdesini, bildirimin `X-Correlation-ID`'siyle, 5 saniyelik zaman aşımıyla ve yönlendirmeleri izlemeden `PROVIDER_URL`'e POST eder. Çağrı, lease bitmeden en az `RESULT_BUDGET` önce sona erer.
- Sonuç yalnızca satır hâlâ aynı token altında `processing` iken ve lease veritabanı saatine göre dolmamışken yazılır. `202`, `provider_message_id`, `provider_timestamp` ve `sent_at` ile `sent` olarak biter. Gövdesi bozuk olan ya da gelmesi hiç bitmeyen bir `202` de `sent` olarak biter; bu alanlar boş kalır ve log'a bir uyarı yazılır: provider bildirimi kabul etmiştir ve ikinci bir POST onu iki kez teslim edebilir.
- Geçici bir hata bildirimi daha sonraki bir `available_at` ile `queued`'a döndürür; kalıcı bir hata `failed` ile bitirir ([Tekrar denemeler ve hatalar](#tekrar-denemeler-ve-hatalar)).
- Her worker, worker'ı çöktüğü ya da takıldığı için süresi dolan lease'leri de geri alır.
- `SIGTERM` gelince worker yeni iş almayı hemen bırakır, süren çağrıların bitip sonuçlarını yazmasına izin verir ve 10 saniye içinde çıkar.

### Bir bildirimi `sent` olana kadar izlemek

```bash
ID=$(curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"hello"}' | jq -r .id)
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, attempts, provider_message_id, sent_at}'
docker compose logs --no-log-prefix worker | grep '"correlation_id":"demo-1"'
```

Worker her deneme için iki JSON satırı yazar: POST'tan önce `provider call started`, sonra da `status_code`, `duration_ms`, `class` ve `outcome` (`sent`, tekrar deneme için `queued` ya da `failed`) ile `provider call finished`. İkisi de `worker_id`, `correlation_id`, `notification_id`, `channel` ve `attempt` taşır. Alıcılar ve içerik asla loglanmaz.

## Tekrar denemeler ve hatalar

Her denemenin sonucu sınıflandırılır ve bildirimin sonraki durumu sınıftan çıkar:

| Provider cevabı | Sınıf | Ne olur |
|---|---|---|
| `202` | accepted | Beşinci dahil herhangi bir denemede `sent` |
| Ağ hatası, zaman aşımı, `408`, `500`, `502`, `503`, `504` | temporary | Daha sonraki bir `available_at` ile `queued`'a döner; beşinci denemeden sonra `failed` |
| `429` | `standard` altında temporary; `webhook_site_free` altında permanent | Ücretsiz bir webhook.site URL'i `429`'u yalnızca istek kotası bittiğinde döner ve beklemek kotayı açmaz; bu profil hemen durur |
| `400`, `501` | permanent | Kalan deneme sayısı ne olursa olsun bu denemede `failed` |
| Başka herhangi bir kod: başka bir `2xx`, bir `3xx` (yönlendirmeler hiç izlenmez), başka bir `4xx` ya da `5xx` | terminal | Bu denemede `failed` ve hiç tekrar denenmez: beklenmeyen bir `2xx`, provider'ın isteği işlediği anlamına gelebilir |

- **Backoff:** n'inci tekrar deneme 0 ile min(60 s, 2 s × 2^(n−1)) arasında rastgele bir süre bekler (full jitter). Saniye ya da HTTP tarihi olarak geçerli bir `Retry-After` daha uzun bir süre isterse o kazanır, 60 saniyeyi aşsa bile. Bekleme PostgreSQL'de `available_at`'e yazılır; böylece bekleyen bir tekrar deneme hiçbir worker'ı tutmaz ve yeniden başlatmadan etkilenmez.
- **`last_error`** `<sınıf>: <ayrıntı>` biçimindedir, örneğin `temporary: status 503`, `permanent: status 400` ya da `terminal: unexpected status 200`.
- **Denemeler** worker bildirimi claim ettiği anda sayılır; worker POST'undan hemen sonra çökse bile deneme sayılmış olur. En fazla beş deneme yapılır.
- **Çökme sonrası kurtarma:** her `RECOVERY_INTERVAL`'de (5 s) her worker, lease'i veritabanı saatine göre dolmuş en fazla `RECOVERY_BATCH` (100) bildirimi geri alır: `queued`'a, beşinci denemeyse `failed`'a, `last_error` `temporary: lease expired before a result was written` ile. Aynı anda kurtarma yapan worker'lar asla aynı satırı almaz. 30 saniyelik lease ile, herhangi bir worker çalıştığı sürece çöken bir worker'ın bildiriminin yaklaşık 35 saniye içinde geri gelmesi beklenir; hiçbir worker çalışmıyorsa hiçbir şey geri alınmaz. Lease'i bitmiş bir worker satırı asla değiştiremez ve lease'i içinde bitiremeyeceği bir POST'a başlamaz.
- **`failed` ne demek:** denemeler bitti ya da hata kalıcıydı. Bildirimin hiç teslim edilmediğini kanıtlamaz: daha önceki bir deneme, provider isteği kabul ettikten sonra zaman aşımına uğramış olabilir.
- **Tekrar gönderim mümkün:** bu tam olarak bir kez (exactly-once) teslim değildir. Bir denemenin sonucu belirsiz olduğunda tekrar deneme ikinci bir gönderim olabilir: örneğin provider isteği kabul eder ama cevabı kaybolur ya da zaman aşımından sonra gelir, veya worker `202` alır ve onu yazmadan çöker. webhook.site herhangi bir tekilleştirme belgelemiyor. Aynı `Idempotency-Key`'i taşıyan tekrarlanan **API istekleri** ise asla ikinci bir bildirim oluşturmaz.

## API

### Bildirim oluşturma

```bash
curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1234-sms' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 123456","priority":"high"}'
```

```json
{"id":"01a1f8ad-6307-7877-9585-950a91640e40","status":"queued"}
```

API, bildirim kaydedilir kaydedilmez `202 Accepted` döner; teslim daha sonra bir worker'da yapılır. `priority` isteğe bağlıdır (`high`, `normal` ya da `low`; varsayılan `normal`). Her yanıt bir `X-Correlation-ID` header'ı taşır: istemcinin değeri `A–Z a–z 0–9 - _ .` karakterlerinden en fazla 64 karakterse o, değilse üretilmiş bir değer. ID bildirimle birlikte saklanır.

### Bildirim okuma

```bash
curl -s http://localhost:8080/notifications/01a1f8ad-6307-7877-9585-950a91640e40
```

`id`, `recipient`, `channel`, `content`, `priority`, `status`, `attempts`, `last_error`, `provider_message_id`, `provider_timestamp`, `sent_at`, `created_at` ve `updated_at` döner. Bilinmeyen bir ID `404`, bozuk biçimli bir ID `422` alır. Üç provider alanı bildirim `sent` olduğunda dolar: `provider_message_id` ve `provider_timestamp` provider'ın `202` gövdesinden gelir, `sent_at` kabulün kaydedildiği andır. `provider_message_id` benzersiz değildir: ücretsiz bir webhook.site URL'i gibi sabit yanıt veren bir provider her bildirime aynı ID'yi verir.

### Idempotency

`Idempotency-Key` isteğe bağlıdır. Gönderildiğinde bir kez gönderilmeli ve `A–Z a–z 0–9 . _ -` karakterlerinden 1 ile 128 karakter arası olmalıdır; aksi halde istek `400` alır ve hiçbir şey kaydedilmez.

- Aynı anahtar ve aynı istek, **ilk** `202` yanıtını bayt bayt aynı döndürür ve yeni bir şey oluşturmaz. Güncel durumu `GET` gösterir.
- Aynı anahtar farklı bir istekle `422` alır.
- Anahtarsız iki istek iki ayrı bildirimdir.
- Anahtarlar oluşturdukları bildirim kadar saklanır ve kimlik doğrulama olmadığı için globaldir. Üretimde anahtarlar belgelenmiş bir süre sonra silinir ve istemci başına ayrılır.

### Doğrulama

Her kural ihlali, alan başına bir kayıtla (`location` ve `message`) `422` alır; bozuk JSON `400` alır. Hata yanıtları istek gövdesini asla geri döndürmez.

| Alan | Kural |
|---|---|
| `recipient` | `sms`: E.164 (`+` ve en fazla 15 rakam). `email`: görünen ad olmadan düz adres. `push`: boşluk yok, en fazla 512 karakter |
| `channel` | `sms`, `email` ya da `push` |
| `content` | Boş olamaz. En fazla 160 (`sms`), 10.000 (`email`) ya da 1.000 (`push`) Unicode kod noktası |
| tüm metinler | PostgreSQL'in saklayamadığı NUL karakteri (U+0000) olamaz |

İçerik sınırları bu uygulamanın sınırlarıdır, operatör ya da platform garantisi değildir: 160 kod noktası tek bir SMS parçasına ancak bütün karakterler GSM-7 kümesindeyse sığar (başka karakterlerle bir parça 70 karakter alır); 1.000 kod noktası da push payload'ının APNs ve FCM'nin izin verdiği 4 KB'ın altında kalacağını garanti etmez. Sınırlar ayarlanabilir ve yalnızca yeni bildirimlere uygulanır: bir sınır düşürüldükten sonra, kabul edilmiş bir isteğin tekrarı yine ilk yanıtını alır.

## Mock provider

Compose worker'ı varsayılan olarak `mockprovider` servisine yönlendirir; böylece stack harici bir URL'e ihtiyaç duymaz.

| İstek | Cevap |
|---|---|
| Tam olarak `{to, channel, content}` ve bilinen bir kanalla `POST /` | `MOCK_DELAY` sonrasında `{"messageId": "<yeni UUID>", "status": "accepted", "timestamp": "<RFC 3339>"}` ile `202` |
| Başka herhangi bir gövdeyle `POST /` | `400` |
| `GET /stats` | Kanal başına: `arrivals` ve kayan herhangi bir bir saniyelik penceredeki en fazla geliş, `max_arrivals_per_second` |

```bash
curl -s http://localhost:8081/stats | jq .
```

Mock isteklerin **geliş** anını kaydeder, worker'ın onları başlattığı anı değil; sayaçları container her başladığında sıfırdan başlar. Yavaşlatmak için: `MOCK_DELAY=3s docker compose up`. Başka bir provider'a göndermek için `docker compose up`'tan önce shell'de `PROVIDER_URL` ve `PROVIDER_PROFILE` verilir, örneğin `PROVIDER_URL=https://webhook.site/<token> PROVIDER_PROFILE=webhook_site_free docker compose up -d`.

## webhook.site kontrolü

[`scripts/webhook-check.sh`](scripts/webhook-check.sh) entegrasyonu gerçek bir webhook.site URL'ine karşı kanıtlar:

```bash
docker compose stop          # ana stack; host portlarını boşaltır, volume'ünü korur
scripts/webhook-check.sh; echo "exit=$?"
docker compose start
```

Script, bu çalıştırmaya ait `{messageId, status, timestamp}` ile `202` dönen ücretsiz bir webhook.site URL'i oluşturur, kendi Compose projesini başlatır (boş veritabanı, tek worker, `PROVIDER_PROFILE=webhook_site_free`), tek bir sentetik bildirim gönderir ve iki tarafı da kontrol eder: bildirim çalıştırmanın `messageId`'si ve timestamp'iyle `sent` olur, webhook.site da beklenen gövde ve correlation ID ile tam olarak bir `POST` almıştır. Başarısız her adım `FAIL: <adım>` yazar ve 1 ile çıkar; başarılı bir çalıştırmanın son satırı `PASS`'tir. Proje her çıkışta silinir. Ücretsiz bir URL'i bilen herkes aldıklarını okuyabildiği için script token'ın yalnızca ilk grubunu yazar. webhook.site dakikada bir yeni ücretsiz URL'e izin verir; ücretsiz bir URL 100 istek kabul eder ve 7 gün sonra silinir.

Son çalıştırma (dilim 4):

```
date:           2026-11-20T16:21:44Z
url:            https://webhook.site/a05481b0-…
correlation_id: wh-check-1795191694
payload:        {"channel":"sms","content":"webhook.site check","to":"+905551234567"}
status:         sent
message_id:     msg-wh-check-1795191694
PASS
exit=0
```

## Host portları

API'de kimlik doğrulama olmadığı için portlar yalnızca `127.0.0.1` üzerinde yayınlanır. Her biri YAML düzenlemeden değiştirilebilir:

| Değişken | Varsayılan | Yayınlandığı yer |
|---|---|---|
| `API_PORT` | `8080` | API, `127.0.0.1:8080` |
| `MOCK_PORT` | `8081` | Mock provider, `127.0.0.1:8081` |
| `POSTGRES_PORT` | `5433` | PostgreSQL, `127.0.0.1:5433` (yerel bir PostgreSQL ile çakışmasın diye 5432 değil) |
| `BIND_ADDRESS` | `127.0.0.1` | Bütün portların bağlandığı host adresi |

Örnek: `API_PORT=18080 docker compose up`.

## Yapılandırma

Her program tipli ortam değişkenlerini okur; geçersiz bir değerle başlamayı reddeder ve bütün sorunları tek seferde listeler.

### API ayarları

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `DATABASE_URL` | — (zorunlu, boş olamaz) | PostgreSQL bağlantı adresi |
| `HTTP_ADDR` | `:8080` | Dinleme adresi |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ya da `error`; sağlık istekleri `debug` seviyesinde loglanır |
| `SHUTDOWN_TIMEOUT` | `10s` | `SIGTERM` sonrasında süren isteklere tanınan süre |
| `SMS_CONTENT_LIMIT` | `160` | `sms` için içerik sınırı, Unicode kod noktası olarak |
| `EMAIL_CONTENT_LIMIT` | `10000` | `email` için içerik sınırı |
| `PUSH_CONTENT_LIMIT` | `1000` | `push` için içerik sınırı |

### Worker ayarları

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `DATABASE_URL` | — (zorunlu, boş olamaz) | PostgreSQL bağlantı adresi |
| `PROVIDER_URL` | — (zorunlu, boş olamaz) | Bütün kanallar için tek provider URL'i, mutlak `http` ya da `https`; kanal gövdede gider. Compose: `http://mockprovider:8081/` |
| `PROVIDER_PROFILE` | `standard` | `standard` ya da `webhook_site_free`; bir `429`'un tekrar denenip denenmeyeceğini belirler ([Tekrar denemeler ve hatalar](#tekrar-denemeler-ve-hatalar)) |
| `HTTP_TIMEOUT` | `5s` | Tek bir provider çağrısının sınırı |
| `RESULT_BUDGET` | `5s` | Lease'in sonucu yazmak için ayrılan kısmı; çağrı lease'ten en az bu kadar önce biter |
| `LEASE_DURATION` | `30s` | Bir claim'in bildirimi ne kadar tuttuğu; en az `HTTP_TIMEOUT + RESULT_BUDGET` |
| `IN_FLIGHT` | `20` | Bir worker'ın aynı anda yürüttüğü provider çağrısı sayısı |
| `IDLE_WAIT` | `200ms` | Zamanı gelmiş bildirim bulamayan bir claim'den sonraki bekleme |
| `SHUTDOWN_BUDGET` | `10s` | `SIGTERM` sonrasında süren çağrılar için bekleme |
| `MAX_ATTEMPTS` | `5` | İlki dahil toplam deneme sayısı |
| `BACKOFF_BASE` | `2s` | İlk tekrar denemenin backoff'u; her tekrar denemede iki katına çıkar |
| `BACKOFF_CAP` | `60s` | En uzun backoff; daha uzun bir `Retry-After` yine kazanır |
| `RECOVERY_INTERVAL` | `5s` | Worker'ın süresi dolan lease'leri ne sıklıkla geri aldığı |
| `RECOVERY_BATCH` | `100` | Bir turda geri alınan en fazla lease sayısı |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ya da `error` |

### Mock provider ayarları

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `HTTP_ADDR` | `:8081` | Dinleme adresi |
| `MOCK_DELAY` | `0s` | Her `202`'den önceki bekleme |
| `LOG_LEVEL` | `info` | `debug`, her gelişi kanalı ve correlation ID'siyle loglar |

`migrate` yalnızca `DATABASE_URL` okur. Compose bütün değerleri [`docker-compose.yml`](docker-compose.yml) içinde açıkça verir; `PROVIDER_URL`, `PROVIDER_PROFILE` ve `MOCK_DELAY` shell'den verilebilir.

## Testler

```bash
go test -count=1 ./...
```

Docker çalışıyor olmalı: veritabanı testleri her paket için bir kez bir PostgreSQL container'ı (`postgres:18.7-trixie`, Compose'un kullandığı imajın aynısı) başlatır ve her teste migration'ları uygulanmış kendi veritabanını verir. `-count=1`, `go test`'in önbellekten sonuç yazmasını engeller. CI test paketini ayrıca `-race` ile çalıştırır. Testlerde provider'ın yerini, her senaryoyu deterministik olarak cevaplayan `httptest` sunucuları alır; `internal/e2e`, API handler'ını, worker'ı ve böyle bir provider'ı tek bir veritabanına karşı çalıştırır.

Lint ve biçimlendirme:

```bash
golangci-lint run
```

## OpenAPI

Doküman koddan Huma ile üretilir ve [`api/openapi.yaml`](api/openapi.yaml) olarak commit'lenir. Bir operasyon değiştiğinde yeniden üretilir:

```bash
go run ./cmd/api openapi > api/openapi.yaml
```

Commit'lenmiş dosya üretilenden farklıysa `TestOpenAPIUpToDate` ve CI'daki `openapi` job'ı başarısız olur.

## Migration'lar

[`migrations/`](migrations/) altındaki sürümlü SQL dosyaları goose ile çalışır ve binary'ye gömülüdür. Compose bunları tek seferlik `migrate` servisiyle çalıştırır; elle yeniden çalıştırmak için:

```bash
docker compose run --rm migrate
```

İkinci çalıştırma hiçbir şey uygulamaz. Her migration kendi transaction'ında çalışır: biri başarısız olursa geri alınır, öncekiler uygulanmış kalır, `migrate` 1 ile çıkar ve API başlamaz.

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) her pull request'te ve `main`'e her push'ta çalışır: golangci-lint, `go test -count=1 -race ./...`, OpenAPI kontrolü ve `docker build`. Her job pull request'in head commit'ini test eder ve onu `tested-sha` olarak loglar. Bir teslim adımı yoktur: imajı Compose yerelde derler.

## Proje yapısı

```
cmd/api/                API giriş noktası; healthcheck ve openapi alt komutları
cmd/worker/             worker giriş noktası
cmd/migrate/            migrate up
cmd/mockprovider/       mock provider giriş noktası
internal/config/        tipli ortam değişkeni yapılandırması
internal/domain/        bildirim tipleri, doğrulama, istek özeti
internal/service/       oluşturma ve okuma kuralları
internal/httpapi/       router, Huma operasyonları, middleware
internal/postgres/      bağlantı havuzu ve bütün SQL: oluşturma, okuma, claim, sonuç yazma, kurtarma
internal/worker/        claim döngüsü, lease bitiş anı, tekrar denemeler ve backoff, lease kurtarma
internal/provider/      provider HTTP istemcisi, cevap sınıflandırması, Retry-After
internal/mockprovider/  mock provider handler'ları ve /stats
internal/e2e/           uçtan uca testler: API, worker ve bir test provider'ı tek veritabanında
internal/testdb/        test yardımcısı: PostgreSQL container'ı ve test başına bir veritabanı
migrations/             SQL migration'ları ve onları gömen embed.go
api/openapi.yaml        üretilmiş OpenAPI dokümanı
scripts/                stack kontrolü yardımcıları ve webhook.site kontrolü
docs/                   gereksinimler, kararlar, plan, yapay zekâ kullanımı
```

## Dokümantasyon

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md): numaralı, test edilebilir gereksinimlere çevrilmiş case.
- [`docs/DECISIONS.md`](docs/DECISIONS.md): her tasarım kararı, neden verdiğim ve bedeli.
- [`docs/PLAN.md`](docs/PLAN.md) ve [`docs/slices/`](docs/slices/): teslim sırası ve her dilimin kontrolleri.
- [`docs/AI_USAGE.md`](docs/AI_USAGE.md): yapay zekâ asistanlarını nasıl kullandığım; neyi doğruladığım, reddettiğim ya da düzelttiğim.
