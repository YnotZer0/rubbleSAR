package check

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// P7-01: a sar-web started before the teams changed is a warning (the live
// picture's teams come from rescue-dispatch); a scene without sites fails.
func TestP7ServedTeams(t *testing.T) {
	var scene atomic.Value
	scene.Store(`{"incident":"inc","map":{},"sites":[{"id":"s01"}],"teams":[{"id":"team-1"}]}`)
	mux := http.NewServeMux()
	for path, body := range map[string]string{"/ops/": "RubbleSAR operator view", "/ops/app.js": "// RubbleSAR operator view", "/ops/app.css": ":root{--signal:#000}", "/ops/fonts/Barlow-Regular.woff2": "wOF2"} {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	}
	mux.HandleFunc("/v1/scene", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(scene.Load().(string))) })
	mux.HandleFunc("/v1/picture", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"incident":"inc","mode":"live","bus":"connected","drones":[{"id":"scout-1"}],"teams":[]}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	web := strings.TrimPrefix(ts.URL, "http://")

	if r := p7Served(web, "inc", []string{"scout-1"}, 1); r.Status != Pass {
		t.Fatalf("same teams: %+v", r)
	}
	r := p7Served(web, "inc", []string{"scout-1"}, 2)
	if r.Status != Warn || !strings.Contains(r.Detail, "sar-web's scene has 1 teams, sar.toml 2") || !strings.Contains(r.Hint, "restart sar-web") {
		t.Fatalf("stale teams: %+v", r)
	}
	scene.Store(`{"incident":"inc","map":{},"sites":[],"teams":[{"id":"team-1"}]}`)
	if r := p7Served(web, "inc", []string{"scout-1"}, 1); r.Status != Fail || !strings.Contains(r.Detail, "no collapse sites") {
		t.Fatalf("no sites: %+v", r)
	}
}
