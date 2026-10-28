# athens-transit-rt

Live positions and GTFS-Realtime for OASA buses and trolleys in Athens, in Go.

**Public API, no key:** `https://transit.haroldpoi.dev`. **API reference (try it in the browser):**
<https://angelospk.github.io/athens-transit-rt/> (short link: [bus.haroldpoi.dev/api](https://bus.haroldpoi.dev/api);
OpenAPI 3.1: [`docs/openapi/openapi.yaml`](docs/openapi/openapi.yaml)).

This is a Go port of [foivospro/athens-gtfs-realtime](https://github.com/foivospro/athens-gtfs-realtime)
by Foivos Proestakis (MIT). The matching logic, constants and tools follow upstream; the last
ported upstream commit is in [`UPSTREAM.md`](UPSTREAM.md). Upstream's HTML viewer and `/events`
stream are not ported: the map lives in [athens-transit-map](https://github.com/angelospk/athens-transit-map).

> **Ελληνικά, σύντομα.** Ένα Go binary (`atrt`) ρωτά το API τηλεματικής του ΟΑΣΑ για όλο το
> δίκτυο, με ένα κοινό όριο αιτημάτων (4/s, max 5), αντιστοιχίζει κάθε όχημα σε δρομολόγιο του
> GTFS και δίνει JSON ανά γραμμή (`/v1/lines/040`) και GTFS-Realtime. Οι χρήστες δεν φτάνουν
> ποτέ στον ΟΑΣΑ: το Cloudflare κρατά κάθε απάντηση ακριβώς μέχρι την επόμενη ανανέωση της
> γραμμής. Η βαριά δουλειά (το GTFS των 225 MB) γίνεται στο GitHub Actions. Ο server κατεβάζει
> μόνο ένα έτοιμο snapshot.

## What it does

- Loads the static GTFS as a compact snapshot and swaps in new ones without a restart.
- Polls `getBusLocation` for every route with scheduled service now, under one global request
  budget (default 4 req/s, max 5), evenly spaced, with backoff on errors and slow answers.
- Maps OASA route codes to GTFS shapes by stop sequence, projects GPS fixes on the shapes and
  matches vehicles to trips one-to-one (Hungarian assignment plus upstream's memory matcher,
  with hysteresis). `internal/lsa` is a port of scipy's `linear_sum_assignment`.
- Keeps the last 5 fixes of each vehicle as distance along its shape (forgotten after 10 min)
  and derives a smoothed `speed` and the `path` ahead, so a map can move buses between updates.
- Serves the live API of [`docs/CONTRACT.md`](docs/CONTRACT.md) and whole-network
  GTFS-Realtime VehiclePositions and TripUpdates (`.pb` and `.json`). Public instance and
  usage: [API reference](https://angelospk.github.io/athens-transit-rt/),
  [`docs/API.md`](docs/API.md). The feeds pass the MobilityData GTFS Realtime validator
  with 0 errors ([details](docs/API.md#validation)).
- Line timetables (`/v1/lines/{id}/schedule`): today from OASA's daily schedule, prefetched once a
  day for every line, with `seen` per run from the buses seen leaving their start; other days from
  the GTFS.
- Live road speeds: the recent passes of every line per 100 m piece of road, blended into each
  bus's `path_speeds`, so a line's ETA follows today's traffic on shared roads.
- Metro and tram departures from the STASY GTFS, and suburban rail (Proastiakos) live departures
  and delays.
- Trains for a map: metro and tram placed from the STASY timetable (between two stops, with their
  times), and the Proastiakos trains with their GPS positions from the railway.gov.gr tracker.
- An MCP server at `/mcp` (Streamable HTTP, read-only) so assistants can ask about lines, buses,
  stops, timetables, statistics and rail ([details](docs/API.md)).
- Tools from upstream as subcommands: `compare`, `record` (SQLite, upstream schema), `replay`
  (including `--blocks`).

## Use the public instance

Base URL `https://transit.haroldpoi.dev`, no key. Reference with try-it:
<https://angelospk.github.io/athens-transit-rt/>, details in [`docs/API.md`](docs/API.md).

```bash
curl -s https://transit.haroldpoi.dev/v1/lines/040                # live buses of a line
curl -s https://transit.haroldpoi.dev/v1/lines/040/schedule       # today's timetable, seen per run
curl -s -G https://transit.haroldpoi.dev/v1/rail/stasy/departures \
  --data-urlencode stop=ΣΥΝΤΑΓΜΑ -d n=4                         # metro/tram (stop name URL-encoded)
curl -s https://transit.haroldpoi.dev/v1/rail/suburban/stations   # suburban rail stations
curl -s https://transit.haroldpoi.dev/v1/rail/suburban/vehicles   # suburban trains, GPS
```

Please ask for at most 5 lines at a time and keep the caching headers: every client shares one
request budget towards OASA.

### MCP (Claude, ChatGPT and other assistants)

URL: `https://transit.haroldpoi.dev/mcp` (Streamable HTTP, no login, read-only). Tools: `search_lines`,
`line_now`, `line_timetable`, `find_stops`, `stop_arrivals`, `network_day`, `metro_departures`,
`suburban_departures` ([what each returns](docs/API.md#mcp-assistants)).

- Claude Code: `claude mcp add --transport http athens-transit https://transit.haroldpoi.dev/mcp`
- Claude (claude.ai, desktop): Settings › Connectors › Add custom connector, paste the URL.
- ChatGPT: turn on developer mode, add a connector (MCP server) with the URL and no authentication.
- Any client: check that it answers with

  ```bash
  curl -s https://transit.haroldpoi.dev/mcp -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
  ```

Then ask, for example, «Πού είναι τώρα τα λεωφορεία του 040;» or «Πότε φεύγει το επόμενο μετρό από
το Σύνταγμα;».

## Architecture

```
data.gov.gr GTFS zip ──(GitHub Actions, every 6 h, only on change)──► atrt snapshot
      │                                                     ├─► Release "gtfs-snapshot": snapshot.bin + manifest.json
      │                                                     └─► GitHub Pages: static/v1/lines.json, lines/{id}.json
      ▼
home machine (Docker): atrt serve ── downloads snapshot (sha256-checked), polls OASA ──► 127.0.0.1:8095
      └─► cloudflared tunnel ─► Cloudflare cache ─► https://transit.haroldpoi.dev/v1/...
```

### Polling tiers

| Tier | Which lines | Target interval |
|---|---|---|
| watched | requested through `/v1/lines/{id}` in the last 10 min | 30 s, at most 50% of the budget |
| dense | ≥ 5 fresh vehicles in the last poll | 60 s |
| other | scheduled service now | 150 s (stretched up to 5 min under load) |
| inactive | no scheduled service now | not polled |

A line that becomes watched and was not polled in the last 10 s is polled at once, before
every other due line (still within the watched share), so its first fresh answer comes a few
seconds after the first request. Later requests of a watched line do not force extra polls.
Routes that came back empty twice are re-polled only every 5 min (except for watched lines).
Metadata refreshes (lines, routes, stops) get every 4th request slot while they are queued.
When a tier needs more than its share, its intervals stretch. A line response carries
`Cache-Control: max-age` = seconds until its next expected poll (5..300), so the Cloudflare
copy expires when fresh data exists.

`/v1/vehicles` (all lines at once) reads only what the tiers above already polled: it never
marks a line watched. Its body is built at most every 30 s, on the first request after the
previous snapshot expired, and gzip-compressed once; every client gets the same bytes.
The same build cuts it into map tiles (`/v1/vehicles/tiles/{z}/{x}/{y}`, `z` 9 without paths,
`z` 13 with them), each encoded once too: a map client loads only its view, and the URLs are a
fixed set (~1,100 tiles over Attica), so the origin load behind the cache does not grow with
clients. Measured on a recorded rush-hour snapshot (922 vehicles): build 11.5 → 23 ms per 30 s,
held bytes 355 → 875 KB; [spec](docs/superpowers/specs/2026-10-08-vehicle-tiles.md).

`/v1/status` reports `ok: false` when no line poll succeeded in the last 5 minutes (while lines
are scheduled). During an OASA outage the last good data stays served; `updated_at` shows its age.

When the static GTFS has expired, line activity is planned from the same weekday in the feed's
last valid week, so vehicles still appear (position and route, `trip_id: null`). A warning is
logged daily from 7 days before expiry.

### Snapshot format

`snapshot.bin` = gzip("ATRTSNAP" + format version byte + gob(`gtfs.Feed`)). Stop times are
deduplicated into stop patterns and time profiles (integer ids, seconds after the service
day start). `manifest.json` holds the GTFS version, source ETag/Last-Modified/size and the
snapshot sha256. The server rejects a download whose sha256 or format does not match and keeps
the old snapshot.

## Commands

```bash
go build -o atrt ./cmd/atrt

# Live service. Downloads the snapshot from this repo's release into --state.
atrt serve --listen 127.0.0.1:8095 --metrics-listen 127.0.0.1:8096 --state /var/lib/atrt --rps 4
atrt serve --gtfs osy_gtfs.zip --state ./state     # local, from a GTFS zip
atrt serve ... --history --history-max-mb 3000     # also keep every GPS fix (30 days) in --state/history

# Snapshot + static map data (what the gtfs workflow runs).
atrt snapshot --gtfs osy_gtfs.zip --out dist --site site

# Upstream tools (dev machine, 2 req/s by default).
atrt compare --lines 040,550
atrt record  --lines 040,A1 --end 2026-10-05T10:00 --db data/record.sqlite
atrt replay  data/record.sqlite            # score the matchers
atrt replay  --blocks data/record.sqlite   # check GTFS vehicle blocks
```

`atrt <command> -h` lists all flags. `scripts/parity/upstream_match.py` runs the upstream
Python matchers on recorded cycles; `internal/match/parity_test.go` checks that the Go
matchers give the same assignments.

### Local metrics

`GET http://127.0.0.1:8096/metrics` (loopback only, not behind the tunnel) returns JSON: OASA
requests per second for the last hour, the maximum over any 1 s and 10 s window, requests by
tier, lines by tier, per-line interval and last poll, error count and RSS. Use it to check the
budget:

```bash
curl -s 127.0.0.1:8096/metrics | jq '{max_1s, max_10s, sent_by_tier, lines_by_tier, rss_mb: (.rss_bytes/1048576)}'
```

## Self-hosting (Docker)

**OASA answers only IP addresses in Greece.** Requests from elsewhere time out (tested:
Oracle Cloud, Google Apps Script, Cloudflare WARP and Workers outside Athens, Webshare proxies,
Hetzner, netcup, Cyprus and Bulgaria, even home connections there). Data-centre addresses in
Greece work: on 2026-10-28, 10 of 10 Globalping probes in Greece got answers, 7 of them in data
centres (Synapsecom, MVPS, Novelcomm, Fusioned, Euclid Services, Datacamp), and an MVPS VPS in
Thessaloniki (1 vCPU, 2 GB) runs the container below. Run it on a machine at home in Greece or
on a VPS whose address is in Greece; check first with
`curl -m 20 "https://telematics.oasa.gr/api/?act=getBusLocation&p1=2045"`, which returns
JSON when OASA answers.

```bash
curl -O https://raw.githubusercontent.com/angelospk/athens-transit-rt/main/compose.yaml
docker compose up -d
curl -s 127.0.0.1:8095/v1/status
```

Without compose: `docker run -d -p 127.0.0.1:8095:8095 -v atrt-state:/var/lib/atrt
ghcr.io/angelospk/athens-transit-rt` (the metrics endpoint then stays inside the container).

The image is `ghcr.io/angelospk/athens-transit-rt` (linux/amd64 and arm64, so a Raspberry Pi
works). It downloads the GTFS snapshot from this repo's release on first start and keeps it
in the `atrt-state` volume. The API listens on `127.0.0.1:8095`; publishing it (Cloudflare
Tunnel, Caddy, nginx, ...) and its cache and rate-limit rules are up to you. Keep the OASA
budget at 4 req/s or lower (`--rps`), and do not run two instances behind one address.

## Deployment

The public instance runs the Docker image from [Self-hosting](#self-hosting-docker) on a
machine at home (OASA does not answer data-centre addresses), with `mem_limit: 200m` and
`restart: unless-stopped`.

`deploy/install.sh <ssh-host>` and `deploy/atrt.service` install the plain binary under
systemd instead (system user `atrt`, state in `/var/lib/atrt`, `Restart=always` with at most
10 starts per 10 min, `GOMEMLIMIT=100MiB`, `MemoryHigh=150M`, `MemoryMax=200M`, sandboxing).
Use it only on a host that can reach OASA. Both listeners are on 127.0.0.1 only.

### Cloudflare

- Tunnel ingress in the cloudflared config:
  `transit.haroldpoi.dev → http://127.0.0.1:<port>`, DNS via
  `cloudflared tunnel route dns <tunnel> transit.haroldpoi.dev`.
- Cache rule (Caching → Cache Rules): when hostname is `transit.haroldpoi.dev` and URI path
  starts with `/v1/` → *Eligible for cache*, Edge TTL *Use cache-control header if present*,
  Browser TTL *Respect origin*. Without it Cloudflare does not cache JSON (`cf-cache-status: DYNAMIC`).
- Rate limiting rule (Security → WAF → Rate limiting): URI path starts with `/v1/` (the
  free plan offers no hostname field), per IP, 30 requests / 10 s, block for 10 s. It also
  counts cache hits (the free plan cannot exclude them). Measured: blocking starts after
  roughly 30-60 requests, since Cloudflare counts approximately; the 429 comes from Cloudflare.

### GitHub Actions

| Workflow | When | What |
|---|---|---|
| `ci` | push, PR | gofmt, `go vet`, `go test -race`, build; lint the OpenAPI spec |
| `gtfs` | every 6 h at :17, manual, push to `docs/openapi/` | HEAD data.gov.gr; if ETag/Last-Modified/size changed (or forced), build the snapshot, upload release assets, deploy Pages; on a docs push, deploy Pages with the same static data |
| `upstream-watch` | daily | compare `UPSTREAM.md` with upstream `main`; open one issue per new upstream head |

Static data: <https://angelospk.github.io/athens-transit-rt/static/v1/lines.json>.
API docs: the same Pages site serves `docs/openapi/` at its root. Check the spec against the
live servers with `uv run scripts/check-openapi-live.py` (every endpoint, strict schemas), and
lint it with `npx @redocly/cli lint docs/openapi/openapi.yaml`.

## Development

```bash
go test -race ./...          # includes a fake-OASA outage test (~20 s); -short skips it
```

Tests never call the live OASA API; recorded answers are in `internal/*/testdata`.

## License

Code: MIT, see [`LICENSE`](LICENSE). Upstream: Copyright (c) 2026 Foivos Proestakis, MIT.
Data: derived from OASA open data (the static GTFS on
[data.gov.gr](https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057) and the OASA
telematics positions); use it under the terms of those sources.
