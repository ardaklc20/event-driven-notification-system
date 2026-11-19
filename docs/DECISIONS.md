# Design Decisions

Each entry says what I chose, why I chose it, and what it costs. I went through the options and their trade-offs with an AI assistant (see [`AI_USAGE.md`](AI_USAGE.md)); the choices and the reasons are mine.

## Development environment

### K0 — Local container runtime: Docker Desktop

I chose Docker Desktop because ease of installation should not decide a tool choice; I wanted the option that is soundest on its own merits.

- **Also considered:** Colima (command-line only and the lightest, but testcontainers needs extra settings) and OrbStack (light and fast, but free only for personal use).
- **What it costs:** It uses the most memory of the three, and the first launch needs administrator rights and a licence acceptance.
- **What it means for the code:** Nothing. `docker-compose.yml` stays runtime-agnostic (no Docker Desktop–only features such as `host.docker.internal`), so a reviewer can use any Docker runtime.

## Process

### K4 — Git workflow: one branch and pull request per slice, merged with a merge commit

I chose to build every slice on its own branch and merge it through a pull request with a merge commit, because I want to work the way a team does and prove each slice in its own pull request.

- **How it works:** Branches are named `feat/<slice>` or `docs/<topic>`. Only the root commit (`.gitignore`) went straight to `main`, because a pull request needs a base branch.
- **Also considered:** Squash merge (one commit per slice on `main`, but the small steps disappear from `main`) and committing straight to `main` (no pull request evidence, and CI would catch problems only after they land).
- **What it costs:** About 3–5 extra minutes per slice. Without squashing, every commit inside a pull request stays in `main`'s history, so each one has to be clean and pass the tests.
- **How to read the history:** `git log --first-parent main` shows one line per slice; `git log` shows every step.
