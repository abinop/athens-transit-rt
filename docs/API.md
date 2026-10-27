# athens-transit-rt API

Live positions and GTFS-Realtime for OASA buses and trolleys in Athens. Free to use, no key.
This page is for external users. Interactive reference: <https://angelospk.github.io/athens-transit-rt/>
(source: [`openapi/openapi.yaml`](openapi/openapi.yaml)). Full field rules: [`CONTRACT.md`](CONTRACT.md).

> **Ελληνικά, σύντομα.** Δημόσιο API χωρίς κλειδί. GTFS-Realtime για όλο το δίκτυο στο
> `/v1/gtfs-rt/`, JSON ανά γραμμή στο `/v1/lines/{γραμμή}`. Σεβαστείτε το caching: τα δεδομένα
> ανανεώνονται κάθε 30-150 s, άρα πιο συχνά αιτήματα δεν φέρνουν νέα δεδομένα.

Base URL: `https://transit.haroldpoi.dev`

The instance runs on one home machine (OASA does not answer data-centre addresses), behind
the Cloudflare cache. There is no uptime guarantee. For anything important, run your own
instance (see [Self-hosting](../README.md#self-hosting-docker)).

## Endpoints

The list of endpoints, their parameters and response schemas is the OpenAPI 3.1 spec
[`openapi/openapi.yaml`](openapi/openapi.yaml), with an interactive reference ("try it") at
<https://angelospk.github.io/athens-transit-rt/>. This page has usage notes and examples.

Live: `/v1/lines/{line_id}`, `/v1/vehicles`, `/v1/vehicles/tiles/{z}/{x}/{y}`,
`/v1/gtfs-rt/{vehicle_positions,trip_updates}.{pb,json}`, `/v1/status`, `/v1/stats`,
`/v1/stats/days/{YYYY-MM-DD}`, `/healthz`. Static data (line list, shapes, stops) is on GitHub Pages:
`https://angelospk.github.io/athens-transit-rt/static/v1/lines.json` and
`.../static/v1/lines/{line_id}.json`.

All responses are UTF-8, send `Access-Control-Allow-Origin: *`, and are gzip-compressed when
the request allows it. Times are Unix seconds.

## Examples

```bash
# Health and GTFS version
curl -s https://transit.haroldpoi.dev/v1/status
# {"ok":true,"gtfs_version":"2026-07-08","gtfs_expires":"2026-10-06","updated_at":1791229867,"lines_active":236,"budget_rps":4}

# Vehicles of line 040
curl -s https://transit.haroldpoi.dev/v1/lines/040

# All vehicles, with speed (m/s) and the route ahead
curl -s --compressed https://transit.haroldpoi.dev/v1/vehicles | jq '.vehicles | length'

# Vehicles around Syntagma only (one ~4 km map tile)
curl -s --compressed https://transit.haroldpoi.dev/v1/vehicles/tiles/13/4636/3160 | jq '.vehicles | length'

# Whole-network feeds
curl -s --compressed -o vehicle_positions.pb https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb
curl -s --compressed https://transit.haroldpoi.dev/v1/gtfs-rt/trip_updates.json | jq '.entity | length'
```

Python:

```python
import requests
from google.transit import gtfs_realtime_pb2  # pip install gtfs-realtime-bindings

feed = gtfs_realtime_pb2.FeedMessage()
feed.ParseFromString(requests.get("https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb").content)
for e in feed.entity[:5]:
    v = e.vehicle
    print(v.vehicle.id, v.trip.route_id, v.trip.trip_id, v.position.latitude, v.position.longitude)
```

## Per-line JSON

`line_id` is the OASA line number in Greek uppercase (`040`, `550`, `Α1`, `Χ96`),
percent-encoded. Latin look-alikes also work (`A1` → `Α1`).

```json
{
  "line": "040",
  "updated_at": 1791229815,
  "next_update_at": 1791229873,
  "vehicles": [
    {
      "id": "44537",
      "lat": 37.974551, "lon": 23.734165,
      "bearing": 41,
      "position_at": 1791229792,
      "route_code": "3923",
      "variant": "5513",
      "trip_id": "938_day_1_1303_2300",
      "trip_label": "23:00 ΣΥΝΤΑΓΜΑ → ΠΕΙΡΑΙΑΣ",
      "delay_s": 0,
      "next_stop_id": "10341"
    }
  ]
}
```

- `trip_id`, `trip_label`, `delay_s` and `next_stop_id` are `null` when the vehicle is not
  matched to a scheduled trip. `bearing` and `variant` may be `null`.
- Refetch at `next_update_at`, not earlier: until then the answer does not change.
- Asking for a line raises its polling priority for about 10 minutes (about every 30 s). If
  nobody asked for it recently, it is polled at once: `next_update_at` is then a few seconds
  away.
- `speed` is the speed along the route in m/s (0..20, `0` = standing, `null` = unknown).
  `path` is the route ahead as `[lat, lon]` pairs, from the vehicle's point on its shape to
  the next stop (or 300 m-1.5 km ahead), or `null`. `path_beyond` continues it to the third
  stop ahead, and `path_stops` gives the stops on both as metres along them (or `null`).
  `path_speeds` (contract rev 6) is the typical speed per 200 m ahead, from the last 14 days of
  fixes on the same shape at that day type and hour (or `null`).
  See [`CONTRACT.md`](CONTRACT.md).

## All vehicles

`GET /v1/vehicles` returns every vehicle with a GPS fix from the last 5 minutes, one snapshot
for everyone, rebuilt at most every 30 s. Each vehicle has `line`, `id`, `lat`, `lon`,
`bearing`, `position_at`, `variant`, `delay_s`, `speed`, `path`, `path_beyond`, `path_stops` and `path_speeds`
(same meanings as above).
It does not raise any line's polling priority, so lines nobody watches update about every
150 s. Send `If-None-Match` with the last `ETag` to get `304` when nothing changed.

## Vehicles of a map area

`GET /v1/vehicles/tiles/{z}/{x}/{y}` returns the part of the same snapshot that lies in one
XYZ (Web Mercator, slippy map) tile, in the same JSON shape. `z` is `9` (~62 km tiles, no
`path`, `path_beyond`, `path_stops`, `path_speeds`; for a zoomed-out map) or `13` (~3.9 km tiles, the full
entries). Tiles exist for lat 37.5..38.5, lon 22.9..24.5 (`z` 9: x 288..290, y 196..198;
`z` 13: x 4617..4653, y 3145..3174); others are `404`. Each vehicle is in the one tile of its
position. Use plain URLs: a query string gives `400`, so every client shares the cached copy.
A map view at MapLibre zoom 13 needs 2-15 tiles of 1-8 KB (gzip) instead of the 60+ KB of
`/v1/vehicles`. Client rules: [`CONTRACT.md`](CONTRACT.md#get-v1vehiclestileszxy-rev-5).

## Daily statistics

A server that runs with `--history` sums each finished day (Europe/Athens, 00:00-24:00) about
4-5 h after midnight (it waits for the trips that run past midnight). `GET /v1/stats` returns `{"version":1,"latest":"2026-10-06","days":[...]}`;
`GET /v1/stats/days/{date}` returns one day (`400` for a malformed date, `404` when missing).
The same files, every day kept, are on the `stats` branch of this repository; prefer them:
`https://raw.githubusercontent.com/angelospk/athens-transit-rt/stats/v1/latest.json`,
`.../stats/v1/index.json`, `.../stats/v1/days/{date}.json`.

| Field | Meaning |
|---|---|
| `date`, `tz` | the local day and its time zone |
| `generated_at` | when the file was written |
| `first_fix`, `last_fix` | first and last fix time of the day (a day the history started late, or with an outage, has fewer hours with fixes) |
| `files`, `fixes`, `vehicles`, `lines` | history archives read, GPS fixes, distinct vehicles, distinct lines |
| `hours[h]` | 24 entries by local hour: `fixes`, `vehicles`, `lines`, `dist_m`, `time_s` |
| `by_line[]` | every line with a fix: `line`, `fixes`, `vehicles`, `dist_m`, `time_s` |
| `trips` | scheduled trips of the day and how many ran (below); missing when the server had no static GTFS of the day's history |

Speed in km/h = `dist_m / time_s * 3.6`; `time_s` 0 means no speed. Sums, not averages, so
several days add up exactly. A speed sample is two consecutive fixes of one vehicle on the
same line, 5-120 s apart and at most 25 m/s, counted in the hour and line of the later fix;
the distance is the straight line between them (a little short on curves) and stops and
traffic lights count. A fix counts for the day and hour of its own fix time; a fix that
reaches the server more than 3 h after the end of its day is not counted. A day without any
history file (server down all day) has no file and is not listed. `vehicles` counts vehicles that sent a
fix, which depends on the polling: a line nobody watches is polled about every 150 s. In the
hour repeated when clocks go back, both hours are summed into one entry.

### Trips that ran (`trips`)

| Field | Meaning |
|---|---|
| `gtfs` | the static GTFS version (`gtfs_version` of `/v1/status`) the trips come from |
| `gtfs_expired` | `true` when the day is outside that feed's validity: the planned trips are those of the same weekday inside it, so they may not be the timetable OASA really ran |
| `min_coverage` | share of a trip's route a vehicle must cover for the trip to count as run (0.5) |
| `planned`, `run`, `partial`, `missed`, `unmeasured` | trips the day planned, and how they split: covered at least `min_coverage`; seen but covered less; never seen; no usable shape, so not measured (`run + partial + missed + unmeasured = planned`) |
| `by_line[]` | every line with a planned trip: `line`, the same five counts, `missed_trips` (GTFS `trip_id`s never seen, by start time; `null` when none) |

A day plans every trip whose scheduled start falls in it (a trip at 24:10 of Monday's service
counts for Tuesday). A history row counts for a trip when the matcher assigned the vehicle to
that `trip_id` (same GTFS version) between 30 min before its scheduled start and 30 min after
its scheduled end. Coverage is the distance along the shape between the first and the last of
those rows, divided by the shape length between the trip's first and last stop. Share of trips
run = `run / (planned - unmeasured)`.

Limits: the matcher assigns each vehicle to the closest scheduled trip, so a late bus can take
the slot of a cancelled one (`run` is an upper bound of the trips really run); a bus without
working telematics looks like a missed trip (`missed` is an upper bound of cancellations); an
outage of the server (an hour with few `fixes` in `hours[]`) makes its trips missed; with an
expired feed, a line or trip the old timetable has but OASA no longer runs counts as missed. Rows of
another GTFS version are ignored; when they are most of the day's rows, `trips` is left out.

## GTFS-Realtime details

- `trip_id`, `route_id` and `stop_id` refer to the official OASA static GTFS
  ([osy_gtfs.zip on data.gov.gr](https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057/resource/119db488-16ea-4c76-b560-41c472872390/download/osy_gtfs.zip)). Use the version in `/v1/status`.
- The feeds are `FULL_DATASET`. `header.timestamp` is the build time, or the newest vehicle fix if that is later.
- VehiclePositions has one entity per vehicle (`vehicle-{id}`). A vehicle that is not matched
  to a trip has a trip descriptor with only `route_id`.
- TripUpdates has one entity per matched trip, with one `stop_time_update` (the next stop)
  that carries the current delay. Consumers propagate it to later stops.
- Matching: each vehicle is assigned to a scheduled trip by its position along the route
  (Hungarian assignment, with departure time and the last 15 minutes of positions to keep
  trips stable when buses bunch). OASA itself publishes no trip ids.

### Validation

On 2026-10-05 (20:06-20:08 UTC, 5 samples 30 s apart, 451 vehicles), the
[MobilityData GTFS Realtime validator](https://github.com/MobilityData/gtfs-realtime-validator)
reported:

| Feed | Errors | Warnings |
|---|---|---|
| TripUpdates | 0 | 0 |
| VehiclePositions | 0 | W006, W009 for unmatched vehicles only (19 of 451, about 4%) |

The warnings come from the `route_id`-only trip descriptor of unmatched vehicles. It is
intended: the line is known even when the trip is not.

To repeat it (batch mode, needs the static GTFS that the feed uses):

```bash
mkdir -p rt/vp && curl -sL -o rt/gtfs.zip https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057/resource/119db488-16ea-4c76-b560-41c472872390/download/osy_gtfs.zip
for i in 1 2 3 4 5; do
  curl -s -o "rt/vp/VehiclePositions-$(date -u +%Y-%m-%dT%H-%M-%SZ).pb" https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb
  sleep 30
done
docker run --rm -v "$PWD/rt:/w" --entrypoint java ghcr.io/mobilitydata/gtfs-realtime-validator -Xmx5g \
  -cp /app/gtfs-realtime-validator-webapp-1.0.0-SNAPSHOT.jar edu.usf.cutr.gtfsrtvalidator.lib.Main \
  -gtfs /w/gtfs.zip -gtfsRealtimePath /w/vp -sort name
# results: rt/vp/*.pb.results.json (empty list = no errors and no warnings)
```

## Errors and limits

| Status | Body | Meaning |
|---|---|---|
| `404` | `{"error":"unknown_line"}` | no such line |
| `404` | `{"error":"not_found"}` | unknown path or feed name |
| `503` | `{"error":"warming_up"}` | the line was not polled yet after a start; retry in a few seconds |
| `503` | `{"error":"feed_unavailable"}` | the GTFS-RT feed is not built yet after a start |
| `429` | (from Cloudflare) | more than about 30 requests per 10 s from one IP; wait at least 10 s |

- `/v1/status` reports `ok: false` when no line poll succeeded in the last 5 minutes. During an
  OASA outage the last good data stays served; check `updated_at`.
- When the static GTFS has expired (`gtfs_expires` in the past), vehicles still appear with
  their line, but `trip_id` is `null` until OASA publishes a new GTFS.
- Please poll the GTFS-RT feeds at most every 15 s and each line at most once per
  `next_update_at`. Faster polling gets the same cached data.

## Licenses

Code: MIT ([`LICENSE`](../LICENSE)). Data: derived from OASA open data (the static GTFS on
[data.gov.gr](https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057) and the OASA
telematics positions); use it under the terms of those sources.
