package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
)

// DefaultOverpassURL is the public Overpass API endpoint.
const DefaultOverpassURL = "https://overpass-api.de/api/interpreter"

// OverpassQuery returns the query for building outlines within radius metres.
func OverpassQuery(lat, lon, radius float64) string {
	return fmt.Sprintf(`[out:json][timeout:90];way["building"](around:%.0f,%.6f,%.6f);out geom;`, radius, lat, lon)
}

// FetchOverpass downloads building outlines (Overpass JSON, "out geom").
func FetchOverpass(ctx context.Context, endpoint string, lat, lon, radius float64) ([]byte, error) {
	if endpoint == "" {
		endpoint = DefaultOverpassURL
	}
	form := url.Values{"data": {OverpassQuery(lat, lon, radius)}}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, diag.Wrap("E-SCN-006", err, "building request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "rubblesar-scenario-tool (simulation; offline research)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, diag.Wrap("E-SCN-006", err, "POST %s", endpoint)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return nil, diag.Wrap("E-SCN-006", err, "reading reply from %s", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(b)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, diag.New("E-SCN-006", "%s answered HTTP %d: %s", endpoint, resp.StatusCode, snippet)
	}
	if _, err := ParseOverpass(b, geo.NewLocal(geo.Point{Lat: lat, Lon: lon})); err != nil {
		return nil, err
	}
	return b, nil
}

type overpassDoc struct {
	Elements []struct {
		Type     string            `json:"type"`
		ID       int64             `json:"id"`
		Tags     map[string]string `json:"tags"`
		Geometry []struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
		} `json:"geometry"`
	} `json:"elements"`
}

// ParseOverpass converts Overpass JSON into local-metre footprints.
func ParseOverpass(b []byte, local geo.Local) ([]Footprint, error) {
	var doc overpassDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, diag.Wrap("E-SCN-005", err, "not Overpass JSON")
	}
	var out []Footprint
	for _, e := range doc.Elements {
		if e.Type != "way" || e.Tags["building"] == "" || len(e.Geometry) < 4 {
			continue
		}
		g := e.Geometry
		if g[0] == g[len(g)-1] {
			g = g[:len(g)-1]
		}
		p := make(geom.Poly, len(g))
		for i, v := range g {
			x, y := local.ToLocal(geo.Point{Lat: v.Lat, Lon: v.Lon})
			p[i] = geom.P{X: x, Y: y}
		}
		if p.Area() < 1 {
			continue
		}
		out = append(out, Footprint{ID: fmt.Sprintf("way/%d", e.ID), Poly: p})
	}
	if len(out) == 0 {
		return nil, diag.Wrap("E-SCN-005", ErrNoFootprints, "the file holds %d elements but no closed building ways", len(doc.Elements))
	}
	return out, nil
}

// LoadOverpassFile reads a saved Overpass file.
func LoadOverpassFile(path string, local geo.Local) ([]Footprint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, diag.Wrap("E-SCN-005", err, "reading %s", path)
	}
	fp, err := ParseOverpass(b, local)
	if err != nil {
		return nil, diag.Wrap("E-SCN-005", err, "%s", path)
	}
	return fp, nil
}
