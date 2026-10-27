# sscale — Huawei Scale 3 weight tracker

`sscale` reads weigh-ins from a **HUAWEI Scale 3** over Bluetooth LE on a Linux
server, stores them in SQLite and serves a calendar web app (an installable
PWA) plus a REST API. Everything ships as one Docker image.

Per weigh-in:

| Value | Source |
|---|---|
| weight, time | scale |
| bio-impedance (Ω), heart rate (if measured) | scale |
| BMI | computed from your height |
| body fat %, water %, skeletal muscle, bone, lean mass, protein %, BMR, visceral fat | computed from impedance + your profile (openScale's Scale 3 model; matches the scale's own fat %) |

Readings that don't fit your trend — e.g. someone else stepping on the
scale — are hidden automatically (see [Filtering](#filtering-readings-that-are-not-you)).

## Quick start

```sh
docker compose build   # build the image once (web app + Go, runs all tests)

./sscale pair          # step on the scale: authenticates with keys.json
./sscale               # start the daemon (BLE + web app) in the background
./sscale apikey        # the API key for the web app
```

Open **http://SERVER:8090** and enter the API key. On the iPhone: Safari →
Share → **Add to Home Screen** for a full-screen app.

| Launcher (host) | |
|---|---|
| `./sscale` | start the daemon in the background (the Docker container) |
| `./sscale -f` | run it in the foreground, log on the terminal (Ctrl-C stops) |
| `./sscale stop` | stop the daemon |
| `./sscale logs [-f]` | daemon log |
| `./sscale scan\|pair\|once\|status\|history\|apikey …` | CLI commands, run inside the container |

`./sscale` never builds anything; if the image is missing it tells you to run
`docker compose build`. Inside the container the binary is also `sscale`.

| Command | What it does |
|---|---|
| `sscale` / `sscale -f` | daemon in the background / foreground |
| `sscale scan [--duration 20s] [--all]` | list nearby Huawei scales (or all LE devices) |
| `sscale pair [--timeout 2m]` | wait for the scale, authenticate, sync time + profile, read firmware |
| `sscale once [--timeout 10m] [--json]` | capture one weigh-in and exit |
| `sscale status [--json]` | daemon, scale, last session, last measurement |
| `sscale history [-n 20] [--all] [--json]` | readings (`--all` includes hidden ones) |
| `sscale apikey [--rotate]` | print / replace the API key |
| `sscale stop` | stop a daemon started by the bare binary |

`pair` and `once` refuse to run while the daemon runs (only one process may
talk to the scale).

## Web app

One view: a month calendar. Each day shows your weight and the change since
the previous weigh-in (green down, red up); dots mark several readings. Above
it: latest weight, the month's trend line, average, change and range. Tap a
day for all readings with full body composition, and to mark a reading
**Not me** / **That was me**, reset it to automatic, or delete it. Swipe
left/right to change month.

Settings (gear icon): height, birthdate, sex (used for BMI and body
composition, and sent to the scale), the filter and its tolerance, and
"show hidden readings". Changing your profile recomputes past readings.

It's a PWA: manifest, icons, iOS standalone meta tags, safe-area layout,
light/dark following the system, and a service worker for the offline app
shell; the last data is cached on the device. It refreshes every 30 s and
when reopened, so a new weigh-in shows up right away.

**HTTPS.** iOS installs the app over plain HTTP too, but the service worker
(offline start) needs HTTPS. Either put sscale behind an HTTPS reverse proxy
(Caddy, Tailscale `serve`, …) or give it a certificate trusted by the phone:
mount it and set `SSCALE_TLS_CERT` / `SSCALE_TLS_KEY`.

## Filtering readings that are not you

Single-user app: each reading is compared with the median of your recent
valid readings (last 30 days, up to 10). It counts if it is within
max(**3 kg**, **4 %**) — both adjustable — plus 0.2 kg per day since your last
weigh-in (so a comeback after a break isn't rejected). Hidden readings stay
in the database with the reason (e.g. "−20.4 kg from the trend").

* "That was me" / "Not me" always wins; "that was me" also re-anchors the
  trend.
* If you have no valid reading for 14 days and then three consistent new
  ones arrive, that becomes your new trend.
* The very first reading (or the first after 30 days) is accepted.

## REST API

All endpoints except `/api/health` need `Authorization: Bearer <key>` (or
`X-API-Key`). The key is generated on first start, stored in the database and
printed to the log at every start; `SSCALE_API_KEY` sets a fixed one.

| | |
|---|---|
| `GET /api/health` | liveness |
| `GET /api/status` | daemon, scale (awake/last seen), device, last session, counts, latest reading |
| `GET /api/measurements?from=&to=&include_invalid=` | readings with derived values; bounds are `YYYY-MM-DD` or RFC 3339 |
| `PATCH /api/measurements/{id}` `{"valid": true\|false\|null}` | your decision (`null` = automatic) |
| `DELETE /api/measurements/{id}` | delete a reading |
| `GET /api/settings`, `PUT /api/settings` | profile + filter settings |

```sh
curl -H "Authorization: Bearer $(./sscale apikey)" "http://localhost:8090/api/measurements?from=2026-10-01"
```

## keys.json

```json
{
  "address": "AA:BB:CC:DD:EE:FF",
  "model": "HUAWEI SCALE 3",
  "cak": "…32 hex…", "c1": "…32 hex…", "c2": "…32 hex…",
  "height_cm": 178, "birthdate": "2000-05-01", "sex": "male"
}
```

`address`, `cak`, `c1`, `c2` are required (the session key is bound to the
address). The profile fields only seed the settings on first start; after
that, edit them in the web app.

## How it works

**Protocol** — Huawei "Hagrid WSP", ported from
[openScale](https://github.com/oliexdev/openScale): challenge/response with
HMAC-SHA256 over the CAK, a root key derived from C1/C2/address that wraps a
random AES-128-CTR session key, ≤20-byte frames with CRC-16. Then: sync
clock, write the encrypted user profile, request the realtime record, read
stored history.

Observed on this Scale 3 (fw 2.0.0.70), and handled:

* It answers only the first measurement-status poll (`01` even when idle) and
  never reports `00`; the realtime record signals the weigh-in. sscale
  re-requests it while it lacks impedance (up to 15 s), then disconnects.
* It isn't bound to a Huawei Health account, so it declines the user profile
  (`01`); recording is unaffected because sscale computes body composition.
* It stores no offline history, so only weigh-ins caught live are recorded —
  the daemon connects within seconds of the scale waking, so that's normally
  all of them.
* It keeps advertising for ~3 minutes after a weigh-in (its own timer).

**Not keeping the scale awake** — the daemon only listens for adverts. When
the scale wakes it connects once (~20–30 s), records and disconnects, then
waits until the scale has been silent for 60 s before connecting again (at
most 15 min). The cooldown survives daemon restarts. Failed sessions retry
with backoff.

**Docker** — the host's `bluetoothd` keeps the controller; the container
talks to it over D-Bus (`/run/dbus` mounted, `apparmor=unconfined` because
Docker's default AppArmor profile blocks D-Bus; no privileged mode). The
image is built in three stages: Node builds the PWA, Go embeds it into a
static binary (pure-Go SQLite), Alpine runs it. `restart: unless-stopped`,
with a healthcheck.

## Data (`./data`)

* `sscale.db` — SQLite: readings, settings, API key
* `state.json` — daemon/scale/pairing state
* `sscale.log` — log of a daemon started by the bare binary (Docker uses `./sscale logs`)
* `measurements.jsonl.imported` — earlier JSONL log, imported once

## Configuration

Set in the shell before `./sscale` / `./sscale -f`, or in `docker-compose.yml`:

| Env | Default | |
|---|---|---|
| `SSCALE_PORT` | `8090` | host port of the web app (compose only) |
| `SSCALE_HTTP` | `:8080` | listen address in the container (`""` disables the web server) |
| `SSCALE_API_KEY` | generated | fixed API key |
| `SSCALE_TLS_CERT`, `SSCALE_TLS_KEY` | — | enable HTTPS |
| `SSCALE_LOG_LEVEL` | `info` | `debug` shows protocol steps |
| `SSCALE_TRACE` | `false` | log every BLE frame |
| `SSCALE_ADAPTER` | first | e.g. `hci1` |
| `SSCALE_SCAN_WINDOW` / `SSCALE_SCAN_PAUSE` | `30s` / `0` | discovery windows (0 pause = continuous) |
| `SSCALE_QUIET_PERIOD` / `SSCALE_MAX_COOLDOWN` | `60s` / `15m` | reconnect policy |
| `SSCALE_IDLE_GRACE` | `25s` | how long to wait for a weigh-in after connecting |

## Development

All tooling runs in Docker. `docker compose build` lints and builds the web
app and runs the Go tests (protocol vectors from a real Scale 3 capture, a
simulated scale covering the observed firmware behaviour, the filter, the
API). For UI work run the daemon (`./sscale -f`) and `npm run dev` in `web/`
(Vite proxies `/api` to the daemon on port 8090).

Layout: `internal/hagrid` protocol · `internal/bluez` BlueZ D-Bus ·
`internal/scale` session · `internal/daemon` scan/connect policy ·
`internal/app` service layer · `internal/db` SQLite · `internal/trend`
filter · `internal/api` REST · `internal/webui` embedded app · `web/` React +
shadcn/ui PWA · `cmd/sscale` CLI.

## Troubleshooting

* **Scale never seen** — it only advertises after you step on it; check range with `./sscale scan --all`.
* **`authentication failed`** — `cak`/`c1`/`c2`/`address` don't match this scale.
* **`An AppArmor policy prevents this sender…`** — missing `apparmor=unconfined`.
* **No body composition / "no impedance measured"** — stand barefoot until the scale finishes; also needs height, birthdate, sex and age ≥ 18.
* **Web app asks for the key again** — the key was rotated; get it with `./sscale apikey`.

## License note

The protocol handling and body-composition coefficients are ported from
openScale (GPL-3.0-or-later); treat this project accordingly.
