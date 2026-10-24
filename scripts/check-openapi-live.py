#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pyyaml", "jsonschema>=4.18", "gtfs-realtime-bindings"]
# ///
"""Calls every endpoint of docs/openapi/openapi.yaml on the live servers and checks the answers.

For each case: the status code, the Content-Type, CORS (`Access-Control-Allow-Origin: *`) and
the body against the response schema. The check is stricter than the spec: a JSON field that
the spec does not document fails (the spec must list every field). `.pb` feeds must parse as
a GTFS-Realtime FeedMessage. Every path of the spec must have a case.

    uv run scripts/check-openapi-live.py            # or: scripts/check-openapi-live.py
"""

import copy
import json
import pathlib
import sys
import time
import urllib.error
import urllib.request

import yaml
from google.transit import gtfs_realtime_pb2
from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

SPEC = pathlib.Path(__file__).resolve().parent.parent / "docs/openapi/openapi.yaml"

# (spec path, concrete path, expected status[, schema of the closed spec to use instead of the
# response schema: a tile must hold only entries of its zoom]). Error cases use answers that do not depend
# on the moment (no warming_up, no feed_unavailable).
CASES = [
    ("/v1/status", "/v1/status", 200),
    ("/healthz", "/healthz", 200),
    ("/v1/lines/{line_id}", "/v1/lines/040", 200),
    ("/v1/lines/{line_id}", "/v1/lines/A1", 200),
    ("/v1/lines/{line_id}", "/v1/lines/%CE%A796", 200),
    ("/v1/lines/{line_id}", "/v1/lines/ZZZ", 404),
    ("/v1/vehicles", "/v1/vehicles", 200),
    ("/v1/vehicles/tiles/{z}/{x}/{y}", "/v1/vehicles/tiles/13/4636/3160", 200, "TileZ13"),
    ("/v1/vehicles/tiles/{z}/{x}/{y}", "/v1/vehicles/tiles/9/289/197", 200, "TileZ9"),
    ("/v1/vehicles/tiles/{z}/{x}/{y}", "/v1/vehicles/tiles/13/1/1", 404),
    ("/v1/vehicles/tiles/{z}/{x}/{y}", "/v1/vehicles/tiles/13/4636/3160?x", 400),
    ("/v1/gtfs-rt/{feed}", "/v1/gtfs-rt/vehicle_positions.pb", 200),
    ("/v1/gtfs-rt/{feed}", "/v1/gtfs-rt/trip_updates.pb", 200),
    ("/v1/gtfs-rt/{feed}", "/v1/gtfs-rt/vehicle_positions.json", 200),
    ("/v1/gtfs-rt/{feed}", "/v1/gtfs-rt/trip_updates.json", 200),
    ("/v1/gtfs-rt/{feed}", "/v1/gtfs-rt/alerts.pb", 404),
    ("/v1/stats", "/v1/stats", 200),
    ("/v1/stats/days/{date}", "/v1/stats/days/{latest}", 200),
    ("/v1/stats/days/{date}", "/v1/stats/days/2026-13-01", 400),
    ("/v1/stats/days/{date}", "/v1/stats/days/2020-01-01", 404),
    ("/static/v1/lines.json", "/static/v1/lines.json", 200),
    ("/static/v1/lines/{line_id}.json", "/static/v1/lines/040.json", 200),
    ("/static/v1/lines/{line_id}.json", "/static/v1/lines/%CE%911.json", 200),
]

# Object schemas that must not carry undocumented fields in the check. VehicleBase and Motion
# are left out: they are parts of an allOf, so the closing goes on the allOf schemas.
CLOSED = ["Error", "LineResponse", "VehiclesResponse", "TileResponse", "LineVehicle", "CityVehicle",
          "CityVehicleLite", "Status", "StatsIndex", "StatsDay", "StaticLines", "StaticLine"]


def close_all(node):
    """Closes every object schema with properties under node (GTFS-RT: no allOf there)."""
    if isinstance(node, dict):
        if "properties" in node:
            node["additionalProperties"] = False
        for v in node.values():
            close_all(v)
    elif isinstance(node, list):
        for v in node:
            close_all(v)


def closed_spec(spec):
    s = copy.deepcopy(spec)
    sch = s["components"]["schemas"]
    for name in CLOSED:
        sch[name]["unevaluatedProperties"] = False
    for item in (sch["StatsDay"]["properties"]["hours"]["items"],
                 sch["StatsDay"]["properties"]["by_line"]["items"],
                 sch["StaticLines"]["properties"]["lines"]["items"],
                 sch["StaticLine"]["properties"]["variants"]["additionalProperties"],
                 sch["StaticLine"]["properties"]["stops"]["additionalProperties"]):
        item["unevaluatedProperties"] = False
    close_all(sch["GtfsRtFeed"])
    close_all(sch["GtfsRtTrip"])
    for name, item in (("TileZ13", "CityVehicle"), ("TileZ9", "CityVehicleLite")):
        sch[name] = copy.deepcopy(sch["TileResponse"])
        sch[name]["properties"]["vehicles"]["items"] = {"$ref": f"#/components/schemas/{item}"}
    return s


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "athens-transit-rt openapi check"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def main():
    spec = yaml.safe_load(SPEC.read_text())
    root = "urn:openapi"
    registry = Registry().with_resource(root, Resource(closed_spec(spec), DRAFT202012))
    default_server = spec["servers"][0]["url"]

    missing = set(spec["paths"]) - {c[0] for c in CASES}
    errors = [f"no case for spec path {p}" for p in sorted(missing)]
    latest = None
    for spec_path, path, want, *override in CASES:
        item = spec["paths"][spec_path]
        server = item.get("servers", [{"url": default_server}])[0]["url"]
        if "{latest}" in path:
            if latest is None:
                errors.append(f"{path}: skipped, /v1/stats gave no latest day")
                continue
            path = path.replace("{latest}", latest)
        url = server + path
        time.sleep(0.5)  # stay well under Cloudflare's ~30 requests per 10 s
        status, headers, body = fetch(url)
        ctype = (headers.get("Content-Type") or "").split(";")[0].strip()
        where = f"{path} -> {status} {ctype}"
        resp = item["get"]["responses"].get(str(status))
        problems = []
        if status != want:
            problems.append(f"status {status}, want {want}")
        if resp is None:
            problems.append(f"status {status} not in the spec")
        if headers.get("Access-Control-Allow-Origin") != "*":
            problems.append("no Access-Control-Allow-Origin: *")
        content = (resp or {}).get("content", {})
        if ctype == "" and "text/plain" in content:
            ctype = "text/plain"  # /healthz sends no Content-Type
        if content and ctype not in content:
            problems.append(f"Content-Type {ctype!r} not in {sorted(content)}")
        elif ctype == "application/json" and content:
            ref = "/".join(["#/paths", spec_path.replace("~", "~0").replace("/", "~1"), "get/responses",
                            str(status), "content/application~1json/schema"])
            if override and status == want:
                ref = "#/components/schemas/" + override[0]
            v = Draft202012Validator({"$ref": root + ref}, registry=registry, format_checker=FormatChecker())
            data = json.loads(body)
            for e in list(v.iter_errors(data))[:5]:
                problems.append(f"schema: {'/'.join(map(str, e.absolute_path))}: {e.message[:200]}")
            if path == "/v1/stats":
                latest = data.get("latest") if isinstance(data, dict) else None
            items = next((data[k] for k in ("vehicles", "entity", "lines", "days") if isinstance(data.get(k), list)), None)
            where += f" ({len(body)} bytes" + (f", {len(items)} items)" if items is not None else ")")
        elif ctype == "application/x-protobuf":
            feed = gtfs_realtime_pb2.FeedMessage()
            feed.ParseFromString(body)  # may have no entity (no vehicle now)
            if not feed.IsInitialized():
                problems.append("FeedMessage without required fields (header)")
            where += f" ({len(feed.entity)} entities)"
        elif ctype == "text/plain" and body != b"ok\n":
            problems.append(f"body {body[:40]!r}")
        print(("FAIL " if problems else "ok   ") + where)
        errors += [f"{path}: {p}" for p in problems]
    if errors:
        print("\n".join(["", f"{len(errors)} problem(s):"] + errors))
        sys.exit(1)
    print(f"\nall {len(CASES)} cases match the spec")


if __name__ == "__main__":
    main()
