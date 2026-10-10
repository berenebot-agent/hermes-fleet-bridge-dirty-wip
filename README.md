# Hermes Fleet Bridge (prototype V1)

One Telegram bot, one Telegram forum supergroup, one topic per Hermes agent. Written in Go using **only the standard library**. Each agent is a Docker Compose project directory; the bridge wakes sleeping agents when a user sends a text message, sends Telegram `typing` every four seconds while the agent wakes and the Hermes run executes, posts the final text reply to the same topic, then removes the agent's containers after inactivity.

**Designed for Linux Docker Engine and an exclusively bridge-managed Hermes fleet.** No cron awareness, attachment relay, approval UI, commands, or webhooks in V1.

## How fleet membership works

A fleet member is a **compose project directory** (e.g. `/home/ben/projects/testpilot-hermes` with its `docker-compose.yml`). The agent's name is the directory's basename; the ephemeral container is never the source of truth. Because persistent state lives in bind mounts, the bridge may freely `docker compose down` an idle agent and later `docker compose up -d` to wake it — picking up fresh image/env each wake.

The bridge also owns each agent's API server configuration: on wake it writes `API_SERVER_ENABLED/HOST/PORT/KEY` into the agent's persistent Hermes home (`<data>/.env`, resolved from the compose file's `/opt/data` bind mount), adopting the existing key if one is present or generating a strong one otherwise. Hermes loads that file with override semantics, so this reliably makes the API server reachable on `0.0.0.0` and authenticated.

## Quick start

1. In Telegram, create a **supergroup** with **Topics enabled**. Create one bot with @BotFather, add it as a **group administrator** with **Manage Topics** permission. Admin bots receive ordinary group messages without disabling privacy mode.
2. Give each Hermes agent a compose project directory with its persistent data on a bind mount (e.g. `./data:/opt/data`). The bridge configures the API server itself; you do not need to set `API_SERVER_*` by hand.
3. Copy the `.env.example` file to `.env`, and edit your Telegram bot token, group ID, allowed Telegram user IDs and the agents' compose directories. Example:

   ```dotenv
   TELEGRAM_BOT_TOKEN=123456:yourtoken
   TELEGRAM_GROUP_ID=-1001234567890
   TELEGRAM_ALLOWED_USER_IDS=123456789,987654321
   HERMES_DIRS=/home/ben/projects/hermes-fleet-bridge/agents/redshirt-one,/home/ben/projects/hermes-fleet-bridge/agents/redshirt-two,/home/ben/projects/hermes-fleet-bridge/agents/redshirt-three
   IDLE_TIMEOUT=15m
   ```

4. Add one bind-mount line per agent directory to `docker-compose.yml` — at the **same absolute path** as on the host, so the project's own relative bind mounts resolve when the bridge runs `docker compose` against it:

```yaml
    volumes:
      - /home/ben/projects/hermes-fleet-bridge/agents/redshirt-one:/home/ben/projects/hermes-fleet-bridge/agents/redshirt-one
      - /home/ben/projects/hermes-fleet-bridge/agents/redshirt-two:/home/ben/projects/hermes-fleet-bridge/agents/redshirt-two
      - /home/ben/projects/hermes-fleet-bridge/agents/redshirt-three:/home/ben/projects/hermes-fleet-bridge/agents/redshirt-three
    ```

5. From this folder, deploy:

   ```bash
   mkdir -p data
   chmod 700 data
   docker compose up -d --build
   docker compose logs -f
   ```

**That's all the per-agent bridge configuration.** When the service starts, it creates topic **Redshirt One** for `redshirt-one`, **Redshirt Two** for `redshirt-two`, **Redshirt Three** for `redshirt-three`, and so on. The thread ID is saved to `./data/state.json`. Changing the directory list creates missing topics. Changes to the generated title are propagated to Telegram automatically. Deleting an existing topic from Telegram causes it to be recreated on the next failed delivery if Telegram reports the missing topic. Removed directories are no longer serviced; historical topics are left untouched.

On typical Linux Docker installations, `network_mode: host` lets the bridge talk to each container's inspected bridge IP even when those containers aren't attached to the bridge's Compose network. The Hermes API must listen on `0.0.0.0` in its container. Where multiple Docker networks exist, set `HERMES_NETWORK=<existing-network-name>` in `.env` to pick one. If the API key is not exposed as an environment variable of each Hermes container, optionally set `HERMES_API_KEY` in the bridge's `.env` as a global fallback.

## Environment variables

| Name | Required | Meaning |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | Yes | Dedicated bot token |
| `TELEGRAM_GROUP_ID` | Yes | Numeric forum supergroup ID |
| `TELEGRAM_ALLOWED_USER_IDS` | Yes | Comma-separated numeric user IDs (fail closed) |
| `HERMES_DIRS` | Yes | Comma-separated absolute compose project directories (agent name = directory basename) |
| `IDLE_TIMEOUT` | No | Defaults to `15m`, measured since last completed bridge turn |
| `START_TIMEOUT` | No | Defaults to `120s` for a cold-booted Hermes API |
| `RUN_TIMEOUT` | No | Defaults to `60m`; asks Hermes to stop a timed-out run |
| `HERMES_API_PORT` | No | API server port written into each member's `.env` (default `8642`) |
| `HERMES_NETWORK` | No | Choose named Docker network where multiple exist |
| `DATA_DIR` | No | Defaults to `/data` |
| `DOCKER_SOCKET` | No | Defaults to `/var/run/docker.sock` |

## Behaviour

- **Topic management:** On startup, create topics for configured members, or rename mapped topics when the generated title changes. A member whose directory has no compose file is skipped (no topic created) and reported in **General**. No Telegram topic ID configuration needed.
- **General (service channel):** The forum's built-in **General** topic is the control channel for the bridge itself. On startup the bridge posts a service-start banner listing the members it serves and any member problems. It re-checks members every minute and posts an alert in General when a project directory becomes unusable, and a recovery note when it comes back. `GET`-free commands `/status` and `/help` are accepted in General; General text is never routed to an agent.
- **Security:** Ignore all messages outside the configured group, all unapproved users, bot-originated messages, empty/non-text messages, and messages in unknown topics.
- **Multiplexing:** Each configured member has its own independent FIFO text queue; separate members run concurrently.
- **Wake and typing:** Bring the member's compose project up (`docker compose up -d`, force-recreating if the bridge changed its API env), wait for `/health`, call Hermes `POST /v1/runs` using stable session identifiers, and send Telegram `sendChatAction(typing)` every four seconds throughout wake and run.
- **Replies:** Poll Hermes `GET /v1/runs/{run_id}` every two seconds until terminal; split replies to meet Telegram limits. Runs for a given agent use a stable Hermes session ID across container restarts.
- **Persistence:** `./data/state.json` (mode 0600) holds Telegram update offset, topic IDs, queued text, run IDs, reply progress, last activity and per-member API keys. Changes are written using atomic replace. A restarted bridge recovers queued messages and outstanding run IDs. **Like most chat bridges, a crash exactly after sending a reply but before committing its progress can cause a duplicate Telegram reply.**
- **Idle down:** Do not remove containers with active or queued bridge turns. Once idle for `IDLE_TIMEOUT`, run `docker compose down` (containers are removed; bind-mounted state persists). Scheduled jobs or other sessions running outside this bridge are **not detected or protected**.
- **Failures:** Retry a failed turn three times before posting an error to its Telegram topic. Idempotency keys protect Hermes run creation from accidental duplicate HTTP retries where supported by the Hermes API.

## Restrictions and security

- **Not production-hardened**: text messages and final replies only. No file, audio or photo relaying; no Telegram slash commands; no Hermes tool-approval handling or background-task awareness. A run waiting for approval stays active until its configured `RUN_TIMEOUT`.
- The bot must be administrator of a Telegram **forum** supergroup. It cannot transform an ordinary supergroup into a forum; enable Topics yourself.
- The bridge uses long polling, and removes any pre-existing webhook from **this same bot token**. Run only one polling instance per bot token.
- The mounted `/var/run/docker.sock` gives the bridge effectively **host-root-equivalent Docker control**, including the ability to run `docker compose` against the mounted member directories. Use a dedicated trusted host or a restricted Docker socket proxy for production. Never expose the socket or this service publicly.
- The bridge writes `API_SERVER_*` (including a generated key, stored in member `.env` and bridge state) into each member's persistent Hermes home. That key grants full agent tool access over HTTP — keep the port un-published and treat member directories as bridge-managed.
- Topic names come from member names, with an optional leading `hermes-` removed and dashes/underscores converted to spaces. Telegram messages show the shared Telegram bot identity, not distinct individual bots.

## Verify source

```bash
./hermes-fleet-bridge-go.sh test -race ./...
./hermes-fleet-bridge-go.sh vet ./...
./hermes-fleet-bridge-go.sh build .
```

The tests use local fake Docker, Compose, Hermes and Telegram HTTP endpoints, including a cold start, session continuation contract, typing, auto-created/renamed topics, Telegram group/user/topic filtering, queue durability and idle shutdown. A live end-to-end test still requires your actual Telegram supergroup, bot token and running Docker host.
