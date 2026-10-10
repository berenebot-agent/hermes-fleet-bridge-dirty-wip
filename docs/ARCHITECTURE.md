# Architecture — Hermes Fleet Bridge

> V1 reference. Describes the shape of the running system, not a product spec.
> Behaviour is defined by `main.go`, `compose.go`, `docker.go`, `telegram.go`,
> `hermes.go`, `store.go`, and `config.go`.

## Purpose

Make a fleet of sleeping Hermes agents reachable from one Telegram forum
supergroup:

- one dedicated Telegram bot, one forum supergroup;
- one topic per configured Hermes agent;
- a text message in a topic wakes that agent, relays the turn to the Hermes
  API, streams `typing` while it runs, posts the final reply back to the topic,
  and removes the agent's containers after inactivity.

V1 is deliberately narrow: **text messages and final text replies only.** No
attachments, audio, photos, slash commands (beyond `/status` and `/help`), tool
approval UI, webhooks, or background-task awareness.

## Membership — compose project directories

A fleet member is a **compose project directory** listed in `HERMES_DIRS` (e.g.
`/home/ben/projects/testpilot-hermes`). The member's name — its Telegram topic
and state identity — is the directory's basename. The container is ephemeral and
never the source of truth: persistent state lives in the project's bind mounts,
so the bridge may `docker compose down` an idle member and `docker compose up -d`
to wake it, picking up fresh image and environment each wake.

The runtime image ships the Docker CLI + Compose plugin and mounts each member
directory at its **same absolute host path**, so the member's own relative bind
mounts (e.g. `./data`) resolve correctly when compose runs inside the bridge.

## Process and layout

One Go binary, one process, one Docker container. Standard library only, so the
image is a static `CGO_ENABLED=0` build on `alpine` plus the `docker` CLI and
compose plugin. The source is a flat `package main` split into narrow
transport/support files plus `main.go`:

| File | Responsibility |
|---|---|
| `config.go` | Environment parsing and validation; topic-name derivation |
| `compose.go` | Per-member `docker compose` lifecycle (up/down/ps/config) and API-env provisioning |
| `docker.go` | Docker Engine API over the mounted unix socket (inspect); endpoint discovery |
| `telegram.go` | Telegram Bot API (long polling, topics, typing, send); UTF-16-safe splitting |
| `hermes.go` | Hermes agent API client (`/health`, `/v1/runs`, run status/stop) |
| `store.go` | Durable JSON state: update offset, topic map, per-agent queues, activity, API keys |
| `main.go` | Wiring: poll loop, per-agent workers, topic monitor, idle monitor, command handling |

Each transport keeps its own struct and never reaches into another's internals.
A change to the Telegram wire format stays in `telegram.go`, and so on.

## Startup

1. Load and validate config (fail loud on any missing required variable).
2. Open the state store at `DATA_DIR/state.json` (default `/data/state.json`).
3. Delete any pre-existing webhook on **this bot token** (polling and webhooks
   are mutually exclusive).
4. Check each configured member for a compose file. Members without one are
   reported in General and excluded; the rest become the active fleet. A stopped
   or absent container is normal and is **not** an issue.
5. Start one worker goroutine per active member, plus the idle monitor and
   the topic monitor; run the long-poll loop on the main goroutine.
6. Post a service-start message to General.

Shutdown is context-driven on `SIGINT`/`SIGTERM`; state is already durable, so a
restart resumes from the stored offset and queues.

## Members and the Hermes API

The bridge never hardcodes a Hermes endpoint. For each member it:

1. **Owns the API server contract** in the member's persistent Hermes home.
   `compose config --format json` resolves the bind mount whose target is
   `/opt/data`; the bridge writes `API_SERVER_ENABLED=true`, `API_SERVER_HOST=0.0.0.0`,
   `API_SERVER_PORT` and `API_SERVER_KEY` into that directory's `.env` (Hermes
   loads that file with override semantics, so it — not the Docker environment —
   is authoritative). An existing strong key is adopted; otherwise a 256-bit
   random key is generated and persisted both in the member `.env` and in bridge
   state. If the file changed under a running member, the next wake uses
   `up --force-recreate` so the gateway picks it up.
2. **Wakes the member** with `docker compose up -d` when it has a turn.
3. **Discovers the endpoint** by inspecting the running container: `127.0.0.1`
   for `network_mode: host`, else a Docker bridge IP (or the network named by
   `HERMES_NETWORK` if multi-homed). DNS names are never trusted to construct a
   destination. The port comes from the container env `API_SERVER_PORT`, else
   `HERMES_API_PORT` (default `8642`); the key is the one the bridge provisioned.

The bridge runs `network_mode: host` so it can reach each container's inspected
bridge IP without reconfiguring Docker networks.

The Hermes client calls `GET /health` to confirm readiness before a turn, then
`POST /v1/runs` to start a run (with stable session headers and an
`Idempotency-Key`), polls `GET /v1/runs/{id}` every 2 s until a terminal status,
and `POST /v1/runs/{id}/stop` if a run exceeds `RUN_TIMEOUT`. Secrets are
redacted from any error surfaced in logs or to Telegram.

## Message flow

### Ingest (main goroutine, long poll)

`getUpdates` is a 25 s long poll with `allowed_updates=["message"]`. For each
update, `routeUpdate` decides an agent name, or `""` for anything not
addressed to a serviced topic. It **fails closed**: no DMs, other groups, bot
senders, unknown users, empty text, or unknown topics. General (thread 1) only
handles `/status` and `/help`.

`store.ingest` advances the offset and, when an agent is targeted, appends the
text to that agent's FIFO queue — both in the same atomic save. This is the
durability point: **an update is persisted before it is acted on**, and a
restart re-reads the offset and queues from disk.

### Per-agent worker (one goroutine per member)

Workers are independent, so members run concurrently. Each worker:

1. Takes the head of its agent's queue.
2. Starts the `typing` keepalive loop (every 4 s, including during cold boot).
3. Ensures the member is awake and the Hermes API is healthy (`ensureHermes`,
   serialised per agent by a lifecycle mutex: provisions the API env, runs
   `docker compose up -d` if stopped or env changed, then polls health until
   `START_TIMEOUT`).
4. Starts a Hermes run once, persisting `run_id` so a crash mid-run resumes
   polling rather than re-submitting.
5. Waits for the run to reach a terminal status, persisting the reply text
   **before** sending it — a failed send never reruns the agent.
6. Sends the reply, splitting it to stay within Telegram's UTF-16 limit
   (3500 units per message), advancing the sent-chunk cursor after each success.
7. Marks the turn finished and records `last_activity`.

A failed turn retries up to 3 times with linear backoff; after the third failure
a concise error is posted to the topic. A run exceeding `RUN_TIMEOUT` is asked to
stop and reported.

### Idle down

A monitor runs every 15 s. A member's containers are removed (`docker compose
down`) only when it has no active or queued turn and has been idle longer than
`IDLE_TIMEOUT`; the check is repeated after taking the per-agent lifecycle lock,
so a message accepted in the gap is not starved. Bind-mounted state persists, so
the next wake restores the agent with fresh image/env. Sessions and other work
running **outside** the bridge are not detected or protected.

## Topics

Topic names are derived from member names (a leading `hermes-` is dropped,
dashes/underscores become spaces, words are title-cased, capped at 64 runes).
The topic monitor runs at startup and every minute:

- create a topic for a member that has none (record the thread ID in state);
- rename a mapped topic when its derived title changes;
- recreate a topic that Telegram reports as missing.

No Telegram topic IDs are configured by hand. Thread IDs live only in state, so
the mapping survives restarts.

## Durable state

A single JSON file, `DATA_DIR/state.json`, mode `0600`, written by atomic
replace (temp file + `fsync` + rename + directory `fsync`) so a crash cannot
truncate it. It holds:

| Field | Meaning |
|---|---|
| `group_id` | The Telegram group this state belongs to |
| `offset` | Next Telegram update offset to request |
| `topics` | Member name → `{thread id, current title}` |
| `pending` | Member name → FIFO queue of turns (text, run ID, reply, sent cursor, attempts) |
| `last_activity` | Member name → timestamp of last completed turn |
| `api_keys` | Member name → the Hermes API key the bridge provisioned |

If the configured group ID changes, the bridge archives the old file
(`state.json.bak.<UTC>`) and starts fresh rather than crash-looping.

**Known limitation (documented, accepted):** as with most chat bridges, a crash
exactly after a reply is sent but before its progress is committed can cause a
duplicate Telegram reply on restart.

## Security posture

- **Fail closed** — every routing decision defaults to "ignore" (wrong group,
  unapproved user, bot, empty/unknown topic).
- **Untrusted input** — all Telegram text and all Hermes reply text is data, not
  instructions to the bridge.
- **Secrets never logged** — the bot token and Hermes API keys are redacted from
  errors; keys are generated by the bridge or adopted from the member's `.env`.
- **The mounted Docker socket is the control plane** and is effectively
  host-root-equivalent Docker control, including `docker compose` against the
  mounted member directories. The bridge only ever touches the configured member
  directories, but the socket itself must never be exposed and the service must
  never be published publicly.
- The default Compose service runs `read_only` with a `/tmp` tmpfs,
  `no-new-privileges`, and `cap_drop: ALL`; only `/data`, `/tmp` and the member
  directories are writable.

## Configuration reference

| Variable | Required | Meaning |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | Dedicated bot token (never shared with a polling Hermes) |
| `TELEGRAM_GROUP_ID` | yes | Numeric forum supergroup ID |
| `TELEGRAM_ALLOWED_USER_IDS` | yes | Comma-separated numeric user IDs (fail closed) |
| `HERMES_DIRS` | yes | Comma-separated absolute compose project directories (name = basename) |
| `IDLE_TIMEOUT` | no | Default `15m`, since last completed turn; min 10s |
| `START_TIMEOUT` | no | Default `120s` for a cold-booted Hermes API |
| `RUN_TIMEOUT` | no | Default `60m`; asks Hermes to stop a timed-out run |
| `HERMES_API_PORT` | no | API server port written into each member's `.env` (default `8642`) |
| `HERMES_NETWORK` | no | Preferred Docker network where a container is multi-homed |
| `DATA_DIR` | no | Defaults to `/data` |
| `DOCKER_SOCKET` | no | Defaults to `/var/run/docker.sock` |
