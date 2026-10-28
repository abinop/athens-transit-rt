# Changelog

The public instance (`https://transit.haroldpoi.dev`) runs `main`; a push to `main` deploys it.
Version tags (`v*`) build an image with that version (`ghcr.io/angelospk/athens-transit-rt:<version>`)
for self-hosting. API reference: [docs/API.md](docs/API.md), OpenAPI 3.1:
[docs/openapi/openapi.yaml](docs/openapi/openapi.yaml), viewer with try-it:
<https://angelospk.github.io/athens-transit-rt/>. Contract for clients: [docs/CONTRACT.md](docs/CONTRACT.md).

## Unreleased

### Live API
- `GET /v1/stops/{stop_id}/arrivals`: OASA's own predictions for a stop (line, route code,
  vehicle, minutes), soonest first, cached 30 s per stop with `Cache-Control` until that cache
  expires. The OASA calls take at most one request slot in four, so the line polls keep at least
  three quarters of the budget; when OASA or the queue cannot answer, the last good answer up to
  2 minutes old is served. Shares its cache with the MCP tool `stop_arrivals`.

## v0.1.0 (2026-10-28)

First tagged version. Everything below is live on the public instance.

### Live API
- Every active OASA route polled under one global budget (4–5 req/s), with backoff on errors and
  slow answers; background jobs report only their errors to the pacer, so a slow prefetch no longer
  holds the pace down.
- Vehicles matched to scheduled trips, with smoothed `speed`, the `path` ahead, `path_stops` and
  `path_speeds` (contract rev 7).
- `path_speeds` follow today's traffic: recent passes of every line per 100 m piece of road and
  heading, blended into the typical speed of the hour (`segspeed`).
- Vehicle tiles for whole-network maps (`/v1/vehicles/tiles/{z}/{x}/{y}`), GTFS-Realtime
  VehiclePositions and TripUpdates.

### Timetables
- `GET /v1/lines/{id}/schedule`: today from OASA's daily schedule (`getDailySchedule`,
  `source: oasa-daily`), the real timetable even while the static GTFS is expired; other days and
  fallback from the GTFS (`source: gtfs`, `expired` when outside its validity).
- Every line's daily schedule is prefetched once a day through the scheduler's background slots.
- `seen` per run for the daily schedule too (new): the daily schedule has no trip ids, so the
  server watches buses leave the start of their shape (within 300 m, then 600 m along within
  15 min) and gives each start to the run due from 3 min before to 20 min after it. `false` once
  20 min pass without one. Loop lines, late or out-of-order fixes and a feed swap are handled.

### Rail
- Metro and tram: `GET /v1/rail/stasy/departures?stop=<name>` (STASY GTFS timetable, not live).
- Suburban rail (Proastiakos): `GET /v1/rail/suburban/stations` and
  `/v1/rail/suburban/stations/{id}/departures` (live departures and delays).

### Statistics
- `/v1/stats` and `/v1/stats/days/{date}`: trips planned, run, partial, missed per line; long-term
  views on the web map.

### MCP
- `/mcp`: a read-only MCP server (Streamable HTTP) with tools for lines, buses, stops, timetables,
  statistics, metro and suburban rail. One OASA call per stop, bounded queue, read deadlines.

### Clients
- Web map: <https://bus.haroldpoi.dev> ([athens-transit-map](https://github.com/angelospk/athens-transit-map)).
- Android app: [athens-transit-app](https://github.com/angelospk/athens-transit-app) (signed APKs on
  its releases page).
