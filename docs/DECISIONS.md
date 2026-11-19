# Design Decisions

Each entry says what I chose, why I chose it, and what it costs. I went through the options and their trade-offs with an AI assistant (see [`AI_USAGE.md`](AI_USAGE.md)); the choices and the reasons are mine.

## Development environment

### K0 — Local container runtime: Docker Desktop

I chose Docker Desktop because ease of installation should not decide a tool choice; I wanted the option that is soundest on its own merits.

- **Also considered:** Colima (command-line only and the lightest, but testcontainers needs extra settings) and OrbStack (light and fast, but free only for personal use).
- **What it costs:** It uses the most memory of the three, and the first launch needs administrator rights and a licence acceptance.
- **What it means for the code:** Nothing. `docker-compose.yml` stays runtime-agnostic (no Docker Desktop–only features such as `host.docker.internal`), so a reviewer can use any Docker runtime.

## Architecture

### K1 — Database: PostgreSQL

I chose PostgreSQL because schemas and SQL are ground I already know, so the queries this system needs are ones I can write and explain myself. This decision is expensive to reverse, so I read through the options carefully before choosing, and PostgreSQL came out as the most sensible fit.

- **Also considered:** MySQL 8 (it also supports `SKIP LOCKED`, but it has no `LISTEN/NOTIFY`, and I have never used it) and MongoDB (none of my SQL or migration experience carries over, and versioned migrations fit poorly without a schema).
- **What is new for me:** Two Go concepts: the database connection pool (the counterpart of SQLAlchemy's engine pool) and errors that come back as return values instead of being raised as exceptions. I am studying both before the first slice is built.
- **What it costs:** It is hard to reverse. Migrations and every query are written in the PostgreSQL dialect, so moving to another database would take an estimated 3+ hours. Tests cannot fall back to SQLite; they need a real PostgreSQL. That is fine for me, because the project always runs with a container.
- **What it means for the code:** `docker-compose.yml` runs PostgreSQL on host port 5433, so it does not collide with a local PostgreSQL on 5432. If a stale volume ever causes a schema mismatch, I delete it (`docker compose down -v`) and the migrations rebuild the schema.

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
