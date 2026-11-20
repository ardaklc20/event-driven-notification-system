# Event-Driven Notification System

*Türkçe sürüm aşağıda — [Türkçe](#olay-güdümlü-bildirim-sistemi).*

A notification service in Go that queues SMS, email and push notifications in PostgreSQL and delivers them to an external provider under a per-channel rate limit. It is built in slices; see [`docs/PLAN.md`](docs/PLAN.md) for the delivery order and status.

**Current state (slice 3):** `docker compose up` starts PostgreSQL, applies the migrations, and runs the API, a worker and a local mock provider. A created notification is delivered by the worker and reaches `sent`. Retries arrive in slice 4: until then, any provider answer other than `202`, and any network error or timeout, ends the notification as `failed` on its first attempt.

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
- On `SIGTERM` the worker stops claiming at once, lets the calls in flight finish and write their results, and exits within 10 seconds.

### Follow a notification to `sent`

```bash
ID=$(curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"hello"}' | jq -r .id)
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, attempts, provider_message_id, sent_at}'
docker compose logs --no-log-prefix worker | grep '"correlation_id":"demo-1"'
```

The worker writes two JSON lines per attempt: `provider call started` before the POST, and `provider call finished` after it with `status_code`, `duration_ms` and `outcome`. Both carry `worker_id`, `correlation_id`, `notification_id`, `channel` and `attempt`. Recipients and content are never logged.

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

The mock records when requests **arrive**, not when the worker starts them; its counters start from zero whenever the container starts. To slow it down: `MOCK_DELAY=3s docker compose up`. To send to another provider, set `PROVIDER_URL` and `PROVIDER_PROFILE` in the shell before `docker compose up`; the webhook.site check arrives in slice 4.

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
| `PROVIDER_PROFILE` | `standard` | `standard` or `webhook_site_free`; decides how a `429` is retried once retries arrive (slice 4) |
| `HTTP_TIMEOUT` | `5s` | Limit of one provider call |
| `RESULT_BUDGET` | `5s` | Part of the lease kept for writing the result; the call ends at least this long before the lease |
| `LEASE_DURATION` | `30s` | How long a claim holds its notification; at least `HTTP_TIMEOUT + RESULT_BUDGET` |
| `IN_FLIGHT` | `20` | Provider calls one worker runs at once |
| `IDLE_WAIT` | `200ms` | Pause after a claim that found no due notification |
| `SHUTDOWN_BUDGET` | `10s` | Wait for calls in flight after `SIGTERM` |
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
internal/postgres/      connection pool and all SQL: create, read, claim, result writes
internal/worker/        claim loop, lease deadline, result handling
internal/provider/      provider HTTP client
internal/mockprovider/  mock provider handlers and /stats
internal/e2e/           end-to-end tests: API, worker and a test provider on one database
internal/testdb/        test helper: PostgreSQL container and one database per test
migrations/         SQL migrations and the embed.go that embeds them
api/openapi.yaml    generated OpenAPI document
scripts/            stack check helpers
docs/               requirements, decisions, plan, AI usage
```

## Documentation

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md): the case turned into numbered, testable requirements.
- [`docs/DECISIONS.md`](docs/DECISIONS.md): every design decision, why I made it and what it costs.
- [`docs/PLAN.md`](docs/PLAN.md) and [`docs/slices/`](docs/slices/): the delivery order and each slice's checks.
- [`docs/AI_USAGE.md`](docs/AI_USAGE.md): how I used AI assistants, and what I verified, rejected or corrected.

---

# Olay Güdümlü Bildirim Sistemi

SMS, e-posta ve push bildirimlerini PostgreSQL'de kuyruğa alan ve kanal başına bir hız sınırı altında harici bir provider'a ileten, Go ile yazılmış bir bildirim servisi. Dilimler halinde geliştiriliyor; teslim sırası ve durum için [`docs/PLAN.md`](docs/PLAN.md).

**Şu anki durum (dilim 3):** `docker compose up` PostgreSQL'i başlatır, migration'ları uygular; API'yi, bir worker'ı ve yerel bir mock provider'ı çalıştırır. Oluşturulan bildirimi worker teslim eder ve bildirim `sent` olur. Tekrar denemeler dilim 4'te gelir: o zamana kadar provider'ın `202` dışındaki her cevabı, her ağ hatası ve her zaman aşımı bildirimi ilk denemesinde `failed` olarak bitirir.

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
- `SIGTERM` gelince worker yeni iş almayı hemen bırakır, süren çağrıların bitip sonuçlarını yazmasına izin verir ve 10 saniye içinde çıkar.

### Bir bildirimi `sent` olana kadar izlemek

```bash
ID=$(curl -s -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"hello"}' | jq -r .id)
curl -s "http://localhost:8080/notifications/$ID" | jq '{status, attempts, provider_message_id, sent_at}'
docker compose logs --no-log-prefix worker | grep '"correlation_id":"demo-1"'
```

Worker her deneme için iki JSON satırı yazar: POST'tan önce `provider call started`, sonra da `status_code`, `duration_ms` ve `outcome` ile `provider call finished`. İkisi de `worker_id`, `correlation_id`, `notification_id`, `channel` ve `attempt` taşır. Alıcılar ve içerik asla loglanmaz.

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

Mock isteklerin **geliş** anını kaydeder, worker'ın onları başlattığı anı değil; sayaçları container her başladığında sıfırdan başlar. Yavaşlatmak için: `MOCK_DELAY=3s docker compose up`. Başka bir provider'a göndermek için `docker compose up`'tan önce shell'de `PROVIDER_URL` ve `PROVIDER_PROFILE` verilir; webhook.site kontrolü dilim 4'te gelir.

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
| `PROVIDER_PROFILE` | `standard` | `standard` ya da `webhook_site_free`; tekrar denemeler gelince (dilim 4) bir `429`'un nasıl ele alınacağını belirler |
| `HTTP_TIMEOUT` | `5s` | Tek bir provider çağrısının sınırı |
| `RESULT_BUDGET` | `5s` | Lease'in sonucu yazmak için ayrılan kısmı; çağrı lease'ten en az bu kadar önce biter |
| `LEASE_DURATION` | `30s` | Bir claim'in bildirimi ne kadar tuttuğu; en az `HTTP_TIMEOUT + RESULT_BUDGET` |
| `IN_FLIGHT` | `20` | Bir worker'ın aynı anda yürüttüğü provider çağrısı sayısı |
| `IDLE_WAIT` | `200ms` | Zamanı gelmiş bildirim bulamayan bir claim'den sonraki bekleme |
| `SHUTDOWN_BUDGET` | `10s` | `SIGTERM` sonrasında süren çağrılar için bekleme |
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
internal/postgres/      bağlantı havuzu ve bütün SQL: oluşturma, okuma, claim, sonuç yazma
internal/worker/        claim döngüsü, lease bitiş anı, sonuçların işlenmesi
internal/provider/      provider HTTP istemcisi
internal/mockprovider/  mock provider handler'ları ve /stats
internal/e2e/           uçtan uca testler: API, worker ve bir test provider'ı tek veritabanında
internal/testdb/        test yardımcısı: PostgreSQL container'ı ve test başına bir veritabanı
migrations/         SQL migration'ları ve onları gömen embed.go
api/openapi.yaml    üretilmiş OpenAPI dokümanı
scripts/            stack kontrolü yardımcıları
docs/               gereksinimler, kararlar, plan, yapay zekâ kullanımı
```

## Dokümantasyon

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md): numaralı, test edilebilir gereksinimlere çevrilmiş case.
- [`docs/DECISIONS.md`](docs/DECISIONS.md): her tasarım kararı, neden verdiğim ve bedeli.
- [`docs/PLAN.md`](docs/PLAN.md) ve [`docs/slices/`](docs/slices/): teslim sırası ve her dilimin kontrolleri.
- [`docs/AI_USAGE.md`](docs/AI_USAGE.md): yapay zekâ asistanlarını nasıl kullandığım; neyi doğruladığım, reddettiğim ya da düzelttiğim.
