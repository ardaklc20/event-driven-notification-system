# Design Decisions

Each entry says what I chose, why I chose it, and what it costs. I worked through the options and their trade-offs with two AI assistants: I had Claude Code draft the options and their consequences, and OpenAI Codex review them (see [`AI_USAGE.md`](AI_USAGE.md)). The choices and the reasons are mine.

## Development environment

### K0 — Local container runtime: Docker Desktop

I chose Docker Desktop because ease of installation should not decide a tool choice; I wanted the option that is soundest on its own merits.

- **Also considered:** Colima (command-line only and the lightest, but testcontainers needs extra settings) and OrbStack (light and fast, but free only for personal use).
- **What it costs:** It uses the most memory of the three, and the first launch needs administrator rights and a licence acceptance.
- **What it means for the code:** Nothing. `docker-compose.yml` stays runtime-agnostic (no Docker Desktop–only features such as `host.docker.internal`), so a reviewer can use any Docker runtime.

## Architecture

### K1 — Database: PostgreSQL

I chose PostgreSQL because schemas and SQL are ground I already know, so the queries this system needs are ones I can write and explain myself. This decision is expensive to reverse, so I read through the options carefully before choosing, and PostgreSQL came out as the most sensible fit.

- **Also considered:** MySQL 8 (it also supports `SKIP LOCKED`, but I have never used it) and MongoDB (none of my SQL or migration experience carries over, and versioned migrations fit poorly without a schema).
- **What is new for me:** Two Go concepts: the database connection pool (the counterpart of SQLAlchemy's engine pool) and errors that come back as return values instead of being raised as exceptions. I am studying both before the first slice is built.
- **What it costs:** It is hard to reverse. Migrations and every query are written in the PostgreSQL dialect, so moving to another database would take an estimated 3+ hours. Tests cannot fall back to SQLite; they need a real PostgreSQL. That is fine for me, because the project always runs with a container.
- **What it means for the code:** `docker-compose.yml` runs PostgreSQL on host port 5433, so it does not collide with a local PostgreSQL on 5432. If a stale volume ever causes a schema mismatch, I delete it (`docker compose down -v`) and the migrations rebuild the schema.

### K2 — Queue: the `notifications` table in PostgreSQL is the queue

I chose to use the `notifications` table itself as the queue because every requirement behind this decision (asynchronous processing, three priority levels, cancelling a pending notification, querying status) depends on the state of the same notification. With one row, saving a notification and queuing it happen in one transaction, and a cancel racing a worker's claim is settled atomically on the same `queued` row. The case does not require a separate message broker.

- **How it works:** Workers poll for work: a worker keeps claiming while it finds work and waits briefly when the queue is empty. Rows are claimed with `SELECT … FOR UPDATE SKIP LOCKED`, and a claimed row carries a lease, so a notification has only one valid lease at a time. If its worker dies, the lease expires and another worker takes the row again.
- **Not in the first version:** Waking workers with `LISTEN/NOTIFY`. I add it only if latency measurements show that polling is not enough.
- **Also considered:** River (a PostgreSQL job queue for Go) was a close second. It creates the job in the same transaction as the notification (`InsertTx`), completes it in the same transaction as my own updates (`JobCompleteTx`), and rescues jobs whose worker crashed. Its cost for me is keeping the notification's status and River's job state matched. Redis + Asynq and RabbitMQ would also meet the requirements, but each adds a service and needs an outbox (or an equivalent) to move a saved notification onto the queue reliably. I did not want that extra cost for this flow.
- **What it costs:** I write and test crash recovery (the lease and taking the row back) myself; River would have given me that. It is hard to reverse: the queue logic is at the centre of the worker and the repository, so moving to a broker would take an estimated 3–4 hours.
- **How I will prove it:** 3 channels × 100 messages per second = 300 is only the ceiling for sends to the provider. It says nothing about the request rate during a campaign or about PostgreSQL's capacity, so I claim no capacity number. Instead, with two workers, a real PostgreSQL and a local provider, I measure the per-channel limit, the race between cancel and claim, and taking work back after a worker crash.

### K3 — Delivery guarantee: at-least-once, with crash recovery and bounded retries

I chose to take work back when a worker crashes and to retry temporary failures a limited number of times, because the case puts reliable delivery and intelligent retries first. A repeated API request with the same idempotency key must not create a second notification, so I deduplicate repeated requests in the database. When the provider's outcome is unclear, I accept the risk of a duplicate send in order to keep delivering.

- **Where a duplicate can still happen:** Whenever the provider may have processed a request but the outcome is unclear, a retry can become a second send. Two examples: the provider accepts the request but its response is lost, or the worker receives `202` and crashes before writing the result to PostgreSQL. A `500` or `504` response does not prove that the provider did nothing either. A `202` only means the provider accepted the request, and webhook.site documents no deduplication (for example by an `Idempotency-Key` header); it records every request it receives.
- **Lease token:** Every claim gets a new `lease_token`, and every status update after the send is conditioned on it, so a worker whose lease has expired cannot overwrite the result written by the worker that took the job back. The lease covers the HTTP call and writing its result.
- **How my decision changed:** The option I was first offered said the provider would deduplicate retries by an `Idempotency-Key` header. While deciding, I had the card reviewed, saw that webhook.site documents no such deduplication, and changed the decision to say openly when a duplicate is possible. I also turned the entry's first "only in two cases" into examples, because a `500` or `504` does not prove the provider did nothing.
- **Also considered:** At-most-once (no duplicates, but after an unclear failure the notification may never be sent) and a hybrid that parks unclear outcomes in an `unknown` status (no automatic duplicate, but those notifications wait for someone to review them).
- **What it costs:** A duplicate send is possible whenever the outcome is unclear. The README states this as a known limit instead of claiming exactly-once delivery.
- **How I will prove it (R11):** Three separate tests.
  1. *API repeat:* the same idempotency key with the same content returns the existing notification.
  2. *Worker race:* a notification has only one valid lease at a time, and a worker whose lease has expired cannot overwrite the state of the job another worker has taken back.
  3. *Unclear provider outcome:* the mock provider accepts the first request and delays its response; a retry happens and the second call is observed. This test shows where the guarantee ends.
- **Decided in later cards:** K10 decides whether the idempotency key is required, whether a batch has one key or one per item, the response when the same key arrives with different content, and how long keys are kept; a content hash is one of the options there. K7 decides which errors are retried, how many times and with what backoff.

### K5 — Rate limiter: one shared permit row per channel in PostgreSQL

I chose a shared permit row per channel in PostgreSQL because every worker and every retry has to go through the same limit; otherwise adding workers would break the 100 messages per second per channel. I only want to bring in Redis if measurement shows that this row is the bottleneck.

- **How it works:** The channel permit and the notification claim happen in one short transaction. The worker locks its channel's limiter row with `FOR UPDATE SKIP LOCKED`; if another worker holds it, the worker returns at once and tries again after a short wait. It reads the database clock (`clock_timestamp()`, the actual current time, not `now()`, which is the start of the transaction). If the permit time has not come, it leaves without taking any notification. If it has, it picks a due notification with `SKIP LOCKED`, writes the lease and the next permit time (10 ms later) together, commits and starts the HTTP call. If there is no work, the permit is not used up.
- **Why the permit and the claim go together:** A notification that is only waiting for the limit stays `queued`, so it can still be cancelled (R5). If the cancel wins, no POST goes out; if the claim wins, the cancel is refused.
- **How my decision changed:** The first design let each worker reserve the next free slot ahead of time and sleep until it. Workers that wake up late would then send back-to-back, so I changed it to grant a permit only when its time has come. Later I also moved the permit into the claim's transaction, for the reason above.
- **Also considered:** Redis with the `redis_rate` library (a standard tool, but it adds a service and the library's last release is v10.0.1 from April 2023), a local limiter in each worker with the limit divided by the number of workers (breaks silently if the worker count changes without updating the setting), and one sending worker per channel chosen with an advisory lock (adding workers would add standby, not throughput).
- **What it costs:** Every claim also updates its channel's limiter row, in the database that also holds the queue, and claims on one channel are taken one at a time. If the shared row turns out to be the bottleneck, the limiter sits behind an interface, so Redis can replace it.
- **Still open:** How many HTTP calls are kept in flight. I set it by measurement in the worker slice: at an average provider latency of 200 ms, 100 requests per second need about 20 calls in flight per channel, summed across all workers.
- **How I will prove it (R8, N2):** Permits spaced 10 ms apart do not by themselves prove that the real HTTP sends are spaced the same way. So I measure the actual send start times per channel in a sliding 1-second window, with two or more workers, a full queue and retry traffic, and check that no window holds more than 100.

### K6 — Process layout: the API and the worker run as separate processes from one Go module

I chose to run the API and the worker as separate processes because I want to add workers without adding API instances: `docker compose up --scale worker=2`.

- **How it works:** One Go module with two entry points (`cmd/api` and `cmd/worker`); the business logic under `internal/` is shared. One Docker image holds both binaries.
- **Startup order:** PostgreSQL is healthy → the migration step finishes successfully → the API and the worker start. In Compose this is `depends_on` with `condition: service_healthy` and `condition: service_completed_successfully`. Compose on its own only waits until a container is running, not until it is ready.
- **Shutdown:** On `SIGTERM` the worker stops claiming new work and gives the calls already in flight a shutdown budget of 10 seconds, enough for the HTTP call and writing its result. Compose's `stop_grace_period` is set to 15 seconds, above that budget; by default Compose kills the container 10 seconds after `SIGTERM`. If a worker crashes instead, the lease recovery from K3 takes the work back.
- **Also considered:** One process running both the API and the workers (scaling it also scales the API, and two copies cannot bind the same host port) and one binary that picks its role from a flag (the same split, plus an all-in-one mode the case does not ask for).
- **What it costs:** Two entry points and a separate migration step in Compose.

### K7 — Retry policy: retry by error type, exponential backoff with jitter, five attempts in total

I chose to retry only temporary failures, a limited number of times and further apart each time, because the case asks for failed deliveries to be retried intelligently: a permanent error should stop at once, and a quota that waiting cannot reopen should not use up attempts.

- **Retried:** temporary network errors, timeouts, `408`, `500`, `502`, `503`, `504` and a temporary `429`. If the response carries a valid `Retry-After`, the next attempt is not sent before that time.
- **Not retried:** permanent errors such as `400` or `501`; the notification fails after one attempt.
- **webhook.site's quota:** webhook.site documents a `429` on a free URL only for a used-up request quota (and, on paid plans, for a URL blocked under fair use), and its response does not say which case it is. Waiting does not reopen the quota, so `429` handling depends on the provider profile, configured together with the provider URL (K14). With the free webhook.site profile the case uses, a `429` stops automatic retries at once. With the mock profile and any other provider profile, a `429` is temporary: it gets the bounded retries, and a valid `Retry-After` is respected.
- **Schedule:** Five attempts in total, the first one included, and a 5-second HTTP timeout. The n-th retry waits a random time between 0 and min(60 s, 2 s × 2^(n−1)) (full jitter). The 60-second cap belongs only to our own backoff: if a valid `Retry-After` asks for longer, the next attempt waits until that time. All values come from environment variables. The retry time is written to `available_at` in PostgreSQL, so a waiting retry holds no worker and survives a crash, and every new POST goes through the K5 limiter. The lease from K3 covers the timeout and writing the result.
- **How my decision changed:** The first version retried every `429` the same way, so a used-up webhook.site quota would have spent all five attempts; I changed it so the provider profile decides. I also revised the list of retried status codes during review: I left out `408` and `500` at first and then added them back, because the HTTP standard allows a `408` request to be repeated and a `500` can be temporary.
- **What `failed` means:** When the attempts run out, the notification is marked `failed` with its last error. That does not prove it was never delivered: an earlier attempt may have timed out after the provider had already accepted it (K3).
- **Also considered:** A fixed interval without classifying errors (simple, but it retries permanent errors and sends retries in waves), a per-channel circuit breaker now (it belongs to the Failure Handling bonus, B1, which comes after the core), and pausing a whole channel when a `429` carries `Retry-After` (also left for B1; all three channels share one provider URL, so the scope of the provider's limit has to be checked first).
- **How I will prove it:** With the mock provider: `503` then `202` ends as sent; `400` stops after one attempt; a `Retry-After` is respected; the attempt limit is reached and the notification ends as `failed`; the same `429` response is handled differently under the webhook.site and mock profiles.

## Processing rules

### K8 — Priority: strict order within each channel

I chose strict priority within each channel because the case asks for a priority queue with three levels, and a worker that always takes the highest ready level first is a rule I can state and test directly. Each channel has its own rate limit, so a low-priority SMS never uses capacity that a high-priority email needs; priority only has to decide the order inside a channel.

- **How it works:** Within one channel, a worker claims from the notifications that are ready (their `available_at` has passed) and not held by another worker, in the order `high → normal → low`. Within a level, the earliest `available_at` goes first, and the notification ID breaks ties. This is the ordering of the K5 claim query (`ORDER BY priority, available_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`), backed by a partial index on `(channel, priority, available_at, id)` for queued rows. I check with `EXPLAIN` that the index serves this order.
- **What it guarantees:** The order in which workers take work. Calls to the provider run in parallel, so the order in which they reach the provider or finish can differ.
- **Also considered:** Aging, where a notification that has waited long enough moves up a level (no starvation, but a `low` can then go before a fresh `high`, and I have no data for the threshold), and weighted shares such as 6:3:1 (no starvation, but a `low` is sent while a `high` waits, and a test can only check ratios, not order).
- **What it costs:** `low` can starve: it waits whenever higher-priority work fills the channel's actual capacity, and that capacity can be below 100 per second, for example when provider latency limits how many sends finish. The metrics show queue depth and the age of the oldest waiting notification per priority, so starvation is visible. If it shows up, aging can be added as a periodic update without changing the index.
- **How I will prove it (R9):** On one channel, I queue 50 `low` and then 50 `high` notifications, run one worker with one call in flight, and check that every `high` is claimed before any `low`; within a level, claims follow `available_at`.

### K9 — Status model and cancellation: five statuses, and "pending" means `queued`

I chose five statuses and defined "pending" as `queued` because a notification that no worker holds right now (waiting for its first attempt, for its channel's limit or for its retry time) can be stopped safely, while one whose POST may already be on its way cannot.

- **Statuses:** `queued` → `processing` → `sent` or `failed`, and `queued` → `cancelled`. A temporary failure puts the notification back to `queued` with a later `available_at`. `sent` means the provider accepted the request with `202`; `failed` is defined in K7. The API also returns `attempts` and `last_error`, so a notification waiting for a retry is visible without a separate status.
- **Attempts:** The attempt counter goes up in the same transaction as the claim, not when the result is written, so an attempt counts even if the worker crashes right after its POST. A notification is claimed only while it has attempts left: if the lease of a fifth attempt expires, recovery marks it `failed` and does not start a sixth POST.
- **Cancel:** `POST /notifications/{id}/cancel` is one conditional update, `… SET status = 'cancelled' WHERE id = $1 AND status = 'queued'`. If it changes the row, the answer is `200` with the notification. If not: `404` when the notification does not exist, `200` when it is already cancelled, and `409 Conflict` with the current status when it is `processing`, `sent` or `failed` (RFC 9110 defines `409` as a conflict with the resource's current state).
- **Cancel racing a claim:** Whichever locks the row first wins. If the claim holds the lock, the cancel waits, then re-checks `status = 'queued'` against the committed row (PostgreSQL's Read Committed behaviour) and returns `409`. If the cancel holds it, the claim skips the row.
- **No POST on an expired lease:** A worker sends its POST with a deadline that ends before its lease does, so a worker that has lost its lease does not start a POST for a notification that may since have been cancelled.
- **Not built:** Cancelling a whole batch. The case asks only for cancelling pending notifications.
- **Also considered:** A separate `retrying` status (clearer to read, but `attempts` and `last_error` carry the same information, and every query would have to handle two waiting statuses) and accepting a cancel while `processing` through a `cancel_requested` flag (it cannot stop the POST already in flight, and a temporary failure returns the notification to `queued`, where it can be cancelled anyway).
- **How my decision changed:** The card I was first offered increased the attempt counter only when a temporary failure was written. I reviewed it with OpenAI Codex and traced the crash case: a worker dying after its POST would leave that attempt uncounted, and recovering the lease of a fifth attempt could start a sixth POST. I moved the counter into the claim's transaction.
- **What it costs:** A crash after a claim uses up an attempt even if the provider never received the POST.
- **How I will prove it (R5):** The rule under test is that no new POST goes out after a successful cancel. Cancelling a queued notification returns `200`, and no POST follows; cancelling a `processing`, `sent` or `failed` one returns `409`; a second cancel returns `200`; an unknown ID returns `404`. A race test runs cancel and claim at the same moment many times: every run ends either with a successful cancel and no new POST, or with a claim and a `409`, never both. A crash test stops a worker after its POST and checks that the attempt was counted.

### K10 — Idempotency key: an optional `Idempotency-Key` header, one key per create request

I chose an optional key sent by the client because only the client knows whether two identical requests are a retry or two notifications it really wants; two calls without a key are two separate requests.

- **How it works:** A create request may carry an `Idempotency-Key` header. One key covers one request, so a batch has a single key. The key, a hash of the validated request and the first response are written in the same transaction as the notification or batch they create, in an `idempotency_keys` table whose primary key is the key.
- **Repeats:** The same key with the same content creates nothing new; the API returns the first create response, with the same notification or batch ID, and the current status comes from `GET`. The same key with different content returns `422`, as the IETF `Idempotency-Key` draft (version 08, not yet an RFC) recommends.
- **Concurrent repeats:** The key is written with `INSERT … ON CONFLICT (key) DO NOTHING RETURNING`. If a second request arrives while the first one's transaction is still open, PostgreSQL makes its insert wait for that transaction. When `RETURNING` comes back empty, a separate `SELECT` reads the stored response; under Read Committed that new statement sees the first transaction's committed row, so both calls get the same answer once the short first transaction ends.
- **Retention and scope:** A key is kept as long as the record it created. There is no authentication (A1), so keys are global. The README states both, and that a production system would expire keys after a documented time and scope them per client.
- **Also considered:** A required key (`400` without it; R11 would then hold for every request, but a reviewer's first call without the header fails, and the case does not ask for it) and deduplication by content hash within a time window (needs nothing from the client, but blocks legitimate repeats, and I have no data for the window).
- **How my decision changed:** The card I was first offered answered a repeat with the notification's current state and did not say how a concurrent repeat reads the first request's result. I reviewed it with OpenAI Codex and researched PostgreSQL's Read Committed rules and the IETF draft. A repeat now returns the first create response while `GET` shows the current state, and an empty `RETURNING` is followed by a separate `SELECT`.
- **What it costs:** A client that sends no key gets no protection against its own retries. Resending only part of a batch creates new notifications, because keys are per request, not per item. The table keeps one response body per key, up to the size of a 1000-item batch response.
- **How I will prove it (R11, the first test in K3):** The same key with the same content returns the same ID and leaves one row; the same key with different content returns `422`; two calls without a key create two notifications; concurrent calls with the same key create one notification; a repeated batch returns the same batch ID without new rows.

### K11 — Validation: one set of rules at the API, limits counted in Unicode code points

I chose to validate every request at the API, with the same rules for single and batch requests, because invalid content should be rejected before it is queued and before it can use a send permit.

- **How it works:** The handler decodes the JSON first, and a malformed body gets `400`. It then runs one validation function shared by single and batch creation. A rule violation gets `422` with a list of field errors, and nothing is saved. The worker trusts saved data.
- **Required fields:** `recipient`, `channel` (`sms`, `email` or `push`) and `content`. `priority` is optional and defaults to `normal`. The K10 request hash is computed after this default is applied, so leaving the field out and sending `normal` count as the same request.
- **Content limits:** Counted in Unicode code points (`utf8.RuneCountInString` in Go; `len` counts bytes): `sms` 160, `push` 1,000 and `email` 10,000, each configurable. These are the application's documented limits, not carrier or platform guarantees. 160 code points fit one SMS segment only when every character is in the GSM-7 set; with other characters a segment holds 70. 1,000 code points do not guarantee that a real push payload stays within the 4 KB that APNs and FCM allow, because JSON escaping and the rest of the payload add bytes. The provider in this case is webhook.site, which sends neither.
- **Recipient format:** `sms` must be an E.164 number (`+` and at most 15 digits, which matches the case's example `+905551234567`). `email` must parse with Go's `net/mail.ParseAddress` and equal the parsed address, so the display-name form is rejected. `push` has no universal token format, so it must be non-empty, contain no whitespace and be at most 512 characters.
- **Also considered:** Counting SMS segments by encoding (GSM-7 or UCS-2) and push content in bytes (closest to real billing, but it needs the GSM-7 character table, and the case asks for character limits) and checking the channel rules in the worker just before sending (the API stays thinner, but an invalid notification is accepted, fails later and uses up a send permit, because K5 takes the permit together with the claim).
- **How my decision changed:** The card I was first offered presented 160 as one SMS segment and 1,000 characters as fitting the 4 KB push payload. I reviewed it with OpenAI Codex and researched the SMS encodings and the APNs and FCM payload limits. Neither holds in general, so the numbers are now documented as application limits. I also set the idempotency hash to be computed after the `priority` default.
- **What it costs:** A 160-character SMS with Turkish characters would take three segments at a real carrier, because the limit does not count segments. The README lists this.
- **How I will prove it (R10):** Table-driven tests on the validation function, with no database: each channel's limit at the edge (for `sms`, 160 passes and 161 fails), a message with Turkish characters counted in code points, an invalid E.164 number, an email with a display name, an empty field, an unknown channel, and a missing `priority` saved as `normal`.

### K12 — Batches: all or nothing, with mixed channels and priorities

I chose all-or-nothing batches because a client can then fix the items the error lists and resend the same batch, and one idempotency key always stands for one fully saved batch.

- **How it works:** `POST /notifications/batch` takes `{"notifications": [ … ]}` with 1 to 1,000 items, and each item has its own channel and priority. Every item goes through the K11 validation, and all errors are collected. If any item is invalid, the answer is `422` with each error's item index and field, and nothing is saved. Otherwise one transaction writes the batch row, all its notifications and the K10 key with the first `202` response, which holds the batch ID and the notification IDs in request order.
- **Limits:** Two separate published limits: the number of items (1 to 1,000, from the case) and the request body size (16 MiB by default, configurable, enforced with Go's `http.MaxBytesReader` and answered with `413`). The body limit is not derived from the content limits: in escaped JSON a single emoji can take 12 bytes (`😀`), so a worst-case figure would be far too large. As a result, a batch of large items can get `413` with fewer than 1,000 items.
- **Batch status (R4):** `GET /batches/{id}` returns the counts per status for the whole batch and a paginated list of its notifications, with the K13 pagination.
- **Also considered:** Saving the valid items and listing the rejected ones (friendlier at first, but resending the whole batch would create the accepted items again, because a key covers a request, not an item) and one channel and priority per batch (simpler validation, but a campaign on several channels must be split into several requests, and the batch item schema would differ from the single-notification schema).
- **How my decision changed:** The card I was first offered derived the body limit from the content limits, at about 41 MB. I reviewed it with OpenAI Codex and researched JSON escaping: one emoji can take 12 bytes, so the estimate did not hold. I replaced it with a separate body limit of 16 MiB.
- **What it costs:** One invalid item stops the whole batch. Some large batches are refused with `413` before they reach 1,000 items.
- **How I will prove it (R2, R4):** Three separate tests: 1,000 valid items are saved while 0 or 1,001 are refused; a batch with one invalid item returns `422` with that item's index and writes no rows; a body over the limit returns `413`. A further test checks the R4 counts for a batch with mixed statuses.

### K13 — Listing: filter dates on `created_at`, paginate with a `(created_at, id)` cursor

I chose `created_at` with a cursor because every notification has a creation time that never changes, and a cursor keeps new notifications from shifting the pages.

- **How it works:** `GET /notifications` takes optional `status`, `channel`, `from` and `to` filters, combined with AND, and a `limit` (50 by default, at most 200). The date range is half-open: `from` is included and `to` is not, and both are RFC 3339 timestamps. Results come newest first, ordered by `created_at DESC, id DESC`. The response carries a `next_cursor` built from the last returned row's `created_at` and `id` at full timestamp precision, and the next page reads the rows below it: `WHERE (created_at, id) < (cursor values)`.
- **What it guarantees:** New notifications do not shift the pages, as they would with offset pagination. The pages are not one snapshot, though: each page is a separate query that sees the data committed when it starts. A notification whose `status` changes between two pages can move into or out of a filtered result, and a notification whose transaction commits after a page has already passed its `created_at` does not appear on later pages.
- **Indexes:** `(created_at DESC, id DESC)`. Filtered queries are checked with `EXPLAIN`, and `(channel, status, created_at, id)` is added only if they need it.
- **Also considered:** Offset pagination with `page` and `page_size` (familiar and able to return a total, but new notifications shift the pages, and deep offsets still read the rows they skip) and letting the client choose the date field from `created_at`, `sent_at` and `updated_at` (flexible for reports, but `sent_at` stays empty until a notification is sent, so filtering on it silently hides the rest, and each field needs its own index).
- **What it costs:** A client cannot jump to a given page; it can only move forward. There is no total count unless I add one as a separate query.
- **How I will prove it (R6):** Each filter alone and combined; the range edges (`from` included, `to` excluded); with new notifications inserted between page reads, every notification that existed before the first page appears exactly once; a malformed cursor returns `400`.

### K14 — Provider: a small Go mock by default, webhook.site as a separate profile

I chose a small Go mock as the Compose default because the project then starts with one command and no URL setup, and load tests can run without webhook.site's request quota and seven-day lifetime.

- **How it works:** The worker reads two settings: `PROVIDER_URL`, and `PROVIDER_PROFILE` (`standard` or `webhook_site_free`), which decides how a `429` is handled (K7). All channels use the one URL (A2). Compose starts a fourth service, `mockprovider` (`cmd/mockprovider`, in the same module and image), and points the worker at it with the `standard` profile.
- **The mock:** It answers `POST /` with `202` and `{messageId, status: "accepted", timestamp}` in the case's shape, with a new UUID each time, after a configurable delay; a malformed body gets `400`. It records when each request arrives, per channel, and `GET /stats` reports the counts and the highest number of arrivals in any sliding one-second window.
- **Two measurements, reported separately:** The mock measures when requests arrive at the provider; K5 measures when the worker starts each send. The R8 report shows both, with retries counted.
- **Deterministic scenarios:** `503` then `202`, a delayed response (the third R11 test in K3) and a `429` under each profile are built inside the tests with Go's `net/http/httptest`, not with a random failure rate in the mock.
- **webhook.site integration check:** To prove the integration the case asks for, I send a synthetic notification through the same worker to a webhook.site URL I created, with the `webhook_site_free` profile, and check the `POST` body webhook.site received, its `202` response and the final status in the API. The README gives the setup steps and the result of this check. Load tests run only against the mock, because a free URL's quota and lifetime are limited.
- **Also considered:** Sending straight to webhook.site from Compose (closest to the case's wording, but every reviewer would first have to create and configure a URL, and the 100-request quota rules out load tests) and WireMock with JSON mappings (it can return delays, scripted scenarios and random UUIDs, but it brings a Java-based image and its own mapping language for something a small service in the same module can do).
- **How my decision changed:** The card I was first offered presented the mock's `/stats` as the proof of R8 and drove failure scenarios through a random failure rate in the mock. I reviewed it with OpenAI Codex and researched the K5 measurement plan and Go's `httptest`. The mock measures arrivals, not send starts, so the two are now reported separately; the failure scenarios are deterministic tests; and I added the check against my own webhook.site URL.
- **What it costs:** A third binary and a fourth Compose service. The mock behaves like no real provider (its latency is a setting), so the webhook.site check stays a manual step.
- **How I will prove it (R13, R8):** R13 with the webhook.site check above; R8 with a load test against the mock that reads both the worker's send starts and the mock's arrivals.

### K15 — Provider response: store `messageId` and `timestamp`

I chose to store the provider's `messageId` and `timestamp` because they let me follow a notification into the provider's own records.

- **How it works:** Two nullable columns, `provider_message_id` and `provider_timestamp`, are written in the same `lease_token`-conditioned update that sets `sent` (K3, K9). `sent_at` is the time we recorded the acceptance; `provider_timestamp` is the time the provider reported. The notification detail shows both.
- **Not unique:** webhook.site's static response can give the same ID to many notifications, so the column has no `UNIQUE` constraint and is never used for deduplication.
- **A `202` with a broken body:** The notification is still `sent`, because the provider accepted the request (`202 Accepted`, RFC 9110) and a second POST could send it twice. The invalid field stays empty, and a warning is logged with the correlation ID.
- **Also considered:** Not storing the response (simplest, but nothing would link a notification to the provider's records) and a table with one row per attempt (the richest history and useful for B1, but one more write per attempt and more than the case asks for).
- **What it costs:** Two columns and decoding the `202` body.
- **How I will prove it (R13):** A `202` with a `messageId` fills both columns; a `202` with a broken body results in exactly one POST, the status `sent` and empty metadata; two notifications can store the same `messageId` without an error.

## Tooling

### K16 — HTTP router: the standard library's `net/http`

I chose the standard library's `net/http` router because its `ServeMux` covers every method and path pattern this API needs, so the project needs no router dependency.

- **How it works:** Routes use Go 1.22 patterns such as `GET /notifications/{id}`, and handlers read parameters with `r.PathValue("id")`. Correlation IDs (R15), request logging and panic recovery are ordinary `func(http.Handler) http.Handler` middleware around the mux. The `http.Server` sets its timeouts and shuts down as described in K6.
- **Also considered:** chi (compatible with `http.Handler` and ships ready middleware, but since Go 1.22 the standard router covers what this API needs) and Gin (popular, but its handlers use its own `*gin.Context`, which ties the middleware, the tests and the OpenAPI layer to Gin).
- **What it costs:** I write the three or four middleware functions myself.
- **How I will prove it:** Tests send requests through the real router instead of calling handlers directly: path parameters reach the handler, an unknown path returns `404`, and a known path with the wrong method returns `405` with an `Allow` header.

### K17 — Database access: pgx with plain SQL in the repository

I chose pgx with plain SQL in the repository because the correctness of the claim, the limiter, cancellation and idempotency depends on SQL conditions and transaction boundaries, and I want to see those queries directly.

- **How it works:** `pgxpool` is the connection pool. Every query is a SQL constant in the repository layer, run with numbered parameters (`$1`, `$2`, …); values are never concatenated into SQL. Transactions use `BeginTx`, `Commit` and a deferred `Rollback`.
- **Batches (K12):** The first version writes a batch with one parameterized multi-row `INSERT`. The protocol counts a statement's parameters in a 16-bit field, and 1,000 items need about 6,000 parameters, well below that limit. If `CopyFrom` is used later, it is called on the transaction (`tx.CopyFrom`), not on the pool, so the batch row, its notifications and the K10 key with the first response stay in one transaction. The `202` is returned only after the commit.
- **Also considered:** sqlc (the SQL stays mine and the scanning code is generated, but it adds a code generation step, and optional filters such as K13's are awkward) and GORM (closest to my SQLAlchemy experience, but the queue, limiter and idempotency queries would mostly end up as raw SQL anyway).
- **What it costs:** I write the row scanning myself, and each query needs its own test so that a column order mistake is caught.
- **How I will prove it:** Repository tests against a real PostgreSQL (K22), one per query, and a test that a batch whose insert fails leaves no batch row, no notifications and no idempotency key behind.

### K18 — Migrations: goose with SQL files embedded in the binary

I chose goose with SQL files embedded in the binary because one migration file with its Up and Down sections works like an Alembic revision, and the application and the tests can run the same files.

- **How it works:** Each migration is one SQL file with `-- +goose Up` and `-- +goose Down` sections, embedded with `embed.FS`. A small `cmd/migrate` creates a provider with `goose.NewProvider` and runs `Up(ctx)`. The provider keeps no package-level state, so the tests can run the same embedded files against their own database. The `*sql.DB` it needs comes from pgx's `stdlib` package. In Compose, the one-off migration service runs `migrate up` from the same image, and the API and the worker start only after it completes successfully (K6).
- **When a migration fails:** goose runs each migration in its own transaction by default, so the failing file is rolled back while the files applied before it stay applied. The migration service exits with an error, and the API and the worker do not start.
- **Also considered:** golang-migrate (common, with an official Docker image, but two files per migration and a "dirty" state after a failure that needs a manual `force`) and Atlas (a declarative schema with generated migrations, but a new tool and workflow for a small project, and its default binaries are under a commercial agreement).
- **How my decision changed:** The card I was first offered set goose up with its global settings (`SetBaseFS`, `SetDialect`) and said that a failed migration would not leave the schema half-done. I reviewed it with OpenAI Codex and researched goose's Provider API and its transaction behaviour. I switched to `NewProvider`, which has no global state, and recorded the real failure behaviour: only the failing file is rolled back.
- **What it costs:** A third binary in the image. A failure in the middle of a run leaves the earlier migrations in place, and the next run continues from there.
- **How I will prove it (D5):** On a clean database the migrations build the schema; a second run changes nothing; and with a broken migration the migration service fails, and the API and the worker do not start.

### K19 — OpenAPI: generated from the code with Huma on the standard library router

I chose Huma because it generates the OpenAPI 3.1 document from the routes and Go types, the way FastAPI does from Pydantic models, so the document cannot drift from the code.

- **How it works:** Huma runs on the K16 `ServeMux` through its `humago` adapter. Operations are registered with Go input and output types, and errors use RFC 9457 problem details. The document is exported to `api/openapi.yaml` and committed, and a check fails if the committed file differs from the one the code generates.
- **Fitting the earlier decisions:** The create operations set `DefaultStatus: 202`. A repeated request (K10) gets the stored first response as `Body []byte`, which Huma writes without serializing it again; a raw byte body does not describe itself, so these operations declare their JSON response schema explicitly. The batch operation sets `MaxBodyBytes` to the K12 body limit (16 MiB) instead of Huma's default of 1 MiB. K11's channel rules run through Huma's `Resolver` interface.
- **Malformed JSON:** Huma v2.41.0 copies the raw request body into its error details when the JSON cannot be parsed. Our error responses remove that value, so a request body is never echoed back.
- **Also considered:** writing `openapi.yaml` by hand and generating code with oapi-codegen (a reviewable contract first, but about 300 lines of YAML to write and generated code to explain) and swag comments (the stable version produces Swagger 2.0, and the comments can drift from the code).
- **How my decision changed:** The card I was first offered left open whether Huma could return the stored K10 response unchanged, and it did not notice that Huma echoes a malformed body in its errors. I reviewed it with OpenAI Codex and researched Huma's response outputs and its v2.41.0 source. `Body []byte` bypasses serialization, so the K10 design holds; I added removing the echoed body and declaring the response schema explicitly.
- **What it costs:** The handlers are tied to Huma's signatures; the service and repository layers are not. Responses returned as raw bytes have to be described by hand in the document.
- **How I will prove it (D4, K10–K12):** Four tests through the real route: malformed JSON returns `400` (K11) without the body in the error; a field error returns `422`; a body over the limit returns `413`; and a repeat with the same key returns the first `202` body byte for byte. A separate check keeps `api/openapi.yaml` up to date.

## Observability

### K20 — Metrics: a JSON endpoint computed from PostgreSQL

I chose a JSON endpoint computed from PostgreSQL because the case asks for queue depth, success and failure rates and latency, and the state shared in PostgreSQL lets the endpoint cover every worker without its numbers resetting when a worker restarts.

- **How it works:** `GET /metrics` on the API is a Huma operation, so it appears in the OpenAPI document. It reads every value in one short `REPEATABLE READ READ ONLY` transaction: all its statements see the same snapshot, and `now()` returns the same time throughout, which the response reports as `as_of`. Results are given per channel.
- **Queue depth:** For each channel and priority, `ready` counts the `queued` notifications whose `available_at` is at or before `as_of`, and `deferred` counts the ones waiting for a later time. I named it `deferred` rather than "retrying" because a scheduled notification (B2) will wait the same way. The age of the oldest ready notification is `as_of − min(available_at)`; this is the starvation signal K8 relies on.
- **Success and failure:** A `completed_at` column is written when a notification becomes `sent` or `failed`; on `sent` it gets the same database time as `sent_at`. The endpoint counts the notifications completed in the last 60 seconds and reports them per second and as a success ratio, `sent / (sent + failed)`. Cancellations and retries that have not finished are left out, and the ratio is `null` when nothing completed. `sent` means the provider accepted the request (K9).
- **Latency:** For the notifications sent in the window, the p50, p95 and p99 of `created_at → sent_at`, with the sample count, computed with PostgreSQL's `percentile_cont`. This time includes queue waiting and retries. The percentiles are `null` when there are no samples. In the first version, the duration of each provider call goes to the logs (K21), not to this endpoint.
- **Window:** A fixed 60 seconds. I measure the queries' cost with a full queue (`EXPLAIN ANALYZE`) before changing anything.
- **Not the proof of R8:** This endpoint summarises completed notifications. The R8 test counts the actual HTTP send starts, retries included, in a sliding one-second window (K5).
- **Also considered:** The Prometheus text format with a Prometheus server in Compose (the industry standard, but the counters live in each process, so scaled workers need the server and replica discovery together, and the rates come from the server's queries, not from our endpoint), this JSON endpoint plus a Prometheus endpoint in every process (two sources that count different things, and without a server nobody collects the workers' numbers), and OpenTelemetry metrics (one SDK shared with the distributed tracing bonus, B5, but its Prometheus exporter is still v0.69.0 and the aggregation problem stays).
- **How my decision changed:** The card I was first offered counted waiting notifications as `waiting_for_retry`, added a `provider_latency_ms` column, took the window as a request parameter, and did not say how the values in one response stay consistent with each other. I reviewed it with OpenAI Codex and researched PostgreSQL's isolation levels and the scheduled notifications bonus. Waiting work is now `deferred`, the provider call duration goes to the logs, the window is fixed at 60 seconds, and every value comes from one snapshot.
- **What it costs:** No history and no Prometheus or Grafana integration; the endpoint shows the current state only. Provider call latency is only in the logs. Each request adds read queries to the database that also holds the queue. On `sent` rows `completed_at` repeats `sent_at`, kept so that one column and one index serve the completion window. When B2 adds scheduled notifications, latency should be measured from the scheduled time instead of `created_at`.
- **How I will prove it (R14):** Repository tests against a real PostgreSQL (K22) with fixed timestamps: ready and deferred counts per channel and priority, the age of the oldest ready notification, the success ratio (`null` when only cancellations exist), the percentiles (`null` without samples), and rows outside the 60-second window left out. A run with a full queue records the `EXPLAIN ANALYZE` time.

### K21 — Logging: `log/slog` JSON with a correlation ID stored on each notification

I chose `log/slog` with a correlation ID stored on each notification because it lets me follow a notification across the API and worker processes, and the standard library's logger is enough for this case.

- **Where the ID comes from:** A middleware (K16) reads the `X-Correlation-ID` header. If the value is valid (at most 64 characters from `A–Z a–z 0–9 - _ .`), the API uses it; otherwise it generates one, so a client cannot write arbitrary text into our logs. The response returns the ID in the same header.
- **How it travels:** At the start of a request, the API creates `logger.With("correlation_id", id)` and passes that logger to the code handling the request. The create transaction writes the ID into the notification row, so every notification in a batch carries the batch request's ID next to its own `notification_id`. At every claim, the worker reads the ID from the row, makes a logger with the ID, the `notification_id`, the channel and the attempt number, and adds the ID to the provider POST as an `X-Correlation-ID` header. webhook.site shows the headers it receives, so the K14 integration check can show the ID reaching the provider.
- **Why `With` and not the context:** `slog`'s built-in JSON handler ignores the context it is given, so `InfoContext` alone adds nothing. The first version passes the derived logger instead of adding a handler that reads the ID from the context.
- **Two lines per attempt:** The worker logs the send start before the POST and the result after it (status code, duration, outcome). A worker that crashes after its POST may never write the result line, so the start has its own line. The R8 measurement reads the start lines, and the provider call duration that K20 leaves out of the metrics is in the result lines.
- **Replays:** A repeated request with the same idempotency key (K10) does not change the correlation ID stored on the notification and starts no new provider call.
- **Not logged:** Message content and recipients. A notification is found by its ID.
- **Output:** JSON lines on stdout (`docker compose logs`), with the level set by `LOG_LEVEL`.
- **Also considered:** A custom `slog` handler that reads the ID from the context (automatic, but more code than the first version needs), zap or zerolog (faster loggers, but a dependency for a log volume of a few lines per send), and W3C trace context through OpenTelemetry now (the standard form of a correlation ID, but it would pull the distributed tracing bonus, B5, into the core).
- **How my decision changed:** The card I was first offered logged each attempt as one line and added a custom handler so that `InfoContext` would pick the ID up from the context. I reviewed it with OpenAI Codex, traced the K3 crash case and researched the `log/slog` documentation and source. A crash after the POST can leave no result line, so each attempt now has a start line and a result line. The built-in handlers ignore the context, so the ID would appear only through that extra handler; the first version passes a logger made with `With` instead. I also recorded that a K10 replay keeps the first ID.
- **What it costs:** Every function that logs must receive the derived logger; a line written with the base logger has no ID. A migration adds the `correlation_id` column. A client can choose its own ID, within the validation rules.
- **How I will prove it (R15):** An end-to-end test creates a notification with `X-Correlation-ID: test-123`, runs the worker against an `httptest` provider, and checks that the provider received the header and that the API and worker log lines carry the ID. Further tests: batch items share the ID with different `notification_id`s; a replay keeps the first ID and makes no new provider call; an invalid header gets a generated ID.

## Tests and code layout

### K22 — Test database: Testcontainers with the same PostgreSQL image as Compose

I chose Testcontainers because the tests then run against a real PostgreSQL, from the same image Compose uses.

- **How it works:** The PostgreSQL module of `testcontainers-go` (v0.45.0) starts a container from the Compose image once per integration test package, in `TestMain`. Each test gets its own database with the same embedded goose migrations applied (K18), and drops it with `t.Cleanup`. The container gets a random free host port and starts fresh on every run, so it neither collides with a local PostgreSQL nor reuses an old volume. Tests that need no database (validation, backoff, metric ratios) do not start one.
- **Race tests:** The claim, cancel and idempotency races run with committed data and separate connections; a transaction rolled back at the end of a test could not show them. Each order of a race is set up with controlled steps instead of `sleep`: for example, one transaction holds the row lock while the test starts the other. The K9 test that runs cancel and claim together many times stays as an additional check.
- **Template database later:** If applying the migrations to every test database turns out to be slow, I clone each test database from a migrated template instead (`CREATE DATABASE … TEMPLATE`).
- **One command (D6):** `go test -count=1 ./...`, with Docker running. `-count=1` keeps `go test` from reporting a cached result: its cache checks only files and environment variables, not the database a test used.
- **Also considered:** A separate test database in Compose with `make test` (familiar, but the single command becomes `make test`, a plain `go test` fails while the service is down, and a fixed host port brings back port collisions) and `embedded-postgres` (no Docker, but this project needs Docker anyway, and its PostgreSQL build comes from the library instead of the Compose image).
- **How my decision changed:** The card I was first offered cloned every test database from a template from the start and left `go test`'s result cache unhandled. I reviewed it with OpenAI Codex and researched the Testcontainers PostgreSQL module and `go test`'s caching rules. I start with migrations per test database and add the template only if measurement calls for it, order race tests with controlled steps instead of `sleep`, and run the suite with `-count=1`.
- **What it costs:** `go test` needs Docker running. Each integration package spends a few seconds starting its container, and each test spends the time it takes to apply the migrations.
- **How I will prove it (D6):** On a clean clone with Docker running, `go test -count=1 ./...` passes; a second run executes the tests again instead of printing `(cached)`; and a row written in one test is not visible in another.

### K23 — Project layout: small packages by layer, with multi-table transactions inside `postgres`

I chose a small set of packages split by layer, with every multi-table transaction closed inside the `postgres` package, because a small structure is enough for this case and keeps each transaction in one place.

```
cmd/{api,worker,migrate,mockprovider}/  entry points; they build and connect the parts
internal/
  domain/    notification types and the K11 validation (no database)
  httpapi/   Huma operations and middleware (K16, K19, K21)
  service/   create, batch, cancel, list and metrics rules
  postgres/  all SQL and transactions (K17)
  worker/    claim loop, sending, retry policy (K3, K5, K7)
  provider/  provider HTTP client and profiles (K14)
migrations/  SQL files and the embed.go that embeds them (K18)
api/openapi.yaml
```

- **How the parts connect:** `service` and `worker` each define the small interfaces they need, and `postgres` implements them without importing either package; the entry points in `cmd/` connect them. The imports are therefore: `httpapi` → `service`; `service`, `worker` and `postgres` → `domain`; `worker` → `provider`; `cmd/*` → all of them. Go's official module layout guide recommends `cmd/` for several programs and `internal/` for server code, and sets no rule for the inside of `internal/`.
- **Transactions:** The batch row with its notifications and the idempotency key (K10, K12), and the rate-limit permit with the claim (K5), are each one transaction inside `postgres`, so no transaction has to cross packages.
- **Opened when needed:** Packages such as `config`, `logging`, `mockprovider` and the test helper (K22) are created by the slice that first needs them, not up front.
- **`migrations/` has its own `embed.go`:** `//go:embed` patterns cannot contain `..`, so the code that embeds the SQL files sits next to them.
- **Also considered:** Packages by feature, such as `notification`, `batch`, `delivery` and `idempotency` (common in Go, but the batch and claim transactions cross those boundaries and would need a shared transaction mechanism), and the `golang-standards/project-layout` template (well known, but its own README says it is not an official standard of the Go team; `pkg/` signals a public library this project does not offer, and most of its directories would stay empty).
- **How my decision changed:** The card I was first offered drew `service → postgres` as a dependency, which reads as an import. I reviewed it with OpenAI Codex and the Go module layout guide: with interfaces defined where they are used, `service` does not import `postgres`, and the entry points connect the two.
- **What it costs:** The entry points in `cmd/` wire every implementation by hand. Moving packages after several slices means changing import paths. `service` may grow; it is then split into files, not into packages.
- **How I will prove it:** `go build ./...` and `go vet ./...` pass, and `go list -deps` for `internal/service` and `internal/worker` does not include `internal/postgres`.

## Real-time updates, scheduling and tracing

### K24 — Real-time status events (B4): PostgreSQL `LISTEN/NOTIFY`

I chose PostgreSQL `LISTEN/NOTIFY` because a status change is published only when its transaction commits, so I need neither a separate broker nor a second write step to the API.

- **How it works:** A trigger added by a migration (K18) calls `pg_notify('notification_status', …)` whenever a notification's `status` changes, with the notification and batch IDs, the status, `attempts` and a `version`. The trigger catches every path that changes a status: the claim, the result, a cancel and lease recovery. PostgreSQL delivers a notification only if its transaction commits, in commit order, and the payload stays far below its 8000-byte limit.
- **Ordering with `version`:** Every status update increments a `version` column. Row locks serialize the updates to one row, so `version` always grows. A timestamp from `now()` does not guarantee that: a transaction can start earlier, wait for the row lock and write an older `updated_at`.
- **Listener:** The API keeps one connection out of its pool (pgxpool `Hijack`) that runs `LISTEN`, waits in `WaitForNotification` and passes each event to the subscribers of that notification or batch.
- **A snapshot without gaps:** A new subscriber is registered first and its events are buffered; then the current state is read from the database and sent; then only events with a higher `version` than the snapshot are passed on. This follows the order the `LISTEN` documentation gives: listen first, then read the state.
- **When the listener drops:** PostgreSQL does not replay notifications sent while a session was not listening. If the listener connection drops, the API closes the affected WebSockets, and a client that reconnects gets a fresh snapshot.
- **Built:** after scheduled notifications (K26), together with K25.
- **Also considered:** Polling the database for subscribed notifications (simple, but the delay equals the polling interval, and a "changed since" query on `updated_at` can skip a change that commits late with an older timestamp) and a separate channel such as Redis pub/sub or HTTP from the worker to the API (a new service or a new coupling, and the status write and the event become two steps that can disagree).
- **How my decision changed:** The card I was first offered said that a connected client would miss no events, compared events by `updated_at`, and left open the order of subscribing and reading the snapshot. I reviewed it with OpenAI Codex and researched PostgreSQL's `LISTEN` and `NOTIFY` documentation: notifications are not replayed, and `now()` timestamps are not guaranteed to grow per row. I removed the claim, added the `version` column and fixed the order: subscribe and buffer, read the snapshot, pass newer events.
- **What it costs:** Delivery to a WebSocket is best effort. A slow client is disconnected (K25), events held in the API's memory are lost if it crashes, and a reconnect brings the current state, not the missed events. If PostgreSQL's notification queue fills up, the commits that call `NOTIFY` fail; the documentation puts the queue at 8 GB and notes that a long transaction in a listening session blocks its cleanup, so the listener never opens a transaction. The API holds one more database connection.
- **How I will prove it (B4):** Tests against a real PostgreSQL (K22): a committed status change reaches the listener and a rolled-back one does not; an update committed while a snapshot is being read is not lost; events older than the snapshot are dropped by `version`; when the listener connection is cut, the affected WebSockets are closed.

### K25 — WebSocket endpoint (B4): `coder/websocket`, one notification or batch per connection

I chose `coder/websocket` because it integrates with `net/http` and supports context-bound writes and control-frame handling for server-pushed updates.

- **Endpoints:** `GET /ws/notifications/{id}` and `GET /ws/batches/{id}` are plain handlers on the K16 `ServeMux`, not Huma operations. OpenAPI 3.1 does not describe WebSocket APIs, so the README documents the messages. An unknown ID gets `404` before the upgrade.
- **Messages:** Only the server sends. The first message is the current state (one notification, or every notification in the batch); each later message is an update: `{notification_id, batch_id, status, attempts, version, updated_at}`.
- **One writer per connection:** Each connection has one writer with a bounded queue and a time limit on every write. If a slow client's queue fills, its connection is closed, so other subscribers never wait for it.
- **Control frames:** The client sends no application messages, but the connection is still read with `CloseRead`, which answers ping, pong and close frames; the library handles control frames only while a connection is being read.
- **Shutdown:** `http.Server.Shutdown` neither closes nor waits for hijacked connections such as WebSockets. A function registered with `RegisterOnShutdown` starts closing them with a "going away" status, and the API waits for them with its own time limit inside the K6 shutdown budget.
- **Also considered:** `gorilla/websocket` (the most widely used, but its last release was in June 2024, it has no context support, and only one goroutine may write to a connection at a time) and `golang.org/x/net/websocket` (its own documentation points to more actively maintained packages).
- **How my decision changed:** The card I was first offered never read the connection, so control frames would not have been handled, and it did not say that `Shutdown` leaves WebSockets open. I reviewed it with OpenAI Codex and researched the `coder/websocket` and `net/http` documentation and source. I added `CloseRead`, one writer with a write time limit, and closing the connections through `RegisterOnShutdown` with a separate wait.
- **What it costs:** A client that follows several notifications opens several connections. The goroutines of each connection must end when it closes; a mistake there leaks them.
- **How I will prove it (B4):** With `httptest.Server` and the library's client: connect, receive the snapshot, change a status, receive the update; a client that stops reading is disconnected without delaying another subscriber; an unknown ID gets `404`; on shutdown, open connections receive a close frame; no goroutines remain after the tests.

### K26 — Scheduled notifications (B2): an optional absolute `scheduled_at`

I chose an optional absolute `scheduled_at` because scheduling fits the existing queue directly: it only sets a notification's first `available_at`.

- **How it works:** Single and batch requests accept an optional `scheduled_at`, an RFC 3339 time with the offset RFC 3339 requires, normalized to UTC. It is stored in its own column and sets the first `available_at`. A retry changes only `available_at`, so the planned time is kept. Cancellation (K9), priority (K8) and the rate limit (K5) work as for any queued notification, and K20 counts scheduled notifications that are still waiting as `deferred`.
- **Validation order:** A new request with a time in the past gets `422`. The idempotency key (K10) is checked first, so a repeat with the same key and content returns the first response even after the planned time has passed. The order is: field validation, the request hash, the key, and only for a new key the time rule.
- **No upper limit:** The case sets no scheduling horizon, so the first version has none.
- **Latency (K20):** Once scheduled notifications exist, the metrics show two distributions, each with its sample count: `created_at → sent_at` for notifications sent immediately and `scheduled_at → sent_at` for scheduled ones. A deliberately planned wait does not enter the p99 of immediate notifications.
- **What it guarantees:** A notification is not sent before its planned time. After that, it waits for its channel's capacity and its priority like any other.
- **Built:** before the WebSocket updates (K24, K25).
- **Also considered:** A relative `delay_seconds` (no time zones, but campaigns are planned at absolute times, and the delay depends on when the request arrives) and a separate table with a scheduler process that moves due notifications into the queue (a third long-running process and a new failure mode, while the queue already has `available_at`).
- **How my decision changed:** The card I was first offered accepted times up to 60 seconds in the past, capped scheduling at 30 days, ran the time rule before the idempotency key and measured all latency from `coalesce(scheduled_at, created_at)`. I reviewed it with OpenAI Codex and traced the K10 flow: a repeat arriving after the planned time would have been rejected with `422`, so the key is now checked first. Past times are rejected outright, the cap is gone, and latency is split into two distributions.
- **What it costs:** A client whose clock runs behind can get `422` for a time it thinks is in the future; a client that wants immediate sending leaves the field out. Without an upper limit, a mistyped year is accepted and waits as a deferred notification until it is cancelled.
- **How I will prove it (B2):** A notification scheduled a few seconds ahead is not claimed before its time and is claimed after it; a cancel before that time leaves no POST; a past time on a new request gets `422`; a repeat with the same key and content after the planned time returns the first response and creates nothing; the same key with a different `scheduled_at` gets `422`; batch items with different times are claimed in time order.

### K27 — Distributed tracing (B5): OpenTelemetry over OTLP/HTTP, linked traces, Jaeger as an optional profile

I chose OpenTelemetry with an OTLP/HTTP exporter and an optional Jaeger profile, limited at first to the API request, the worker attempt and the provider call, because those three are enough to follow a notification across the processes in the first version.

- **Spans:** otelhttp (v0.72.0) creates a span for each API request and for each provider call. The create transaction stores the request's W3C trace context in a `traceparent` column. Each worker attempt starts its own span with a link to that stored context, and the provider call is a child of the attempt's span, so the `traceparent` header reaches the provider.
- **Why a link:** OpenTelemetry's messaging conventions use links as the default connection between a producer and a consumer. As a child, a batch of 1,000 notifications, with every retry, would hang under one request's trace. The result is linked traces, not one trace.
- **Switched on by configuration:** Tracing is off unless an OTLP endpoint is set (`OTEL_EXPORTER_OTLP_ENDPOINT`, which the exporter reads). Jaeger (`jaegertracing/jaeger:2.23.0`, in-memory storage, UI on port 16686) runs only with `docker compose --profile tracing up`, and the README shows how to point the services at it.
- **Not in the first version:** Database spans (for example with otelpgx).
- **Built:** after the core flow and its tests pass.
- **Also considered:** The stdout exporter without a UI (no extra service, but the steps cannot be seen on a timeline) and correlation IDs with step durations in the logs only (no new dependency, but that is not distributed tracing).
- **How my decision changed:** The card I was first offered promised a single Jaeger trace from the API through the worker to the provider while linking the worker's span instead of making it a child, ran Jaeger as a permanent Compose service and included database spans. I reviewed it with OpenAI Codex and researched OpenTelemetry's messaging conventions and the Jaeger setup: a link connects separate traces. I narrowed the first version to three spans, made Jaeger an optional profile and added a test that the flow completes while Jaeger is down.
- **What it costs:** A `traceparent` column, and otelhttp is a pre-1.0 contrib module whose version is pinned. Following one notification means opening the request's trace and then the linked attempt traces.
- **How I will prove it (B5):** With the SDK's in-memory exporter: an API request produces a span; a worker attempt's span links to the stored context; the request the provider receives (`httptest`) carries a `traceparent` from the attempt's trace. With an OTLP endpoint set and Jaeger stopped, a notification still reaches `sent`. The README shows the linked traces in Jaeger.

## Lint and CI

### K28 — Linter: golangci-lint's standard set plus `bodyclose` and `noctx`

I chose golangci-lint's `standard` set plus `bodyclose` and `noctx` because both extra linters catch problems this project can actually have: a provider response body left open, and HTTP calls not bounded by a context.

- **How it works:** `.golangci.yml` has `version: "2"` and `linters.default: standard` (errcheck, govet, ineffassign, staticcheck and unused), enables `bodyclose` and `noctx`, and uses `goimports` as the formatter; goimports also applies gofmt's formatting. `golangci-lint run` reports lint and formatting problems without changing files, and `golangci-lint fmt` formats the code. The same version, v2.15.0, is pinned locally and in CI.
- **Built:** with the first working slice. Every slice gate runs `golangci-lint run`.
- **Also considered:** `go vet`, `staticcheck` and `gofmt -l` as three separate commands (staticcheck is already in the standard set, and errcheck, which catches ignored errors, would be missing) and only `go vet` with `gofmt` (too thin for a linting pipeline).
- **What it costs:** One more tool, pinned to the same version on my machine and in CI. A false positive is silenced on its own line with a `//nolint:<linter>` comment that gives the reason.
- **How I will prove it:** `golangci-lint run` passes at every slice gate and in CI.

### K29 — CI (B6): a GitHub Actions pipeline for lint, tests, the OpenAPI check and the Docker build

I chose a CI pipeline without image publishing because the case describes this bonus as an automated testing and linting pipeline, and publishing an image to GHCR would be an extra delivery step.

- **How it works:** On every pull request and every push to `main`, one workflow runs golangci-lint (K28, through its v9 action after `actions/setup-go`), `go test -count=1 -race ./...`, the OpenAPI check (K19: regenerate the document and fail if `api/openapi.yaml` differs) and `docker build`. GitHub's Ubuntu runners come with Docker, so the Testcontainers tests (K22) run there.
- **What `-race` covers:** The race detector finds data races in Go memory. The claim, cancel and idempotency races in the database are proved by the PostgreSQL tests (K22), not by `-race`.
- **Built:** with the first working slice, so every pull request after it shows a CI result (K4).
- **Also considered:** CI plus publishing the image to GitHub Container Registry on every merge to `main` (the option I was recommended: a verifiable delivery step that needs only `GITHUB_TOKEN` with `packages: write`) and CI plus deployment to a hosted environment (a cloud account, secrets, a managed PostgreSQL and possible costs, none of which the case asks for).
- **What it costs:** The bonus is titled "CI/CD", and this pipeline has no delivery step; the README says so, and that Compose builds the image locally. Adding a publishing job later takes about 10 minutes.
- **How I will prove it (B6):** The workflow runs on the pull request that adds it and on every pull request after that, and its result is visible on each one.

## Health, image and configuration

### K30 — Health checks (R16): `/healthz` for liveness, `/readyz` for readiness

I chose a liveness endpoint and a readiness endpoint because they let me measure the API's health, and I left the provider out because the API keeps queueing notifications while webhook.site is down.

- **How it works:** Both are Huma operations, so they appear in the OpenAPI document (D4). `GET /healthz` checks no dependency and returns `200` whenever the API can answer. `GET /readyz` pings PostgreSQL with a short time limit and returns `200` with the result of each check, or `503` naming the failing check when the database cannot be reached; it also returns `503` once shutdown has started (K6). Health requests are logged at debug level (K21), so a check every few seconds does not flood the logs.
- **Why the provider is not checked:** The API never calls the provider (R7). While webhook.site is down, the API can still accept and queue notifications.
- **In Compose:** The `api` service's health check calls `/readyz` through the `healthcheck` command described in K31. Docker does not restart a container because it turns unhealthy; the status shows in `docker compose ps` and in `depends_on` conditions.
- **The worker:** It has no health endpoint. If it crashes, lease recovery (K3) takes its work back; if it stalls, the K20 metrics show the queue growing and `processing` notifications piling up.
- **Why this entry came late:** While reviewing the decisions against the requirements, I found that R16 had no decision. The PostgreSQL health check in Compose (K6) is not an API endpoint.
- **Also considered:** A single `/health` that checks the database (it matches the case's wording, but a liveness check that depends on the database can get a healthy process restarted) and worker heartbeats written to PostgreSQL and shown in `/readyz` (a view of the whole system, but R16 does not ask for it, and zero workers cannot be a reason for `503`, because the queue keeps accepting work).
- **What it costs:** The worker's health is visible only indirectly, through K3's recovery and K20's metrics.
- **How I will prove it (R16):** Through the real router: with the database reachable, both endpoints return `200`; with the database unreachable, `/healthz` returns `200` and `/readyz` returns `503`; after shutdown starts, `/readyz` returns `503`. In Compose, `docker compose ps` shows the API as healthy.

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
