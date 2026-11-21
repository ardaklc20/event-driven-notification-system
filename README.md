# Event-Driven Notification System

[English](#quick-start) · [Türkçe](#olay-güdümlü-bildirim-sistemi) · [Full reference](docs/REFERENCE.md)

A Go service that queues SMS, email and push notifications in PostgreSQL and delivers them asynchronously to an external provider. The core case and all six bonuses are implemented: failure handling, scheduling, templates, WebSocket updates, distributed tracing and CI.

## Quick start

You need a running Docker with Compose v2. Examples use `curl` and `jq`; host-side tests and tools use Go 1.27.4 and golangci-lint v2.15.0.

```bash
docker compose up
```

`docker-compose up` also works. Compose starts PostgreSQL, applies the versioned migrations, then starts the API and worker; a local mock provider makes delivery work without an external URL. For a background start with a readiness check:

```bash
docker compose up -d --build
scripts/wait-health.sh api healthy 60
curl -sS http://localhost:8080/readyz
```

| Address | Purpose |
|---|---|
| [localhost:8080/docs](http://localhost:8080/docs) | Interactive API docs; the viewer loads from a CDN |
| [localhost:8080/openapi.yaml](http://localhost:8080/openapi.yaml) | OpenAPI 3.1; JSON at `/openapi.json` |
| [localhost:8080/healthz](http://localhost:8080/healthz) | Process liveness |
| [localhost:8080/readyz](http://localhost:8080/readyz) | Readiness, including PostgreSQL |
| [localhost:8081/stats](http://localhost:8081/stats) | Mock provider arrival counts |

`docker compose down` stops the stack. Adding `-v` deletes its database volume. Ports bind to `127.0.0.1`; use `API_PORT`, `MOCK_PORT`, `POSTGRES_PORT`, `JAEGER_UI_PORT` and `BIND_ADDRESS` to change them ([defaults](docs/REFERENCE.md#host-ports)).

## Architecture

```text
client → API → PostgreSQL queue ← worker → provider
          202 immediately          claim, send, save result
```

One image contains four programs: `api`, `worker`, `migrate` and `mockprovider`.

- **API:** validates and stores a notification as `queued`, then returns `202` without calling the provider. Batches contain 1–1,000 items and commit all or nothing.
- **Queue:** the `notifications` table is the durable queue. Workers poll and claim rows with `SKIP LOCKED`; the notification lease, shared channel permit and circuit-breaker probe are coordinated in PostgreSQL.
- **Worker:** claims `high → normal → low` within each channel, posts `{to, channel, content}`, retries temporary failures with backoff up to five attempts and recovers expired leases. Scale with `docker compose up --scale worker=2`.
- **Rate limit:** at most 100 send starts per channel in any one-second window across all workers. The conservative permit spacing normally yields about 82–89 starts per second; remote arrivals can bunch together ([design and measurements](docs/REFERENCE.md#rate-limit-and-priority)).
- **Observability:** structured logs carry a correlation ID; metrics read one consistent database snapshot. PostgreSQL `LISTEN/NOTIFY` feeds WebSocket status updates. Optional OpenTelemetry tracing links the request to delivery attempts.

**Delivery contract:** accepted work is stored durably, expired leases are recovered, and retries are bounded. This does not guarantee a provider receives every notification: attempts count when claimed, so repeated crashes before the POST can exhaust them. An unclear provider outcome can cause a duplicate on retry. `sent` means the provider accepted the request; `failed` does not prove it was never delivered. [Failure rules and guarantees](docs/REFERENCE.md#retries-and-failures).

## API examples

Create one notification; the response is `202` with `id` and initial status `queued`:

```bash
RESP=$(curl -sS -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1234-sms' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 123456","priority":"high"}')
echo "$RESP"; ID=$(echo "$RESP" | jq -r .id)
```

Read its current state or list matching notifications; delivery continues in the background:

```bash
curl -sS "http://localhost:8080/notifications/$ID" | jq .
curl -sS 'http://localhost:8080/notifications?channel=sms&status=sent&limit=20' | jq .
```

A repeated request with the same `Idempotency-Key` and normalized content returns the first `202` unchanged; different content with that key gets `422`. The key is optional, global and retained. List filters combine `status`, `channel` and a `created_at` date range; follow `next_cursor` for subsequent pages.

Cancel while `queued` with the command below. Repeating a successful cancel returns `200`; `processing`, `sent` or `failed` returns `409` with `current_status`. A successful cancel stops future attempts; an earlier attempt may already have reached the provider.

```bash
curl -sS -X POST "http://localhost:8080/notifications/$ID/cancel" | jq .
```

Create a batch and read its status counts and first page:

```bash
RESP=$(curl -sS -X POST http://localhost:8080/notifications/batch \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: campaign-42' \
  -d '{"notifications":[
        {"recipient":"+905551234567","channel":"sms","content":"one","priority":"high"},
        {"recipient":"a@example.com","channel":"email","content":"two"},
        {"recipient":"device-token-1","channel":"push","content":"three","priority":"low"}]}')
echo "$RESP"; BATCH_ID=$(echo "$RESP" | jq -r .batch_id)
curl -sS "http://localhost:8080/batches/$BATCH_ID?limit=50" | jq .
```

Required fields, recipient formats and content limits are checked before saving. Default limits are 160 Unicode code points for SMS, 10,000 for email and 1,000 for push; these are application limits. Malformed JSON gets `400`, validation errors `422`, and a batch body above 16 MiB gets `413`. [Complete API rules and examples](docs/REFERENCE.md#api).

## Bonuses and reference

| Feature | Implementation and example |
|---|---|
| Failure handling | Shared circuit breaker pauses claims during provider outages; [probe and recovery rules](docs/REFERENCE.md#provider-outage) |
| Scheduling | Optional absolute `scheduled_at`; [schedule a notification](docs/REFERENCE.md#scheduled-notifications) |
| Templates | Immutable stored templates with `{{name}}` substitution; [create and use one](docs/REFERENCE.md#templates) |
| Real-time status | Notification/batch WebSockets send a snapshot, then best-effort updates; [messages and `wswatch`](docs/REFERENCE.md#real-time-status) |
| Distributed tracing | Optional OTLP/HTTP and Jaeger profile; [start the tracing demo](docs/REFERENCE.md#tracing) |
| CI | GitHub Actions runs lint, race-enabled tests, OpenAPI verification and image build; [workflow](.github/workflows/ci.yml) |

The [full bilingual reference](docs/REFERENCE.md) preserves all operational details, examples and recorded measurements: [metrics](docs/REFERENCE.md#metrics), [configuration](docs/REFERENCE.md#configuration), [webhook.site integration](docs/REFERENCE.md#webhooksite-check), [migrations](docs/REFERENCE.md#migrations), [release checks](docs/REFERENCE.md#release-check) and [project layout](docs/REFERENCE.md#project-layout). Run its shell commands from the repository root.

## Tests

With Docker running:

```bash
go test -count=1 ./...
```

Database tests use a real PostgreSQL container (`postgres:18.7-trixie`, matching Compose), one per package and a fresh migrated database per test. Deterministic `httptest` providers cover delivery outcomes; end-to-end tests run the API, worker and provider together. `-count=1` disables cached test results.

```bash
go test -count=1 -race ./...
golangci-lint run
docker build .
```

CI also compares generated OpenAPI with [api/openapi.yaml](api/openapi.yaml). [Detailed verification and release procedure](docs/REFERENCE.md#tests).

## Known limits

There is no authentication; Compose binds ports to localhost. Duplicate sends remain possible, idempotency keys are global, strict priority can starve `low`, listing pages are separate snapshots, and WebSocket events are not replayed. The queue and metrics share PostgreSQL; measurements describe single-machine runs. A provider outage affecting one channel pauses all three. [All limits and tradeoffs](docs/REFERENCE.md#known-limits).

## Documentation

- [Requirements](docs/REQUIREMENTS.md): case requirements and deliverables.
- [Decisions](docs/DECISIONS.md): choices, alternatives and costs.
- [Delivery plan](docs/PLAN.md) and [slice checks](docs/slices/): implementation order and evidence.
- [AI usage](docs/AI_USAGE.md): what assistants produced and how it was verified or corrected.

---

# Olay Güdümlü Bildirim Sistemi

[English](#event-driven-notification-system) · [Ayrıntılı başvuru](docs/REFERENCE.md#olay-güdümlü-bildirim-sistemi)

SMS, e-posta ve push bildirimlerini PostgreSQL'de kuyruğa alan, harici bir provider'a asenkron teslim eden Go servisi. Case'in çekirdeği ve altı bonusun tamamı uygulandı: hata yönetimi, zamanlama, şablonlar, WebSocket güncellemeleri, dağıtık izleme ve CI.

## Hızlı başlangıç

Compose v2 ile çalışan bir Docker gerekir. Örnekler `curl` ve `jq`; makinedeki testler ve araçlar Go 1.27.4 ile golangci-lint v2.15.0 kullanır.

```bash
docker compose up
```

`docker-compose up` da çalışır. Compose PostgreSQL'i başlatır, sürümlü migration'ları uygular, ardından API ve worker'ı başlatır; yerel mock provider sayesinde harici URL vermeden teslimat çalışır. Arka planda başlatıp readiness kontrolü yapmak için:

```bash
docker compose up -d --build
scripts/wait-health.sh api healthy 60
curl -sS http://localhost:8080/readyz
```

| Adres | İşlev |
|---|---|
| [localhost:8080/docs](http://localhost:8080/docs) | Etkileşimli API dokümanı; görüntüleyici CDN'den yüklenir |
| [localhost:8080/openapi.yaml](http://localhost:8080/openapi.yaml) | OpenAPI 3.1; JSON sürümü `/openapi.json` |
| [localhost:8080/healthz](http://localhost:8080/healthz) | Sürecin canlılık kontrolü |
| [localhost:8080/readyz](http://localhost:8080/readyz) | PostgreSQL dahil hazır olma kontrolü |
| [localhost:8081/stats](http://localhost:8081/stats) | Mock provider'ın geliş sayıları |

`docker compose down` stack'i durdurur. `-v` eklemek veritabanı volume'ünü siler. Portlar `127.0.0.1` üzerinde yayınlanır; `API_PORT`, `MOCK_PORT`, `POSTGRES_PORT`, `JAEGER_UI_PORT` ve `BIND_ADDRESS` ile değiştirilebilir ([varsayılanlar](docs/REFERENCE.md#host-portları)).

## Mimari

```text
istemci → API → PostgreSQL kuyruğu ← worker → provider
           hemen 202                 claim, gönder, sonucu kaydet
```

Tek imaj dört program içerir: `api`, `worker`, `migrate` ve `mockprovider`.

- **API:** bildirimi doğrulayıp `queued` olarak kaydeder; provider'ı çağırmadan `202` döner. Toplu istekler 1–1.000 öğe içerir ve ya hep ya hiç kaydedilir.
- **Kuyruk:** kalıcı kuyruk `notifications` tablosudur. Worker'lar polling yapar ve satırları `SKIP LOCKED` ile alır; bildirim lease'i, paylaşılan kanal izni ve devre kesicinin yoklama hakkı PostgreSQL'de koordine edilir.
- **Worker:** her kanalda `high → normal → low` sırasıyla claim eder, `{to, channel, content}` gönderir, geçici hataları backoff ile en fazla beş denemeye kadar tekrarlar ve süresi dolan lease'leri kurtarır. `docker compose up --scale worker=2` ile çoğaltılabilir.
- **Hız sınırı:** bütün worker'lar birlikte, kanal başına herhangi bir bir saniyelik pencerede en fazla 100 gönderim başlatır. Koruyucu izin aralığı genellikle saniyede yaklaşık 82–89 başlangıç verir; uzak provider'a gelişler kümelenebilir ([tasarım ve ölçümler](docs/REFERENCE.md#hız-sınırı-ve-öncelik)).
- **Gözlemlenebilirlik:** yapısal loglar correlation ID taşır; metrikler tutarlı tek bir veritabanı snapshot'ı okur. PostgreSQL `LISTEN/NOTIFY`, WebSocket durum güncellemelerini besler. İsteğe bağlı OpenTelemetry, istek ile teslim denemelerini birbirine bağlar.

**Teslimat sözleşmesi:** kabul edilen işler kalıcı kaydedilir, süresi dolan lease'ler kurtarılır ve tekrar denemeler sınırlıdır. Provider'ın her bildirimi alması garanti edilmez: denemeler claim sırasında sayıldığı için POST öncesindeki tekrarlanan çökmeler onları tüketebilir. Belirsiz bir provider sonucu tekrar denemede çift gönderime yol açabilir. `sent`, provider'ın isteği kabul ettiğini söyler; `failed`, bildirimin hiç teslim edilmediğini kanıtlamaz. [Hata kuralları ve garantiler](docs/REFERENCE.md#tekrar-denemeler-ve-hatalar).

## API örnekleri

Tek bildirim oluşturma; yanıt `id` ve ilk durum `queued` ile `202`'dir:

```bash
RESP=$(curl -sS -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1234-sms' -H 'X-Correlation-ID: demo-1' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 123456","priority":"high"}')
echo "$RESP"; ID=$(echo "$RESP" | jq -r .id)
```

Güncel durumu veya eşleşen bildirimleri okuyabilirsiniz; teslimat arka planda sürer:

```bash
curl -sS "http://localhost:8080/notifications/$ID" | jq .
curl -sS 'http://localhost:8080/notifications?channel=sms&status=sent&limit=20' | jq .
```

Aynı `Idempotency-Key` ve normalize edilmiş içerikle tekrar edilen istek ilk `202`'yi değiştirmeden döner; aynı anahtarla farklı içerik `422` alır. Anahtar isteğe bağlıdır, globaldir ve saklanır. Listelemede `status`, `channel` ve `created_at` tarih aralığı filtreleri birleşir; sonraki sayfalara `next_cursor` ile gidilir.

`queued` durumundayken aşağıdaki komutla iptal edilebilir. Başarılı iptalin tekrarı `200`; `processing`, `sent` veya `failed` ise `current_status` ile `409` döner. Başarılı iptal sonraki denemeleri durdurur; önceki bir deneme provider'a ulaşmış olabilir.

```bash
curl -sS -X POST "http://localhost:8080/notifications/$ID/cancel" | jq .
```

Toplu bildirim oluşturup durum sayılarını ve ilk sayfayı okuma:

```bash
RESP=$(curl -sS -X POST http://localhost:8080/notifications/batch \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: campaign-42' \
  -d '{"notifications":[
        {"recipient":"+905551234567","channel":"sms","content":"one","priority":"high"},
        {"recipient":"a@example.com","channel":"email","content":"two"},
        {"recipient":"device-token-1","channel":"push","content":"three","priority":"low"}]}')
echo "$RESP"; BATCH_ID=$(echo "$RESP" | jq -r .batch_id)
curl -sS "http://localhost:8080/batches/$BATCH_ID?limit=50" | jq .
```

Zorunlu alanlar, alıcı biçimleri ve içerik sınırları kayıt öncesinde denetlenir. Varsayılan sınırlar SMS için 160, e-posta için 10.000, push için 1.000 Unicode kod noktasıdır; bunlar uygulama sınırlarıdır. Bozuk JSON `400`, doğrulama hataları `422`, 16 MiB'yi aşan toplu istek gövdesi `413` alır. [Bütün API kuralları ve örnekleri](docs/REFERENCE.md#api-1).

## Bonuslar ve başvuru

| Özellik | Uygulama ve örnek |
|---|---|
| Hata yönetimi | Paylaşılan devre kesici provider kesintisinde claim'leri durdurur; [yoklama ve kurtarma kuralları](docs/REFERENCE.md#provider-kesintisi) |
| Zamanlama | İsteğe bağlı mutlak `scheduled_at`; [bildirim zamanlama](docs/REFERENCE.md#zamanlanmış-bildirimler) |
| Şablonlar | `{{name}}` değişkenleri içeren değişmez kayıtlı şablonlar; [oluşturma ve kullanma](docs/REFERENCE.md#şablonlar) |
| Gerçek zamanlı durum | Bildirim/toplu istek WebSocket'leri snapshot, ardından best-effort güncellemeler gönderir; [mesajlar ve `wswatch`](docs/REFERENCE.md#gerçek-zamanlı-durum) |
| Dağıtık izleme | İsteğe bağlı OTLP/HTTP ve Jaeger profili; [izleme demosu](docs/REFERENCE.md#tracing-izleme) |
| CI | GitHub Actions; lint, race denetimli test, OpenAPI doğrulama ve imaj derleme çalıştırır; [workflow](.github/workflows/ci.yml) |

[İki dilli ayrıntılı başvuru](docs/REFERENCE.md#olay-güdümlü-bildirim-sistemi), işletim ayrıntılarını, örnekleri ve kayıtlı ölçümleri korur: [metrikler](docs/REFERENCE.md#metrikler), [yapılandırma](docs/REFERENCE.md#yapılandırma), [webhook.site entegrasyonu](docs/REFERENCE.md#webhooksite-kontrolü), [migration'lar](docs/REFERENCE.md#migrationlar), [sürüm kontrolleri](docs/REFERENCE.md#sürüm-kontrolü) ve [proje yapısı](docs/REFERENCE.md#proje-yapısı). Shell komutlarını depo kökünden çalıştırın.

## Testler

Docker çalışırken:

```bash
go test -count=1 ./...
```

Veritabanı testleri Compose ile aynı gerçek PostgreSQL imajını (`postgres:18.7-trixie`) kullanır: paket başına bir container, test başına migration'ları uygulanmış yeni bir veritabanı. Deterministik `httptest` provider'ları teslim sonuçlarını sınar; uçtan uca testler API, worker ve provider'ı birlikte çalıştırır. `-count=1`, önbellekteki test sonuçlarının kullanılmasını engeller.

```bash
go test -count=1 -race ./...
golangci-lint run
docker build .
```

CI, üretilen OpenAPI'yi [api/openapi.yaml](api/openapi.yaml) ile de karşılaştırır. [Ayrıntılı doğrulama ve sürüm kontrolü](docs/REFERENCE.md#testler).

## Bilinen sınırlar

Kimlik doğrulama yoktur; Compose portları localhost'a bağlar. Çift gönderim mümkündür, idempotency anahtarları globaldir, katı öncelik `low` işlerini süresiz bekletebilir, liste sayfaları ayrı snapshot'lardır ve WebSocket olayları tekrar oynatılmaz. Kuyruk ve metrikler PostgreSQL'i paylaşır; ölçümler tek makinedeki çalıştırmaları anlatır. Provider'da bir kanalı etkileyen kesinti üçünü de durdurur. [Bütün sınırlar ve karşılıkları](docs/REFERENCE.md#bilinen-sınırlar).

## Dokümantasyon

- [Gereksinimler](docs/REQUIREMENTS.md): case gereksinimleri ve teslimler.
- [Kararlar](docs/DECISIONS.md): seçimler, alternatifler ve maliyetler.
- [Teslim planı](docs/PLAN.md) ve [dilim kontrolleri](docs/slices/): uygulama sırası ve kanıtlar.
- [Yapay zekâ kullanımı](docs/AI_USAGE.md#yapay-zekâ-kullanımı): asistanların ürettikleri ve nasıl doğrulandıkları veya düzeltildikleri.
