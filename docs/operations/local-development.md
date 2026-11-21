# Local development

How to run the app on one machine: Compose PostgreSQL, the Air rebuild loop, a staff account and Stripe webhook forwarding. Environment variables are defined in [Configuration](configuration.md); the deployed setup is in [Railway](railway.md); checks are in [Testing](../testing.md).

## Quick start

```fish
test -f .env; or cp .env.example .env
docker compose up --detach --wait --wait-timeout 90 db
go tool air
```

- `.env.example` holds the local Compose `DATABASE_URL` and Stripe **placeholders**. Its `PUBLIC_BASE_URL=http://localhost:3000` keeps session cookies without `Secure`, so sign-in works over plain http. The server refuses to start without a valid sandbox Stripe key, webhook secret, `PUBLIC_BASE_URL`, two configuration IDs and the six catalog prices; see [Stripe setup](stripe-setup.md).
- `go.mod` declares Air as a project tool, so run `go tool air`, never a global `air`.
- The server listens on `127.0.0.1:3000` unless `PORT` is set ([Configuration](configuration.md)).
- Create a staff account before opening `/management/` (below).

```mermaid
flowchart LR
    customer["Browser: customer"]
    staff["Browser: staff"]
    checkout["Stripe-hosted Checkout"]
    stripe["Stripe sandbox API"]
    subgraph host["Developer machine"]
        air["go tool air\nenv_files .env"]
        app["tmp/main\n127.0.0.1:3000"]
        listen["stripe listen"]
        pg[("postgres:18\n127.0.0.1:5432")]
    end
    air -->|"builds and runs"| app
    customer -->|"/customer/{token}"| app
    staff -->|"/management/"| app
    staff -->|"GET /management/events"| app
    app -->|"SQL"| pg
    app -->|"HTTPS API calls"| stripe
    customer -->|"redirect to Checkout"| checkout
    checkout -->|"return to PUBLIC_BASE_URL"| customer
    stripe -->|"events"| listen
    listen -->|"POST /webhooks/stripe"| app
```

## Database

[compose.yaml](../../compose.yaml) runs one `db` service, `postgres:18` pinned to the same digest as CI ([Testing](../testing.md#ci)), published only on `127.0.0.1:5432` with database `hermes` and user and password `postgres`. Data lives in the named volume `postgres18_data`, mounted at `/var/lib/postgresql` (Postgres 18 keeps its cluster in the versioned subdirectory `18/docker`), so it survives `docker compose down`; `docker compose down --volumes` discards it. `--wait` blocks until `pg_isready` passes (checked every 2 s, up to 15 retries).

Compose used to run `postgres:17` with the volume `postgres_data`. Postgres 18 cannot start on a 17 data directory, so the 18 service uses a new volume and leaves the old one untouched. Once you no longer need the old data, drop it from the repository root (Compose prefixes the volume with the project name, the lowercased directory name):

```fish
docker volume rm (string lower (basename $PWD))_postgres_data
```

The new volume starts empty, holding only `hermes`. If you set up the integration test database before, re-create `hermes_test` on it, or every integration test fails in `testdb.Open` ([Testing](../testing.md#commands)):

```fish
docker compose up --detach --wait --wait-timeout 90 db
docker compose exec -T db createdb -U postgres hermes_test
```

The app creates its own tables: startup runs `AutoMigrate` ([Startup and shutdown](startup-shutdown.md)). Mock data may be dropped freely ([AGENTS.md](../../AGENTS.md#production-ready-false)).

This repository owns the `hermes` database. ProjectHephaestus (`../ProjectHephaestus`) connects to the same database and has no Compose database of its own, so start it from here.

## Air

Air is configured in [.air.toml](../../.air.toml), which sets only six things.

| Setting | Value | Effect |
|---|---|---|
| `env_files` | `[".env"]` | Air loads `.env` into the app's environment. A variable already set in your shell wins. **The app never reads `.env` itself.** |
| `build.pre_cmd` | `go tool templ generate` | Regenerates `*_templ.go` from the `.templ` views before every build ([templ](#templ)). |
| `build.entrypoint` | `./tmp/main` | The binary Air runs; the build is Air's default `go build -o ./tmp/main .`. |
| `build.include_ext` | `go`, `templ`, `html`, `css`, `js` | A change to any such file rebuilds and restarts. |
| `build.exclude_dir` | `tmp`, `loadtest` | Air ignores its own build output and the k6 scripts, so editing a load test never restarts the server. The repository has none of Air's other default exclusions (`assets`, `vendor`, `testdata`). |
| `build.exclude_regex` | `_test\.go$`, `_templ\.go$` | Tests never rebuild, as by Air's default; generated views are not watched, so `pre_cmd` writing them cannot trigger a second rebuild. |

Consequences:

- **Static files are embedded.** `main.go` embeds `static/` (`assets`), so a CSS or script edit takes effect only after Air rebuilds.
- **No browser refresh.** `.air.toml` has no `[proxy]` section; reload the page yourself.
- **Rebuilds kill the server.** `.air.toml` does not set `send_interrupt`, so Air stops the old process with SIGKILL. Locally there is no graceful drain and no `shutdown_*` log lines; the shutdown sequence in [Startup and shutdown](startup-shutdown.md) applies to SIGINT and SIGTERM only.
- **Environment edits.** `.env` is not a watched file. Air re-reads it on the next rebuild, so save any watched file or restart Air after changing it.
- **Run exactly one Air.** It owns port 3000 and `tmp/main`. Reuse the running instance and start one only if nothing listens on port 3000 (rule and reasoning in [AGENTS.md](../../AGENTS.md#dev-server)).

## templ

Views are `.templ` files beside their handlers, and the layouts they share are in `internal/ui` ([Architecture](../architecture.md)). templ is a `tool` line in `go.mod`, so run `go tool templ`, never a global `templ`. Each `.templ` compiles to a `*_templ.go` file that is committed; never edit one by hand.

```fish
go tool templ fmt internal  # format .templ files
go tool templ generate      # regenerate every *_templ.go
```

Air regenerates before each build, so a running dev server needs nothing more. Commit the `.templ` change and its regenerated `*_templ.go` together: CI regenerates and fails on any difference ([Testing](../testing.md#ci)).

## Staff accounts

Accounts exist only through the CLI; there is no registration or email recovery. The CLI needs `DATABASE_URL` in its own environment (not a running server or Stripe configuration), and because the app never loads `.env`, pass it explicitly:

```fish
env (string match -r '^DATABASE_URL=.*' < .env) go run . user add alice
go run . --help   # usage, username and password rules; needs no environment
```

- Commands: `add`, `passwd`, `revoke`, `remove` take a username; `list` takes none. Passwords are read twice from a hidden terminal prompt, so run `add` and `passwd` in an interactive terminal, never through a pipe.
- Every `user` command connects to the database and runs `AutoMigrate` first, so point `DATABASE_URL` only at the database you mean to migrate.
- Account rules, sessions and login limiting: [Staff auth](../staff-auth.md). The deployed equivalent is `./user.fish` ([Railway](railway.md)).

## Stripe webhooks

Checkout and invoice outcomes reach the app as signed webhooks ([Webhooks](../billing/webhooks.md)). Locally the Stripe CLI forwards them:

```fish
stripe listen --forward-to http://127.0.0.1:3000/webhooks/stripe
```

1. The CLI prints a `whsec_...` signing secret.
2. Put it in `STRIPE_WEBHOOK_SECRET` in `.env` and let Air rebuild (see **Air** above).
3. Keep `stripe listen` running while you test payments.

Local development uses the `stripe listen` secret; a deployed environment uses the signing secret of the registered webhook endpoint ([Stripe setup](stripe-setup.md)). Without forwarding no event reaches the app, so the worker only notices a payment on a scheduled contract re-sync, which runs every 15 minutes while a Checkout attempt is in flight ([Worker](../billing/worker.md)).

## Management tabs and live updates

Every open links, Trash, students or dashboard page holds one server-sent-event stream (`GET /management/events`). The local server speaks plain HTTP/1.1, and browsers allow six connections per host, so six such tabs starve every other request to that host. **Keep fewer than six management tabs open locally.** The settings and sign-in pages open no stream. Stream behavior: [Live updates](../live-updates.md).

## Source

- `compose.yaml`: `postgres:18@sha256:…`, `POSTGRES_DB`, `postgres18_data`, `pg_isready`
- `.air.toml`: `env_files`, `pre_cmd`, `entrypoint`, `include_ext`, `exclude_dir`, `exclude_regex`
- `go.mod`: `tool github.com/a-h/templ/cmd/templ`
- `.env.example`: `DATABASE_URL`, `STRIPE_WEBHOOK_SECRET`, `PUBLIC_BASE_URL`
- `main.go`: `assets`, `run`, `newServer`, `models`
- `internal/auth/cli.go`: `Usage`, `RunCLI`, `readNewPassword`
- `internal/live/live.go`: `Feed`
- `static/management-live.js`: `listenForChanges`
- `AGENTS.md`: `Production ready: false`, `Dev server`
