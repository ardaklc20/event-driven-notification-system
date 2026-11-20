# Slice 15 — Distributed tracing

[Delivery plan](../PLAN.md) · Branch `feat/tracing` · Optional ([B5]) · Depends on: slice 10

## Goal

An API request, each worker attempt and each provider call produce spans. The create transaction stores the context of the API's own server span; every attempt starts its own trace with a link to that context; the provider call is a child of the attempt, so its `traceparent` reaches the provider. Tracing is off unless an OTLP endpoint is set, and a stopped collector never stops delivery ([K27]).

## What this slice delivers

| ID | Delivered here |
|---|---|
| [B5] | Linked traces across the API, the worker and the provider call ([K27]) |

## Preconditions

- Slice 10 merged: the core flow and its tests pass ([K27]).

## Files

| Path | Purpose |
|---|---|
| `migrations/00012_traceparent.sql` | `notifications.traceparent text`, null when tracing is off |
| `internal/tracing/` | Tracer provider set-up: the OTLP/HTTP exporter only when an endpoint is set; the W3C propagator; bounded shutdown |
| `internal/httpapi/` | otelhttp server middleware ([K16]) |
| `internal/service/`, `internal/postgres/` | Store the server span's context with each notification, single and batch |
| `internal/worker/` | The attempt span with a link to the stored context |
| `internal/provider/` | otelhttp transport on the provider client, so the call is a child span and carries `traceparent`. It wraps [K42]'s start gate from outside; the gate stays the `RoundTripper` directly around `http.Transport` |
| `cmd/api/`, `cmd/worker/` | Set-up at start; flush within the shutdown budget ([K6]) |
| `docker-compose.yml` | `jaeger` service under the `tracing` profile; the OTLP variables on `api` and `worker` |
| `go.mod` | OpenTelemetry SDK, the OTLP/HTTP trace exporter, otelhttp v0.72.0, pinned ([K27]) |

## What is stored and linked ([K27])

| Point | Rule |
|---|---|
| API request | otelhttp starts the server span. If the client sent `traceparent`, the server span is its child: same trace ID, a new span ID |
| Stored value | The W3C `traceparent` of the **server span** (its trace ID and its own span ID), not the header the client sent |
| Batch | Every item stores the same context: the batch request's server span |
| Tracing off | No valid span context exists; the column stays null and nothing else changes |
| Worker attempt | A new root span per attempt, with one link to the stored context; no parent |
| Retry | Each attempt is its own span, each linked to the same stored context |
| Provider call | A child of the attempt span; its request carries a `traceparent` from the attempt's trace |
| Not in this version | Database spans |

## Compose and settings

| Item | Value |
|---|---|
| `jaeger` service | `jaegertracing/jaeger:2.23.0`, in-memory storage, UI on host port 16686, `profiles: ["tracing"]` |
| `api` and `worker` | `OTEL_EXPORTER_OTLP_ENDPOINT: ${OTEL_EXPORTER_OTLP_ENDPOINT:-}`; each sets its own `OTEL_SERVICE_NAME` (proposals: `notification-api`, `notification-worker`) |
| Rule | Tracing starts only when `OTEL_EXPORTER_OTLP_ENDPOINT` is non-empty ([K32]) |
| Demo command | `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318 docker compose --profile tracing up -d`: one variable, given to both services |

## Steps

1. **Migration** `00012`; extend `TestMigrations`.
2. **`internal/tracing`:** set-up, propagator and a shutdown bounded by the remaining shutdown budget; the exporter's own timeout is shorter than that budget, so a collector that never answers cannot hold a process past it; `TestTracingConfig`.
3. **API:** the middleware; store the server span's context in the single and batch transactions.
4. **Worker:** the attempt span with its link; the transport on the provider client, in the order otelhttp → start gate → `http.Transport`. Span work then happens before the deadline check, never between the counted start and the transport ([K42]); `TestTransportOrder`.
5. **Compose:** the `jaeger` service and the variables.
6. **Tests** below, with the SDK's in-memory exporter (`tracetest`).
7. **README:** how to start the demo, where to find the traces, how to follow a notification from the request trace to its linked attempt traces, and a screenshot.

## Tests

| Test | Package | Kind | Setup | Asserts |
|---|---|---|---|---|
| `TestTracingConfig` | `internal/tracing` | Unit | Endpoint empty; endpoint set | Empty: no exporter is created; set: an OTLP/HTTP exporter for that endpoint |
| `TestStoredTraceContext` | `internal/httpapi` | HTTP + database, in-memory exporter | Subtest *with header*: the client sends its own `traceparent`. Subtest *without header* | With header: the stored trace ID equals the header's, and the stored span ID equals the recorded server span's ID, not the header's parent ID. Without header: the stored value equals the server span's trace and span IDs |
| `TestBatchStoredTraceContext` | `internal/httpapi` | HTTP + database, in-memory exporter | Batch of 5 | All 5 rows store the batch request's server span context |
| `TestNoTraceContextWhenOff` | `internal/httpapi` | HTTP + database | Tracing off; the client sends `traceparent` | The column is null; the request gets `202` |
| `TestAttemptSpanLink` | `internal/worker` | Worker + `httptest`, in-memory exporter | A row with a stored context | The attempt span has no parent and one link whose trace and span IDs equal the stored ones |
| `TestRetryAttemptsLinked` | `internal/worker` | Worker + `httptest`, in-memory exporter | The provider answers `503`, then `202` | Two attempt spans with different trace IDs, each linked to the same stored context |
| `TestProviderTraceparent` | `internal/worker` | Worker + `httptest`, in-memory exporter | One attempt | The provider's request has a `traceparent` with the attempt span's trace ID, and its parent is the client span under the attempt span |
| `TestTransportOrder` | `internal/provider` | Unit | The provider client built with tracing on | The outer `RoundTripper` is otelhttp's, it wraps the start gate, and the gate wraps `http.Transport` directly; with a deadline already passed, the gate refuses the request and the inner transport is never called |
| `TestTracingCollectorDown` | `internal/e2e` | API + worker + `httptest` | Subtests: the endpoint is an address where nothing listens; the endpoint is a collector that accepts the connection and never replies | In both, the notification reaches `sent` within 30 seconds, and both processes shut down within their budgets |

## Acceptance checks

| Check | Command | Expected observation |
|---|---|---|
| Set-up | `go test -count=1 -v -run '^TestTracingConfig$' ./internal/tracing/` | `--- PASS: TestTracingConfig` |
| Stored context ([B5]) | `go test -count=1 -v -run '^(TestStoredTraceContext\|TestBatchStoredTraceContext\|TestNoTraceContextWhenOff)$' ./internal/httpapi/` | Three `--- PASS` lines, every subtest passing |
| Links and propagation ([B5]) | `go test -count=1 -v -run '^(TestAttemptSpanLink\|TestRetryAttemptsLinked\|TestProviderTraceparent)$' ./internal/worker/` | Three `--- PASS` lines |
| Start gate stays innermost ([K42]) | `go test -count=1 -v -run '^TestTransportOrder$' ./internal/provider/` | `--- PASS: TestTransportOrder` |
| Collector down | `go test -count=1 -v -run '^TestTracingCollectorDown$' ./internal/e2e/` | `--- PASS: TestTracingCollectorDown`, both subtests passing |
| Through the stack | [Stack check](#stack-check) | As described there |

## Stack check

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318 docker compose --profile tracing up -d
for i in $(seq 1 30); do curl -sf -o /dev/null http://localhost:16686/ && break; sleep 2; done
ID=$(curl -s -X POST http://localhost:8080/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"traced"}' | jq -r .id)
for i in $(seq 1 30); do
  STATUS=$(curl -s "http://localhost:8080/notifications/$ID" | jq -r .status)
  [ "$STATUS" = sent ] && break
  sleep 1
done
echo "status=$STATUS"
sleep 5                        # the batch span processor exports on a timer
TRACE=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-postgres}" -tAc \
  "select split_part(traceparent, '-', 2) from notifications where id = '$ID'")
curl -s http://localhost:16686/api/services | jq -r '.data[]'
curl -s "http://localhost:16686/api/traces/$TRACE" | jq '[.data[0].spans[].operationName]'
curl -s 'http://localhost:16686/api/traces?service=notification-worker&limit=20' \
  | jq --arg t "$TRACE" '[.data[].spans[] | select(any(.references[]?; .traceID == $t)) | .operationName]'
docker compose --profile tracing down
docker compose up -d
```

**Expected observation:** `status=sent` before anything else is read; if the loop ends with another status, the check fails. Both service names are listed; the request's trace holds the API's server span; at least one worker span refers to that trace. The Jaeger UI shows the same, and a screenshot of it goes into the README. The `/api/...` paths are the ones the Jaeger UI itself uses; the screenshot is the evidence the README keeps.

## Evidence for the pull request

- Output of every acceptance check and of the stack check.
- The Jaeger screenshot added to the README.

## Documentation

- `README.md`: the tracing demo, the linked-trace model and why it is a link, not one trace ([K27]); the bonus listed as done only after this pull request merges.
- `AI_USAGE.md`: slice log row; evidence row for [B5].

[B5]: ../REQUIREMENTS.md#bonus-features-in-scope-built-after-the-core
[K6]: ../DECISIONS.md#k6--process-layout-the-api-and-the-worker-run-as-separate-processes-from-one-go-module
[K16]: ../DECISIONS.md#k16--http-router-the-standard-librarys-nethttp
[K27]: ../DECISIONS.md#k27--distributed-tracing-b5-opentelemetry-over-otlphttp-linked-traces-jaeger-as-an-optional-profile
[K32]: ../DECISIONS.md#k32--configuration-typed-environment-variables-with-caarlos0env
[K42]: ../DECISIONS.md#k42--send-starts-a-permit-with-a-start-deadline
