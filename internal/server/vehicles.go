package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	vehiclesEvery  = 30 * time.Second // the snapshot is rebuilt at most this often
	vehiclesMaxAge = 30
	vehicleMaxAge  = 5 * time.Minute // older GPS fixes are left out
)

// CityVehicle is one entry of the /v1/vehicles response (docs/CONTRACT.md).
type CityVehicle struct {
	Line       string       `json:"line"`
	ID         string       `json:"id"`
	Lat        float64      `json:"lat"`
	Lon        float64      `json:"lon"`
	Bearing    *float64     `json:"bearing"`
	PositionAt int64        `json:"position_at"`
	Variant    *string      `json:"variant"`
	DelayS     *int         `json:"delay_s"`
	Speed      *float64     `json:"speed"`
	Path       [][2]float64 `json:"path"`
	PathBeyond [][2]float64 `json:"path_beyond"`
	PathStops  []int        `json:"path_stops"`
	PathSpeeds []*float64   `json:"path_speeds"`
}

type VehiclesResponse struct {
	UpdatedAt    int64         `json:"updated_at"`
	NextUpdateAt int64         `json:"next_update_at"`
	Vehicles     []CityVehicle `json:"vehicles"`
}

// vehiclesSnapshot is the encoded /v1/vehicles body and tiles every client gets until the next
// build.
type vehiclesSnapshot struct {
	built time.Time
	encoded
	tiles map[tileKey]*encoded // non-empty tiles
	empty *encoded             // every other tile of the domain
}

// encoded is one response body, raw and gzipped, with their ETags.
type encoded struct {
	raw, gz      []byte
	etag, gzETag string
}

// vehicles returns the current snapshot, building it when it is due. Requests wait for a
// build in progress instead of starting their own.
func (a *App) vehicles() (*vehiclesSnapshot, error) {
	a.vehMu.Lock()
	defer a.vehMu.Unlock()
	now := a.now()
	if s := a.vehSnap; s != nil && now.Sub(s.built) < vehiclesEvery && !now.Before(s.built) {
		return s, nil
	}
	s, err := a.buildVehicles(now)
	if err != nil {
		return nil, err
	}
	a.vehSnap = s
	return s, nil
}

// collectVehicles is every vehicle with a recent fix on the current feed, once (newest fix
// wins; on a tie the first line in id order), sorted by line and id.
func (a *App) collectVehicles(now time.Time) []CityVehicle {
	a.mu.RLock()
	ids := make([]string, 0, len(a.lines))
	for id, d := range a.lines {
		if a.w != nil && d.feed == a.w.feed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := []CityVehicle{}
	at := map[string]int{}
	for _, id := range ids {
		for i := range a.lines[id].vehicles {
			v := &a.lines[id].vehicles[i]
			if now.Unix()-v.PositionAt > int64(vehicleMaxAge/time.Second) {
				continue
			}
			c := CityVehicle{Line: id, ID: v.ID, Lat: round5(v.Lat), Lon: round5(v.Lon), Bearing: v.Bearing,
				PositionAt: v.PositionAt, Variant: v.Variant, DelayS: v.DelayS, Speed: v.Speed, Path: v.Path,
				PathBeyond: v.PathBeyond, PathStops: v.PathStops, PathSpeeds: v.PathSpeeds}
			if j, ok := at[v.ID]; !ok {
				at[v.ID] = len(out)
				out = append(out, c)
			} else if v.PositionAt > out[j].PositionAt {
				out[j] = c
			}
		}
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (a *App) buildVehicles(now time.Time) (*vehiclesSnapshot, error) {
	resp := VehiclesResponse{UpdatedAt: now.Unix(), NextUpdateAt: now.Add(vehiclesEvery).Unix(),
		Vehicles: a.collectVehicles(now)}
	zw := gzip.NewWriter(nil)
	all, err := encode(zw, resp)
	if err != nil {
		return nil, err
	}
	s := &vehiclesSnapshot{built: now, encoded: *all}
	if err := s.buildTiles(zw, resp); err != nil {
		return nil, err
	}
	return s, nil
}

// encode marshals v and gzips it with zw.
func encode(zw *gzip.Writer, v any) (*encoded, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw.Reset(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	h := hex.EncodeToString(sum[:8])
	return &encoded{raw: raw, gz: buf.Bytes(), etag: `"` + h + `"`, gzETag: `"` + h + `-gz"`}, nil
}

// handleVehicles serves the prebuilt bytes; the Handler does not gzip this path again.
func (a *App) handleVehicles(w http.ResponseWriter, r *http.Request) {
	a.serveSnapshot(w, r, func(s *vehiclesSnapshot) *encoded { return &s.encoded })
}

// serveSnapshot serves the body pick chooses from the current snapshot, with the snapshot's
// cache headers.
func (a *App) serveSnapshot(w http.ResponseWriter, r *http.Request, pick func(*vehiclesSnapshot) *encoded) {
	s, err := a.vehicles()
	if err != nil {
		a.log.Error("building /v1/vehicles", "err", err)
		writeJSON(w, http.StatusInternalServerError, minMaxAge, map[string]string{"error": "internal"})
		return
	}
	e := pick(s)
	body, etag := e.raw, e.etag
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body, etag = e.gz, e.gzETag
	}
	// The floor of 5 s can keep a copy in caches up to 5 s past next_update_at.
	maxAge := clamp(int64(s.built.Add(vehiclesEvery).Sub(a.now())/time.Second), minMaxAge, vehiclesMaxAge)
	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	h.Set("ETag", etag)
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	if etag == e.gzETag {
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// etagMatch implements If-None-Match's weak comparison ("*", lists, W/ prefixes).
func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}
