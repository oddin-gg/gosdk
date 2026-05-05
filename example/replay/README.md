# Replay example

Standalone CLI to drive the Oddin.gg **replay** REST endpoints and consume the
resulting `oddinreplay` AMQP feed via the SDK.

Replay is only available on `integration` environments.

```
go run ./example/replay <command> [args]
```

## Commands

| Command                                     | Description                                                                         |
| ------------------------------------------- | ----------------------------------------------------------------------------------- |
| `list`                                      | `GET /v1/replay` — raw XML response.                                                |
| `add <urn>...`                              | `PUT /v1/replay/events/{urn}` for each URN. `urn` form: `od:match:<id>`.            |
| `remove <urn>...`                           | `DELETE /v1/replay/events/{urn}` for each.                                          |
| `clear`                                     | `POST /v1/replay/clear` — empty the list and stop.                                  |
| `play [flags]`                              | `POST /v1/replay/play` (see flags below).                                           |
| `stop`                                      | `POST /v1/replay/stop`.                                                             |
| `status`                                    | `GET /v1/replay/status` — raw XML.                                                  |
| `listen [<urn>... \| all]`                  | Open a session against the `oddinreplay` exchange and print messages (see filters). |

### Play flags

| Flag                     | Default | Meaning                                  |
| ------------------------ | ------- | ---------------------------------------- |
| `-speed=<int>`           | `1`     | Playback speed multiplier.               |
| `-max-delay=<int>`       | `30000` | Cap inter-message delay (ms).            |
| `-rewrite-timestamps`    | `false` | Rewrite each message timestamp to *now*. |
| `-product=<live\|pre>`   | both    | Replay only one producer.                |
| `-parallel`              | `false` | Run multiple matches in parallel.        |

### Listen filters

- `listen` (no args) — fetches the current replay list and prints messages **only** for those events.
- `listen all` — no filter; print every message arriving on `oddinreplay`.
- `listen od:match:1234 od:match:5678` — explicit URN allow-list.

## Configuration

All overridable via environment variables:

| Variable | Default               | Notes                                          |
| -------- | --------------------- | ---------------------------------------------- |
| `TOKEN`  | hard-coded test token | Your `x-access-token`.                         |
| `ENV`    | `test`                | One of `test` / `integration` / `production`.  |
| `REGION` | `eu`                  | One of `eu` / `ap`.                            |
| `NODE`   | `2`                   | Replay `node_id`. Replay lists are isolated per node. |

> Replay is **not supported** when `ENV=production`.

## Recommended workflow

Two terminals.

**Terminal 1 — listener (start FIRST so you don't miss messages):**

```bash
go run ./example/replay listen
```

If the replay list is empty when you start, the listener prints nothing. Re-run after `add` so its filter picks up the new list — or use `listen all` to skip filtering.

**Terminal 2 — control:**

```bash
go run ./example/replay list                                # empty
go run ./example/replay add od:match:<ID1> od:match:<ID2>
go run ./example/replay list                                # both shown
go run ./example/replay status                              # stopped
go run ./example/replay play                                # default speed=1 max-delay=30000
go run ./example/replay status                              # playing
# messages stream into terminal 1
go run ./example/replay stop
go run ./example/replay clear
```

## Choosing match IDs

Replay only accepts events that match all of:

- the event is **published**,
- the event is **older than 48 hours but no older than 7 days**,
- there is replay data on file for the event.

If `add` returns *"No replay data for this event"* or *"record not found"* /
*"Sport event not found"*, pick a different match.
