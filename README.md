# Event-Driven Notification System

*Türkçe sürüm aşağıda — [Türkçe](#olay-güdümlü-bildirim-sistemi).*

A notification service in Go that queues SMS, email and push notifications in PostgreSQL and delivers them to an external provider under a per-channel rate limit. It is built in slices; see [`docs/PLAN.md`](docs/PLAN.md) for the delivery order and status.

**Current state (slice 1, walking skeleton):** `docker compose up` starts PostgreSQL, applies the migrations and runs an API that reports its health. The notification endpoints arrive in the next slices.

## Requirements

- Docker with Compose v2, running (Docker Desktop or any other runtime).
- For running the tests and tools on the host: Go 1.27.4 and golangci-lint v2.15.0.

## Quick start

```bash
docker compose up
```

`docker-compose up` works the same. The services start in this order: `postgres` becomes healthy, `migrate` applies the migrations and exits with 0, then `api` starts. To start in the background and wait until the API is healthy:

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

Stop with `docker compose down`; `docker compose down -v` also deletes the database volume, and the next start rebuilds the schema from the migrations.

## Host ports

Ports are published on `127.0.0.1` only, because the API has no authentication. Each can be changed without editing YAML:

| Variable | Default | Published as |
|---|---|---|
| `API_PORT` | `8080` | API on `127.0.0.1:8080` |
| `POSTGRES_PORT` | `5433` | PostgreSQL on `127.0.0.1:5433` (not 5432, so it does not collide with a local PostgreSQL) |
| `BIND_ADDRESS` | `127.0.0.1` | The host address every port binds to |

Example: `API_PORT=18080 docker compose up`.

## Configuration

The API reads typed environment variables and refuses to start with an invalid value, listing every problem at once.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required, not empty) | PostgreSQL connection string |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; health requests are logged at `debug` |
| `SHUTDOWN_TIMEOUT` | `10s` | Budget for in-flight requests after `SIGTERM` |

`migrate` reads only `DATABASE_URL`. Compose sets every value explicitly in [`docker-compose.yml`](docker-compose.yml).

## Tests

```bash
go test -count=1 ./...
```

Docker must be running: the database tests start a PostgreSQL container (`postgres:18.7-trixie`, the same image Compose uses) once per package and give each test its own migrated database. `-count=1` keeps `go test` from printing a cached result. CI also runs the suite with `-race`.

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
cmd/api/            API entry point; subcommands healthcheck and openapi
cmd/migrate/        migrate up
internal/config/    typed environment configuration
internal/httpapi/   router, Huma operations, middleware
internal/postgres/  connection pool
internal/testdb/    test helper: PostgreSQL container and one database per test
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

**Şu anki durum (dilim 1, walking skeleton):** `docker compose up` PostgreSQL'i başlatır, migration'ları uygular ve sağlık durumunu bildiren bir API çalıştırır. Bildirim endpoint'leri sonraki dilimlerde geliyor.

## Gereksinimler

- Compose v2 ile çalışan bir Docker (Docker Desktop ya da başka bir çalıştırıcı).
- Testleri ve araçları makinede çalıştırmak için: Go 1.27.4 ve golangci-lint v2.15.0.

## Hızlı başlangıç

```bash
docker compose up
```

`docker-compose up` da aynı şekilde çalışır. Servisler şu sırayla başlar: `postgres` sağlıklı hale gelir, `migrate` migration'ları uygulayıp 0 ile çıkar, ardından `api` başlar. Arka planda başlatıp API sağlıklı olana kadar beklemek için:

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

`docker compose down` ile durdurulur; `docker compose down -v` veritabanı volume'ünü de siler ve bir sonraki başlangıçta şema migration'lardan yeniden kurulur.

## Host portları

API'de kimlik doğrulama olmadığı için portlar yalnızca `127.0.0.1` üzerinde yayınlanır. Her biri YAML düzenlemeden değiştirilebilir:

| Değişken | Varsayılan | Yayınlandığı yer |
|---|---|---|
| `API_PORT` | `8080` | API, `127.0.0.1:8080` |
| `POSTGRES_PORT` | `5433` | PostgreSQL, `127.0.0.1:5433` (yerel bir PostgreSQL ile çakışmasın diye 5432 değil) |
| `BIND_ADDRESS` | `127.0.0.1` | Bütün portların bağlandığı host adresi |

Örnek: `API_PORT=18080 docker compose up`.

## Yapılandırma

API tipli ortam değişkenlerini okur; geçersiz bir değerle başlamayı reddeder ve bütün sorunları tek seferde listeler.

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `DATABASE_URL` | — (zorunlu, boş olamaz) | PostgreSQL bağlantı adresi |
| `HTTP_ADDR` | `:8080` | Dinleme adresi |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ya da `error`; sağlık istekleri `debug` seviyesinde loglanır |
| `SHUTDOWN_TIMEOUT` | `10s` | `SIGTERM` sonrasında süren isteklere tanınan süre |

`migrate` yalnızca `DATABASE_URL` okur. Compose bütün değerleri [`docker-compose.yml`](docker-compose.yml) içinde açıkça verir.

## Testler

```bash
go test -count=1 ./...
```

Docker çalışıyor olmalı: veritabanı testleri her paket için bir kez bir PostgreSQL container'ı (`postgres:18.7-trixie`, Compose'un kullandığı imajın aynısı) başlatır ve her teste migration'ları uygulanmış kendi veritabanını verir. `-count=1`, `go test`'in önbellekten sonuç yazmasını engeller. CI test paketini ayrıca `-race` ile çalıştırır.

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
cmd/api/            API giriş noktası; healthcheck ve openapi alt komutları
cmd/migrate/        migrate up
internal/config/    tipli ortam değişkeni yapılandırması
internal/httpapi/   router, Huma operasyonları, middleware
internal/postgres/  bağlantı havuzu
internal/testdb/    test yardımcısı: PostgreSQL container'ı ve test başına bir veritabanı
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
