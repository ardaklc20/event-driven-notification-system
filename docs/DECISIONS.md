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

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
