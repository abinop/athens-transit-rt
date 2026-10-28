# athens-transit contract v1

Shared interface between the backend (`athens-transit-rt`, Go, on a home machine)
and the frontend (`athens-transit-map`, Svelte, on GitHub Pages).
Source of truth: `athens-transit-rt/docs/CONTRACT.md`. The frontend keeps a copy.

## Ownership rules

- The backend owns this contract. v1 changes are **additive only** (new optional
  fields, new endpoints). A breaking change needs a `/v2` path.
- The frontend never edits the backend repo. It asks for a change with a GitHub
  issue on `angelospk/athens-transit-rt`, label `contract`.
- Each change to this file bumps the `Revision` line below and is copied to the
  frontend repo by whoever made it (backend thread opens the copy as a commit or
  issue on the frontend repo).

Revision: 7 (2026-10-28): `path_speeds` also follow today's traffic (same shape and units)
(rev 6: `path_speeds` on vehicles (typical speeds ahead); rev 5: `GET /v1/vehicles/tiles/{z}/{x}/{y}`; rev 4: `path_beyond` and `path_stops` on vehicles; rev 3: `GET /v1/vehicles`; `speed` and
`path` on vehicles)

## Hosts

| Host | Served by | Content |
|---|---|---|
| `https://transit.haroldpoi.dev` | Home machine → Cloudflare Tunnel → Cloudflare cache | Live data (small, changes every ~30 s) |
| `https://angelospk.github.io/athens-transit-rt/static/v1/` | GitHub Pages of the backend repo | Static map data (changes only when OASA publishes a new GTFS) |
| `https://raw.githubusercontent.com/angelospk/athens-transit-rt/stats/v1/` | `stats` branch of the backend repo, copied daily from `/v1/stats` | Daily network statistics ([API.md](API.md#daily-statistics)) |
| `https://bus.haroldpoi.dev` | GitHub Pages of the frontend repo | The map app |

All JSON is UTF-8. Line ids are the OASA telematics `LineID` in Greek uppercase
(`040`, `550`, `Α1`, `Χ96`), percent-encoded in URLs. The backend also accepts the
Latin look-alikes (`A1` → `Α1`). Times are Unix seconds (integers).
CORS: every live and static response has `Access-Control-Allow-Origin: *`.

## Live API (`transit.haroldpoi.dev`)

### `GET /v1/lines/{line_id}`

Vehicles of one line. One URL per line so all users share one cached copy.
The frontend combines lines in the browser. Max 5 lines per client (enforced in UI).

```json
{
  "line": "040",
  "updated_at": 1791100000,
  "next_update_at": 1791100030,
  "vehicles": [
    {
      "id": "52134",
      "lat": 37.9755, "lon": 23.7348,
      "bearing": 180,
      "position_at": 1791099988,
      "route_code": "2045",
      "variant": "2045",
      "trip_id": "12345-WINTER",
      "trip_label": "00:35 ΠΕΙΡΑΙΑΣ → ΣΥΝΤΑΓΜΑ",
      "delay_s": 140,
      "next_stop_id": "400012",
      "speed": 6.4,
      "path": [[37.9755, 23.7348], [37.97601, 23.73502], [37.9781, 23.7356]],
      "path_beyond": [[37.9781, 23.7356], [37.98145, 23.73668], [37.98466, 23.73771]],
      "path_stops": [298, 682, 1050],
      "path_speeds": [6.2, 3.1, 4.8, 5.5, null, 7.0, 6.1, 5.9]
    }
  ]
}
```

- `trip_id`, `trip_label`, `delay_s`, `next_stop_id` are `null` when the vehicle is
  not matched to a scheduled trip. `bearing` may be `null`.
- `variant` keys into `static/v1/lines/{line_id}.json` → `variants` (for shape + stops).
  It may be `null` if no static variant matches.
- `speed` (rev 3): smoothed speed along the route in m/s, one decimal, 0..20. It is the
  median slope over the vehicle's recent fixes (up to 5, within 300 s) as distance along
  its shape; moving less than 20 m over them gives `0` (standing). `null` when unknown:
  fewer than 2 fixes ≥ 20 s apart on the same shape (first sighting, new run, long gap
  between polls), position on the shape ambiguous (a loop or out-and-back passes there
  twice and history cannot tell which), more than 150 m from the shape, no shape.
- `path` (rev 3): the route ahead as `[lat, lon]` pairs (5 decimals), from the vehicle's
  projected point on its shape (at `position_at`) to its next stop when matched,
  otherwise `max(speed × 150 s, 300 m)` ahead; at most 1.5 km and never past the
  shape's end. Simplified (5 m), so a straight street has 2 points. `null` when there is
  nothing to move along: no shape, > 150 m from it, ambiguous position, or already at the
  next stop / the shape's end. Use: move the marker along `path` at `speed` from
  `position_at`, stop at the last point and wait for the next update. The static
  `shape` is not needed for this.
- `path_beyond` (rev 4): matched vehicles only, the route on from the end of `path` (from
  the vehicle's point when `path` is `null` because it is at its next stop) to its third
  stop ahead; at most 1.5 km from the vehicle and never past the shape's end. Same format
  as `path`; its first point is the last point of `path`. `null` when unmatched, ambiguous,
  waiting to start, past the last stop, or when `path` already reaches the 1.5 km limit.
- `path_stops` (rev 4): the stops on `path` followed by `path_beyond`, as whole metres along
  those points (joined, the shared point once), ascending; the next stop is the end of
  `path`. `null` when not matched or no stop is on them. Use (rev 4 clients): drive
  `path` then `path_beyond` from `position_at`, slowing down and waiting a little at each
  of `path_stops`.
- `path_speeds` (rev 6): matched vehicles only, the typical speed ahead in m/s (one decimal,
  at least 0.1) for consecutive 200 m steps from the vehicle's point along its shape: step `i`
  covers metres `200·i` to `200·(i+1)` ahead, the last one may be shorter (shape's end), at
  most 8 steps (1.6 km). It comes from the last 14 days of fixes of vehicles on the same shape,
  per 200 m of the shape, day type (weekday, Saturday, Sunday) and local hour, falling back to
  all hours; waits at stops and lights are included. Rev 7: where buses of any line crossed
  the same road in the same direction in the last 20 minutes (at least 2 buses, per 100 m of
  road), that road counts at 0.7 × their speed + 0.3 × the typical one (their speed alone
  without history). A step with neither is `null`. `null` when unmatched, waiting to start,
  or nothing ahead is known. Clients need no change. Use (rev 6 clients): move along `path` then `path_beyond` from `position_at` at
  the step speeds (a `null` step at `speed`, or 1.5 m/s without one), instead of the stop
  waits; stop at the end of the points, as before. Metres are along the shape; the 5 m
  simplified points are a few metres shorter, so a client may simply walk them.
- Headers: `Cache-Control: public, max-age=<seconds until next_update_at, min 5, max 300>`.
  The frontend refetches a line at `next_update_at + 1..3 s` (random jitter), never faster
  than every 5 s, and at most every 60 s while the tab is hidden.
- Asking for a line marks it "watched" for ~10 min: the backend polls it at the top
  priority tier (~30 s), within a capped share of the OASA request budget.
- `404` `{"error":"unknown_line"}` for an unknown line. `503` `{"error":"warming_up"}`
  before the first poll of that line finished.
- `429` may come from Cloudflare rate limiting (per IP). The frontend backs off ≥ 10 s.

### `GET /v1/vehicles` (rev 3)

Every vehicle the backend knows now, from all lines, for a citywide map. Data comes from
the normal polling (watched lines ~30 s, busy ~60 s, others ~150 s); this endpoint does
**not** mark any line watched and causes no OASA request, so positions here can be up
to ~2.5 min staler than `/v1/lines/{id}` for lines nobody watches.

```json
{
  "updated_at": 1791100000,
  "next_update_at": 1791100030,
  "vehicles": [
    {
      "line": "040",
      "id": "52134",
      "lat": 37.9755, "lon": 23.7348,
      "bearing": 180,
      "position_at": 1791099988,
      "variant": "2045",
      "delay_s": 140,
      "speed": 6.4,
      "path": [[37.9755, 23.7348], [37.9781, 23.7356]],
      "path_beyond": [[37.9781, 23.7356], [37.98466, 23.73771]],
      "path_stops": [297, 1049],
      "path_speeds": [6.2, 3.1, 4.8, null]
    }
  ]
}
```

- Same field meanings as `/v1/lines/{id}`; `line` is the line id. Coordinates are rounded
  to 5 decimals. No `route_code`, `trip_id`, `trip_label`, `next_stop_id`: fetch
  `/v1/lines/{line}` when the user selects a vehicle.
- Vehicles with a GPS fix older than 5 min are left out. A vehicle seen on two lines
  appears once (newest fix). Sorted by `line`, then `id`. `vehicles` may be `[]`.
- One snapshot for everyone, built at most every 30 s (`updated_at` = build time,
  `next_update_at` = earliest next build). Refetch at `next_update_at + 1..3 s`.
- Headers: `Cache-Control: public, max-age=<seconds until next_update_at, min 5, max 30>`
  (the 5 s floor can keep a copy up to 5 s past `next_update_at`). `ETag`;
  `If-None-Match` gives `304`. In a browser, let the HTTP cache revalidate (plain
  `fetch`); do not set `If-None-Match` from JS: it is not a CORS-safelisted header and
  there is no `OPTIONS` preflight support. gzip when asked (~50 KB for 1500 vehicles).

### `GET /v1/vehicles/tiles/{z}/{x}/{y}` (rev 5)

The `/v1/vehicles` snapshot cut into map tiles, so a client loads only what its map view shows.
Tiles are XYZ (Web Mercator, "slippy map") tiles, the scheme of OSM/MapLibre raster tiles. Each
tile is one fixed URL shared by all clients, so Cloudflare caches it like `/v1/vehicles`.
Example: `/v1/vehicles/tiles/13/4636/3160` (around Syntagma).

| `z` | tile size (Athens) | content | use at MapLibre zoom |
|---|---|---|---|
| `9` | ~62 km | vehicles without `path`, `path_beyond`, `path_stops`, `path_speeds` (the keys are absent) | < 13 |
| `13` | ~3.9 km | the same vehicle entries as `/v1/vehicles` | ≥ 13 |

```json
{
  "updated_at": 1791187860,
  "next_update_at": 1791187890,
  "vehicles": [
    {
      "line": "040", "id": "44537",
      "lat": 37.95614, "lon": 23.71609,
      "bearing": 52, "position_at": 1791187838,
      "variant": "5512", "delay_s": -35, "speed": 0
    }
  ]
}
```

- Body: the shape of `/v1/vehicles` (same fields, order and rules), with only the vehicles whose
  `lat`/`lon` is in that tile. A vehicle is in exactly one tile per zoom; its paths may run on
  into other tiles. `z` 13 entries are byte-identical to `/v1/vehicles`; `z` 9 entries have
  `line, id, lat, lon, bearing, position_at, variant, delay_s, speed` only.
- Same snapshot as `/v1/vehicles`: one build, at most every 30 s, gives `/v1/vehicles` and all
  tiles the same `updated_at` and `next_update_at`. No OASA request, no line marked watched.
- Tiles exist only for the box lat 37.5..38.5, lon 22.9..24.5: `z` 9 x 288..290, y 196..198;
  `z` 13 x 4617..4653, y 3145..3174. A tile in the box with no vehicle → `200` with
  `"vehicles":[]`. A vehicle is in a tile only when its `z` 13 tile is one of these (then its `z` 9
tile is too); one farther out is in no tile (none seen so far).
- Tile of a point (the usual slippy-map formula, `n = 2^z`):
  `x = floor((lon + 180) / 360 × n)`,
  `y = floor((1 − asinh(tan(lat × π/180)) / π) / 2 × n)`.
- `404` `{"error":"not_found"}` (`max-age=60`) for any other `z`, `x`/`y` outside the box, numbers
  not in plain decimal (`04636`, `+4636`), percent-encoded digits, or a trailing `/`. `400`
  `{"error":"query_not_allowed"}` (`no-store`) for any query string, even a bare `?`: each
  variant would be another Cloudflare cache entry. Never add cache busters.
- Headers as `/v1/vehicles`: `Cache-Control: public, max-age=<seconds until next_update_at,
  min 5, max 30>`, `ETag`, `If-None-Match` → `304`, gzip when asked, `Vary: Accept-Encoding`.
  Let the browser HTTP cache handle them (plain `fetch`); a tile asked again within its max-age
  then costs no request.

Client rules:

1. Tile set: take the map bounds, pad by 300 m (≈ 30 s of driving, so a vehicle that will drive
   into view is loaded), clamp to the box, choose `z` = 13 at MapLibre zoom ≥ 13 else 9, and take
   every tile from the north-west corner's tile to the south-east corner's tile.
2. On `moveend` (fires once the user stops panning or zooming): recompute the set, fetch tiles
   not loaded, drop vehicles of tiles that left the set. When `z` changes, replace the whole set.
3. Refresh all tiles of the set at `max(min(next_update_at) + 1..3 s, last refresh + 5 s)` (a
   cached copy can carry a `next_update_at` up to 5 s in the past), and at most every 60 s
   while the tab is hidden.
4. Merge all tiles by vehicle `id`: newer `position_at` wins, on a tie the response with the
   newer `updated_at`. Caches can mix two builds for a few seconds, so a vehicle that just
   crossed a tile edge can be in two tiles, or in none until the next refresh.
5. Fetch `/v1/lines/{line}` for `route_code`, `trip_id`, `trip_label`, `next_stop_id` when the
   user selects a vehicle, as with `/v1/vehicles`.
6. Cloudflare rate-limits per IP (`429`, back off ≥ 10 s). A desktop view at MapLibre 13 is ~15
   tiles; do not refetch tiles that are still fresh.

### `GET /v1/status`

```json
{ "ok": true, "gtfs_version": "2026-10-06", "gtfs_expires": "2027-01-06",
  "updated_at": 1791100000, "lines_active": 312, "budget_rps": 4 }
```

`Cache-Control: public, max-age=10`. The frontend shows a banner if `ok` is false or
`gtfs_expires` is in the past.

### GTFS-Realtime feeds (whole network, for journey planners)

- `GET /v1/gtfs-rt/vehicle_positions.pb`
- `GET /v1/gtfs-rt/trip_updates.pb`
- `.json` variants of both, for debugging.

`Cache-Control: public, max-age=15`. The frontend does not use these.

## Static data (`.../athens-transit-rt/static/v1/`)

Published by GitHub Actions in the backend repo when OASA publishes a new GTFS.

### `lines.json`

```json
{
  "gtfs_version": "2026-10-06",
  "lines": [
    { "id": "040", "name": "ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ", "name_en": "PIRAEUS - SYNTAGMA",
      "color": "#1a73e8", "text_color": "#ffffff", "kind": "bus" }
  ]
}
```

`kind` is `bus` or `trolley`.

### `lines/{line_id}.json`

```json
{
  "id": "040",
  "variants": {
    "2045": {
      "headsign": "ΣΥΝΤΑΓΜΑ",
      "direction": 0,
      "shape": [[37.9420, 23.6466], [37.9431, 23.6480]],
      "stops": ["400012", "400013"]
    }
  },
  "stops": {
    "400012": { "name": "ΠΛ. ΚΑΡΑΪΣΚΑΚΗ", "lat": 37.9420, "lon": 23.6466 }
  }
}
```

`shape` is `[lat, lon]` pairs, rounded to 5 decimals. `variants[*].stops` is ordered.

## Fixtures

`docs/fixtures/` in the backend repo holds one example of every response above
(`line-040.json`, `vehicles.json`, `vehicles-tile-13-4636-3160.json`,
`vehicles-tile-9-289-197.json`, `status.json`, `lines.json`, `lines-040.json`). The frontend
develops against copies of these until the live API is up.
