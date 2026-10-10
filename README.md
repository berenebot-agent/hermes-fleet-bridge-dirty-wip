# Hermes Fleet Bridge (prototype V1)

One Telegram bot, one Telegram forum supergroup, one topic per Hermes Docker container. Written in Go using **only the standard library**. The bridge wakes sleeping containers when a user sends a text message, sends Telegram `typing` every four seconds while the container wakes and the Hermes run executes, posts the final text reply to the same topic, then stops the container after inactivity.

**Designed for Linux Docker Engine and an exclusively bridge-managed Hermes fleet.** No cron awareness, attachment relay, approval UI, commands, or webhooks in V1.

## Quick start

1. In Telegram, create a **supergroup** with **Topics enabled**. Create one bot with @BotFather, add it as a **group administrator** with **Manage Topics** permission. Admin bots receive ordinary group messages without disabling privacy mode.
2. Each Hermes container must start its Hermes gateway/API server on an address accessible from the Docker host, with these environment settings (or equivalent config):

   ```dotenv
   API_SERVER_ENABLED=true
   API_SERVER_HOST=0.0.0.0
   API_SERVER_PORT=8642
   API_SERVER_KEY=your-long-random-key
   ```

   The Hermes instance must retain its existing persistent `.hermes` data so sessions continue after sleep. **Do not configure these Hermes containers to poll Telegram using the same bot token.**
3. Copy the `.env.example` file to `.env`, and edit only your Telegram bot token, group ID, allowed Telegram user IDs and existing Docker container names. Example:

   ```dotenv
   TELEGRAM_BOT_TOKEN=123456:yourtoken
   TELEGRAM_GROUP_ID=-1001234567890
   TELEGRAM_ALLOWED_USER_IDS=123456789,987654321
   HERMES_CONTAINERS=hermes-atlas,hermes-janus,hermes-hydra
   IDLE_TIMEOUT=15m
   ```

4. From this folder, deploy:

   ```bash
   mkdir -p data
   chmod 700 data
   docker compose up -d --build
   docker compose logs -f
   ```

**That's all the per-agent bridge configuration.** The service inspects **only the named containers** and automatically discovers each container's `API_SERVER_KEY`, `API_SERVER_PORT`, and IP address. When the service starts, it creates topic **Atlas** for `hermes-atlas`, **Janus** for `hermes-janus`, and so on. The thread ID is saved to `./data/state.json`. Changing the container list creates missing topics. Changes to the generated title are propagated to Telegram automatically. Deleting an existing topic from Telegram causes it to be recreated on the next failed delivery if Telegram reports the missing topic. Removed container names are no longer serviced; historical topics are left untouched.

On typical Linux Docker installations, `network_mode: host` lets the bridge talk to each container's inspected bridge IP even when those containers aren't attached to the bridge's Compose network. The Hermes API must listen on `0.0.0.0` in its container. Where multiple Docker networks exist, set `HERMES_NETWORK=<existing-network-name>` in `.env` to pick one. If the API key is not exposed as an environment variable of each Hermes container, optionally set `HERMES_API_KEY` in the bridge's `.env` as a global fallback.

## Environment variables

| Name | Required | Meaning |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | Yes | Dedicated bot token |
| `TELEGRAM_GROUP_ID` | Yes | Numeric forum supergroup ID |
| `TELEGRAM_ALLOWED_USER_IDS` | Yes | Comma-separated numeric user IDs (fail closed) |
| `HERMES_CONTAINERS` | Yes | Comma-separated exact Docker container names |
| `IDLE_TIMEOUT` | No | Defaults to `15m`, measured since last completed bridge turn |
| `START_TIMEOUT` | No | Defaults to `120s` for a cold-booted Hermes API |
| `RUN_TIMEOUT` | No | Defaults to `60m`; asks Hermes to stop a timed-out run |
| `HERMES_API_PORT` | No | Fallback port `8642`, overridden by target container env |
| `HERMES_API_KEY` | No | Global fallback key if target container env doesn't expose one |
| `HERMES_NETWORK` | No | Choose named Docker network where multiple exist |
| `DATA_DIR` | No | Defaults to `/data` |
| `DOCKER_SOCKET` | No | Defaults to `/var/run/docker.sock` |

## Behaviour

- **Topic management:** On startup, create topics for configured containers that exist, or rename mapped topics when the generated title changes. Containers that don't exist are skipped (no topic created) and reported in **General**. No Telegram topic ID configuration needed.
- **General (service channel):** The forum's built-in **General** topic is the control channel for the bridge itself. On startup the bridge posts a service-start banner listing the containers it serves and any container problems. It re-checks containers every minute and posts an alert in General when a container goes missing/errors, and a recovery note when it comes back. `GET`-free commands `/status` and `/help` are accepted in General; General text is never routed to an agent.
- **Security:** Ignore all messages outside the configured group, all unapproved users, bot-originated messages, empty/non-text messages, and messages in unknown topics.
- **Multiplexing:** Each configured container has its own independent FIFO text queue; separate containers run concurrently.
- **Wake and typing:** Start a stopped container using Docker Engine API, wait for `/health`, call Hermes `POST /v1/runs` using stable session identifiers, and send Telegram `sendChatAction(typing)` every four seconds throughout wake and run.
- **Replies:** Poll Hermes `GET /v1/runs/{run_id}` every two seconds until terminal; split replies to meet Telegram limits. Runs for a given agent use a stable Hermes session ID across container restarts.
- **Persistence:** `./data/state.json` (mode 0600) holds Telegram update offset, topic IDs, queued text, run IDs, reply progress and last activity. Changes are written using atomic replace. A restarted bridge recovers queued messages and outstanding run IDs. **Like most chat bridges, a crash exactly after sending a reply but before committing its progress can cause a duplicate Telegram reply.**
- **Idle stop:** Do not stop containers with active or queued bridge turns. Once idle for `IDLE_TIMEOUT`, issue Docker `stop` with a 10-second grace period (then Docker forces termination). Scheduled jobs or other sessions running outside this bridge are **not detected or protected**.
- **Failures:** Retry a failed turn three times before posting an error to its Telegram topic. Idempotency keys protect Hermes run creation from accidental duplicate HTTP retries where supported by the Hermes API.

## Restrictions and security

- **Not production-hardened**: text messages and final replies only. No file, audio or photo relaying; no Telegram slash commands; no Hermes tool-approval handling or background-task awareness. A run waiting for approval stays active until its configured `RUN_TIMEOUT`.
- The bot must be administrator of a Telegram **forum** supergroup. It cannot transform an ordinary supergroup into a forum; enable Topics yourself.
- The bridge uses long polling, and removes any pre-existing webhook from **this same bot token**. Run only one polling instance per bot token.
- The mounted `/var/run/docker.sock` gives the bridge effectively **host-root-equivalent Docker control**, even though the application is programmed to touch only configured containers. Use a dedicated trusted host or a restricted Docker socket proxy for production. Never expose the socket or this service publicly.
- The Hermes API has full agent tool access. Restrict access to its Docker network, use unique strong API keys and allow only trusted Telegram users.
- Topic names come from container names, with an optional leading `hermes-` removed and dashes/underscores converted to spaces. Telegram messages show the shared Telegram bot identity, not distinct individual bots.

## Verify source

```bash
go test -race ./...
go vet ./...
go build .
```

The tests use local fake Docker, Hermes and Telegram HTTP endpoints, including a cold start, session continuation contract, typing, auto-created/renamed topics, Telegram group/user/topic filtering, queue durability and idle shutdown. A live end-to-end test still requires your actual Telegram supergroup, bot token and running Docker host.
