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

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
