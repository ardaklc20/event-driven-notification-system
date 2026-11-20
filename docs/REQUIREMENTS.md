# Requirements

Source: Insider One — *Software Engineer Assessment 2026*, project "Event-Driven Notification System" (Go track).

Every line is written so it can be tested. `docs/AI_USAGE.md` maps each ID to the test or script that proves it.

## Functional requirements

### Notification Management API

| ID | Requirement |
|---|---|
| R1 | Create a single notification with `recipient`, `channel` (`sms` \| `email` \| `push`), `content` and `priority` (`high` \| `normal` \| `low`). The response returns an ID and the initial status without waiting for delivery. |
| R2 | Create a batch of 1–1000 notifications in one request. A request with more than 1000 items is rejected with a 4xx status. The response returns a batch ID. |
| R3 | Query a notification's status by its ID. |
| R4 | Query by batch ID: return the statuses of the notifications in that batch. |
| R5 | A pending notification can be cancelled. A successful cancel stops any new send attempt. |
| R6 | List notifications filtered by status, channel and date range (filters combine), with pagination. |

### Processing engine

| ID | Requirement |
|---|---|
| R7 | Notifications are processed asynchronously by queue workers. API response time does not depend on the provider. |
| R8 | At most 100 sends per second per channel (`sms`, `email` and `push` each have their own limit). This holds when measured under load. |
| R9 | Priority queue support with three levels: `high`, `normal` and `low`. The ordering rule between the levels is a design decision (K8 in `DECISIONS.md`). |
| R10 | Required fields and per-channel character limits are validated. |
| R11 | Idempotency support to prevent duplicate sends: a repeated API request with the same idempotency key does not create a new notification, and a notification has only one valid worker lease at a time. A retry after an unclear provider outcome can still send twice (K3 in `DECISIONS.md`). |

### Delivery & retry (left to the candidate's design)

| ID | Requirement |
|---|---|
| R12 | We design how workers deliver to the provider (calls, timeouts) and how failed deliveries are retried "intelligently". |

### External provider

| ID | Requirement |
|---|---|
| R13 | Delivery is `POST {to, channel, content}` to a webhook.site URL. A `202` with `{messageId, status, timestamp}` is expected. |

### Observability

| ID | Requirement |
|---|---|
| R14 | A metrics endpoint returns queue depth, success/failure rates and latency in real time. |
| R15 | Logs are structured and carry a correlation ID that can be followed from the API request through the worker to the provider call. |
| R16 | A health check endpoint exists. |

## Non-functional requirements (from the case text)

| ID | Requirement |
|---|---|
| N1 | Burst traffic: under a sudden load the API keeps accepting requests, the queue buffers them, and nothing is lost. |
| N2 | Scalable: workers can be scaled out. R8 must then hold across all workers together. |
| N3 | Reliable delivery: a worker crash does not lose an in-flight notification. *(Our reading of "ensure reliable delivery".)* |

## Bonus features (in scope, built after the core)

| ID | Feature |
|---|---|
| B1 | Failure handling |
| B2 | Scheduled notifications: a notification is not sent before its scheduled time. |
| B3 | Message templates with variable substitution |
| B4 | Real-time status updates over WebSocket |
| B5 | Distributed tracing |
| B6 | GitHub Actions pipeline for tests and linting |

## Deliverables

| ID | Deliverable |
|---|---|
| D1 | A GitHub or GitLab repository with a clean commit history, shared by its link. *(The case does not say "public"; we publish a public GitHub repository so that the link works for any reviewer.)* |
| D2 | README: setup, architecture overview, API examples |
| D3 | One-command setup: `docker-compose up` |
| D4 | Swagger / OpenAPI specification |
| D5 | Versioned database migrations |
| D6 | Test suite runnable with a single command |

## Assumptions

- **A1:** No authentication. The case does not ask for it. This is listed as a limitation in the README.
- **A2:** All channels use one webhook.site URL and the channel travels in the request body, as in the case's request example.

## Known constraints

- **webhook.site free URLs** accept 100 requests, are deleted after 7 days, and answer `429` once the cap is reached (source: docs.webhook.site, last updated 2026-11-07; the pricing page says 50, and we follow the documentation). A free URL can return a static `202` JSON body, but per-request dynamic values are a paid feature, so the `messageId` in the response is the same every time.
- So load tests and rate-limit tests cannot run against webhook.site. A reviewer running the project after 7 days will find our URL gone.

## Open design questions (decided in `docs/DECISIONS.md`)

1. Database
2. Queue technology. The project is named "Event-Driven", so the design should be explainable in those terms.
3. Delivery guarantee (at-least-once vs at-most-once) and how ambiguous outcomes (timeouts) are handled
4. Where the rate limiter lives (per worker vs shared by all workers)
5. Retry policy: which errors are retried, how often, backoff, and the final state. The 429 at the webhook.site cap is permanent.
6. Priority policy (strict vs weighted, starvation of `low`)
7. Status model and cancellation: what "pending" means, the HTTP code for a refused cancel, and whether a whole batch can be cancelled
8. Source of the idempotency key
9. Validation: where it runs (API vs worker), character limits per channel, and whether recipient format is validated
10. Batches: partial invalidity (reject all vs accept the valid ones), mixed channels and priorities, whether `priority` is required or has a default, and whether status counts are returned
11. Field used by the date-range filter (`created_at` / `scheduled_at` / `sent_at`)
12. Local mock provider and the provider URL configuration
13. Whether to store the provider's `messageId`
14. Process layout (API and worker in one binary or separate)
15. HTTP router, database access, migration tool, OpenAPI approach, metrics format, logging, test database, project structure
16. Planning tool
