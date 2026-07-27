# Deploying Agent Task Broker with Kamal 2

The Docker alternative to the [systemd recipe](./README.md). Same single process —
one Go binary serving `/rpc`, the embedded admin UI and the poll endpoint — but built
into an image, run as a container behind `kamal-proxy`, with the SQLite database on a
persistent volume. Deploys are health-gated: traffic swaps to the new container only
after it answers `GET /health`, and the previous one keeps serving until then.

## Nothing deployment-specific is committed

This is a public repository, so every committed file is **abstract**:

| File | Contains |
|---|---|
| `config/deploy.yml` | structure only — every host, domain, registry and port is `ENV[...]` |
| `.kamal/secrets` | references (`API_KEY=$API_KEY`), never values |
| `deploy/Dockerfile` | the build recipe |
| `.env.example` | the documented list of variable NAMES, with empty values |
| `.env` | **your** server IP, ssh user, domain and keys — gitignored, never committed |

That includes the **ssh account name**: `KAMAL_SSH_USER` has no default anywhere in
the repo. A committed username is both a guess about someone else's server and a
small piece of infrastructure intel — it belongs in `.env` like the rest.

Kamal itself does **not** read `.env`. The `make` targets source it before invoking
`kamal`; that is what makes the ERB lookups in `config/deploy.yml` and the `$VAR`
references in `.kamal/secrets` resolve. Run `kamal` by hand and you must do it
yourself:

```bash
set -a; source .env; set +a
kamal deploy
```

Skip that and the ERB values come out empty — the failure surfaces as an obscure
`servers/web/0` error, not as "you forgot the env".

## Prerequisites

- **Deploy host** (your machine or a build box): Docker, Ruby, and Kamal 2
  (`gem install kamal`).
- **Target server**: SSH access as a user in the `docker` group (or root), and Docker
  installed. Kamal installs and manages `kamal-proxy` itself on first deploy.
- **A registry** the server can pull from. Either a public one (ghcr.io, Docker Hub)
  with credentials, or a registry running on the deploy host — Kamal tunnels it over
  SSH, in which case leave `KAMAL_REGISTRY_USERNAME`/`PASSWORD` empty.

## First deploy

```bash
cp .env.example .env
$EDITOR .env                 # server IP, domain, API_KEY, registry
make kamal config            # renders the config — catches an unset variable early
make kamal deploy
```

`make kamal deploy` builds the image (Svelte UI → Go binary → alpine), pushes it, boots a
container, waits for `/health`, then swaps traffic. Re-run it for every subsequent
deploy; the data volume is untouched by rebuilds.

## Everyday commands

There is one entry point: `make kamal <words>` loads `.env`, checks the variables
the config needs, and passes `<words>` to kamal unchanged.

```bash
make kamal deploy            # build → push → boot → health-gate → swap
make kamal config            # validate the rendered config without deploying
make kamal rollback          # back to the previous version
make kamal app details       # what is running where
make kamal proxy reboot
```

Three commands are needed daily but carry options, so they are declared once as
Kamal **aliases** in `config/deploy.yml` — which means they work from a raw `kamal`
invocation exactly as they do through make:

| Alias | Runs | What it does |
|---|---|---|
| `kamal logs` | `app logs -f` | follow container logs |
| `kamal shell` | `app exec --interactive --reuse "sh"` | shell **inside the running container** |
| `kamal sql` | `app exec --interactive --reuse "sqlite3 -readonly …"` | read-only query, statement on stdin |

```bash
echo "SELECT status, count(*) FROM tasks GROUP BY status;" | make kamal sql
```

`--reuse` is the load-bearing part of the last two: without it kamal starts a fresh
container and you land in an empty `/data` rather than at the live database. Note
that aliases do not appear in `kamal help`, but they do run.

Options can not be typed bare — make parses the command line before the recipe runs
and swallows anything starting with `-` (`-f` even expects a makefile after it). Pass
them through `ARGS`, which replaces the words entirely:

```bash
make kamal ARGS="app logs -n 200 --grep=poll"
```

Override the kamal binary if it is not on `PATH`: `make kamal deploy KAMAL=/path/to/kamal`.

## Data

The SQLite database lives on the named volume `<service>_data`, mounted at `/data`,
with `DB_PATH=/data/broker.db`. It is deliberately **not** in the image: an image
rebuild would otherwise discard every task, message and poll token.

Reads are safe while the container runs (`make kamal sql` opens the database
read-only; WAL allows concurrent readers). For a **write** — a manual repair, a
restore — stop the container first, then bring it back with `make kamal deploy`:

```bash
kamal app stop
# … operate on the volume with a throwaway container …
make kamal deploy
```

Back up by copying `broker.db` (plus `-wal`/`-shm`) off the volume, or by running
`sqlite3 /data/broker.db ".backup /data/backup.db"` inside `make kamal shell`.

## What is deliberately open

`AuthMiddleware` gates everything behind `API_KEY` **except** three paths, and each
exemption is load-bearing:

- `GET /health` — the liveness probe. `kamal-proxy` polls it with no credential; if
  it were gated, a deploy with `API_KEY` set would never go healthy. The body carries
  version, protocol version, the two feature flags and an echo of the caller's own
  project id — no data, no secrets.
- `GET /poll/{token}` — capability URL; the unguessable token *is* the credential.
- `GET /skill/install` — the non-secret installer scripts, fetched by harnesses that
  have no credential yet.

## Known limitation — the admin UI and `API_KEY`

The admin UI is a plain SPA that sends no `Authorization` header, so **with `API_KEY`
set the browser gets 401 on `/admin/`**. On a public deployment that leaves two
honest options today:

- keep `API_KEY` set (so `/rpc` is protected) and reach the admin UI through an SSH
  tunnel to the container port rather than over the public domain; or
- put an additional credential (basic auth) on `/admin/` at the reverse proxy in
  front of Kamal.

Leaving `API_KEY` empty to make the UI reachable would also un-gate `/rpc` — do not
do that on a public host. Giving the UI a browser-friendly login is the proper fix
and has not been built yet.

## Choosing between systemd and Kamal

Both are supported; they are not meant to run side by side on one host.

- **systemd** — fewer moving parts, no registry, no Docker. Good on a box you own and
  build on directly. Restarts are `make systemd-restart` and briefly drop the port.
- **Kamal** — reproducible image, health-gated zero-downtime swaps, trivial rollback
  to the previous image, and several apps sharing one server behind `kamal-proxy`.
  The cost is the extra indirection: environment variables must be declared in
  `config/deploy.yml` to reach the container, and anything you want available for
  debugging must be in the image.
