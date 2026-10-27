# athens-transit-rt daily statistics

One file per day (Europe/Athens) of the OASA buses and trolleys seen by
[athens-transit-rt](https://github.com/angelospk/athens-transit-rt): GPS fixes,
vehicles and travel speed per hour and per line, and how many scheduled trips ran
(`trips`). Updated daily by the `stats` workflow.

- `v1/latest.json`: the newest day
- `v1/index.json`: all days
- `v1/days/YYYY-MM-DD.json`: one day

Speed (km/h) = `dist_m / time_s * 3.6`. Fields and method: `docs/API.md` on `main`.
Raw URL: `https://raw.githubusercontent.com/angelospk/athens-transit-rt/stats/v1/latest.json`
