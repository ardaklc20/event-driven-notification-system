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

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
