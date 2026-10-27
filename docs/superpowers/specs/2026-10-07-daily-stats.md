# Daily network statistics

Status: built 2026-10-07 (Harold asked); long-term tab planned for 2026-10-14. Input: the fix history
([2026-10-06-fix-history.md](2026-10-06-fix-history.md)).

## What Harold wants

An ⓘ sheet in athens-transit-map with three charts that keep their form while the data changes
every day: vehicles in service per hour, network mean speed per hour, the 10 slowest and the 10
fastest lines. The same numbers are public: an API endpoint, and a copy on GitHub that the
sheet links to ("δες τα δεδομένα"). The GitHub copy also keeps the mini PC from serving the
sheet's traffic. Later (2026-10-14): a long-term tab built from the stored days.

## Built (after the Codex plan review)

Format and method: [API.md, Daily statistics](../../API.md#daily-statistics). Code:
`internal/stats` (tests: DST days, midnight, shuffled and numbered archives, bad pairs),
`internal/server/stats.go`, `atrt stats`, `.github/workflows/stats.yml`.

Decisions taken from the review:

- **Fix time, not file time.** A day reads the archives received from its start until
  `Late` (1 h) after its end and keeps rows by `fix_t`. A pair across midnight is dropped.
- **Ready = time, not files.** A day is summed `Settle` (4 h since 2026-10-27, so trips past midnight end first) after its end, once none of its
  receipt hours has a plain `.csv` or a sealed `.csv.part`. An idle hour (no file) does not
  block. A read error skips the day; the hourly job retries. A written day is never rewritten
  (`atrt stats --force` does). `index.json` is rebuilt from the day files every run.
- **DST.** `Bounds` uses the next local midnight (23 / 25 h). `hours` has 24 entries by local
  hour; the repeated hour is merged (vehicles distinct across both). `time/tzdata` is linked in.
- **Coverage.** `first_fix`, `last_fix`, `files` and per-hour `fixes` show a partial day; the
  frontend says so. No speed is `time_s` 0, shown as missing, not 0 km/h.
- **Names.** `vehicles` = vehicles that sent a fix (depends on polling), documented.
- **Publishing.** `stats.yml` uses `GITHUB_TOKEN` with `contents: write`, one run at a time,
  starts the orphan branch when missing, validates every day with jq, and writes `index.json`
  and `latest.json` from the files on the branch (so GitHub keeps days the server lost).
- **Retention.** The hourly job sums a day within hours, far inside the 30-day history.

Not done: per-row lateness above 1 h is lost by design. Running `atrt stats` against the live
state directory while the server runs is safe (atomic renames) but pointless.

## Frontend (athens-transit-map)

- ⓘ button in the panel head, next to layers. Opens a sheet: full-screen on phones, a
  centred dialog (max 720 px) on wider screens; Esc and a close button close it.
- A native `<dialog>` (`showModal`): focus trap, Esc and focus return come with it.
- Loads `latest.json` from GitHub when the sheet opens; again on a later open if it failed
  or is older than an hour. Fetch error and "no data yet" are different messages. Shows the
  day's date, and a note when the day is partial (first fix after 01:00 or last before 23:00).
- Three charts in SVG, as in the prototype: bars of vehicles per hour, a line of km/h per
  hour (hover/tap tooltip), the 10 slowest / 10 fastest lines with ≥ 15 vehicle-hours.
  Short text per chart; the date; "δες τα δεδομένα" → the `stats` branch on GitHub.
- Pure helpers (`speed`, ranking) in `src/lib/stats.ts` with vitest tests.

## Tests that prove it

- stats: two vehicles, known positions/times → exact dist/time per hour and line; Δt > 120 s,
  jumps > 25 m/s, line change between fixes, duplicate fix_t → not counted; a day crossing
  the DST change (25 or 23 hours) maps to local hours right; few fixes → first_fix / last_fix.
- job: does not write an incomplete day; does not rewrite an existing day; index lists days.
- http: 200 / 404 / bad date → 400.
- frontend: ranking threshold and order; empty data renders the fallback.
