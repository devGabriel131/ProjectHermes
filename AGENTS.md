# AGENTS.md

## Production ready: false

All data is mock. Losing links, sessions, settings, or any rows in a migration is fine: prefer the simplest schema change (drop, recreate, reset) over backfills or compatibility code. Revisit when this flips to true.

## Dev server

- Keep one Air dev server always running: `go tool air` from the repo root, serving <http://127.0.0.1:3000>.
- Reuse it. Never start a second server or Air instance; start one only if nothing listens on port 3000.
- It needs the Compose database (`docker compose up --detach --wait db`) and `.env` with `DATABASE_URL` and `SESSION_COOKIE_SECURE=false`.

## Railway

- Pushes to `master` auto-deploy; push only with approval.
- After an approved push, use the Railway MCP to confirm the deployment reached SUCCESS, read its logs, and test the change live in its environment.
- Server-side dependency changes (`go.mod`/`go.sum`, Go toolchain, build or deploy config) first get a preview deployment in a non-production Railway environment, verified there, before merge or push to `master`.

## Docs

- A change that alters behavior updates the matching page in docs/ in the same change; docs/README.md maps each area to its page.
