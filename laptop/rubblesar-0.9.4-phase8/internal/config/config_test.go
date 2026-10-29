package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rubblesar/internal/diag"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodEdge = `
[site]
incident_id = "antakya-demo"
[zenoh]
connect = ["tcp/192.168.1.50:7447"]
[[uav]]
id = "scout-1"
role = "scout"
behaviour = "orbit"
alt_agl_m = 40
[[uav]]
id = "inv-1"
role = "investigator"
behaviour = "landed"
`

func TestLoadEdgeGood(t *testing.T) {
	c, err := LoadEdge(write(t, goodEdge))
	if err != nil {
		t.Fatalf("LoadEdge: %v", err)
	}
	if len(c.UAVs) != 2 || c.UAVs[0].OrbitRadiusM != 150 || *c.UAVs[1].BudgetKbps != 256 {
		t.Fatalf("defaults not applied: %+v", c.UAVs)
	}
	if c.Site.CenterLat != defaultLat || !c.Zenoh.RetryForever {
		t.Fatalf("site/zenoh defaults wrong: %+v %+v", c.Site, c.Zenoh)
	}
}

func TestLoadEdgeEnvOverride(t *testing.T) {
	t.Setenv("SAR_ZENOH_CONNECT", " tcp/10.0.0.9:7447 , tcp/10.0.0.10:7447 ")
	t.Setenv("SAR_INCIDENT_ID", "drill-7")
	c, err := LoadEdge(write(t, goodEdge))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Zenoh.Connect) != 2 || c.Zenoh.Connect[0] != "tcp/10.0.0.9:7447" || c.Site.IncidentID != "drill-7" {
		t.Fatalf("env override not applied: %+v %+v", c.Zenoh.Connect, c.Site.IncidentID)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := LoadEdge(filepath.Join(t.TempDir(), "nope.toml"))
	if diag.CodeOf(err) != "E-CFG-001" {
		t.Fatalf("want E-CFG-001, got %v", err)
	}
}

func TestLoadBadSyntax(t *testing.T) {
	_, err := LoadEdge(write(t, "[site\nincident_id = 1"))
	if diag.CodeOf(err) != "E-CFG-002" {
		t.Fatalf("want E-CFG-002, got %v", err)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	_, err := LoadEdge(write(t, goodEdge+"\n[admin]\nlisten = \"127.0.0.1:9101\"\nlistn = \"x\"\n"))
	if diag.CodeOf(err) != "E-CFG-003" || !strings.Contains(err.Error(), "admin.listn") {
		t.Fatalf("want E-CFG-003 naming admin.listn, got %v", err)
	}
}

func TestValidateCollectsAllProblems(t *testing.T) {
	body := `
[site]
incident_id = "Bad Id"
center_lat = 99
[zenoh]
mode = "router"
connect = ["tcp/nohost"]
[[uav]]
id = "x"
role = "pilot"
behaviour = "loop"
alt_agl_m = 500
[[uav]]
id = "x"
role = "scout"
`
	_, err := LoadEdge(write(t, body))
	var ve *ValidationError
	if err == nil {
		t.Fatal("want error")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	want := []string{"incident_id", "center_lat", "mode", "tcp/nohost", "role \"pilot\"", "behaviour \"loop\"", "alt_agl_m 500", "duplicate id"}
	all := strings.Join(ve.Problems, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("problems do not mention %q:\n%s", w, all)
		}
	}
	if diag.CodeOf(err) != "E-CFG-003" {
		t.Errorf("CodeOf = %q", diag.CodeOf(err))
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, ep := range []string{"tcp/127.0.0.1:7447", "tcp/zbook.local:7447", "tls/10.0.0.1:7447", "tcp/[::1]:7447"} {
		if err := ValidateEndpoint(ep); err != nil {
			t.Errorf("%s: %v", ep, err)
		}
	}
	for _, ep := range []string{"127.0.0.1:7447", "http/1.2.3.4:80", "tcp/1.2.3.4", "tcp/:7447", "tcp/1.2.3.4:0", "tcp/1.2.3.4:99999"} {
		if err := ValidateEndpoint(ep); err == nil {
			t.Errorf("%s: want error", ep)
		}
	}
}

func TestZenohJSON5(t *testing.T) {
	z := Zenoh{Mode: "client", Connect: []string{"tcp/1.2.3.4:7447"}}
	s, err := z.ZenohJSON5()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"mode":"client"`, `"endpoints":["tcp/1.2.3.4:7447"]`, `"multicast":{"enabled":false}`} {
		if !strings.Contains(s, w) {
			t.Errorf("%s missing %s", s, w)
		}
	}
	z = Zenoh{Mode: "peer", Listen: []string{"tcp/127.0.0.1:0"}}
	s, _ = z.ZenohJSON5()
	if !strings.Contains(s, `"listen":{"endpoints":["tcp/127.0.0.1:0"]}`) || !strings.Contains(s, `"connect":{"endpoints":[]}`) {
		t.Errorf("peer config wrong: %s", s)
	}
}

func TestLoadZBookDefaults(t *testing.T) {
	c, err := LoadZBook(write(t, "[site]\nincident_id = \"antakya-demo\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Check.ExpectUAVs) != 3 || c.Check.RateWindow.Seconds() != 30 || c.Zenoh.Connect[0] != "tcp/127.0.0.1:7447" {
		t.Fatalf("defaults wrong: %+v", c.Check)
	}
}

func TestDispatchConfig(t *testing.T) {
	c, err := LoadZBook(write(t, "[site]\nincident_id = \"antakya-demo\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A phase 4 config without [dispatch]: the default teams, staging at the centre, the incident's street file.
	if len(c.Dispatch.Teams) != 4 || c.Dispatch.Teams[3].Source != "tak" || c.RoadsPath() != "/var/lib/rubblesar/roads/antakya-demo.json" {
		t.Fatalf("defaults: %+v %s", c.Dispatch.Teams, c.RoadsPath())
	}
	if lat, lon := c.Staging(); lat != c.Site.CenterLat || lon != c.Site.CenterLon {
		t.Fatalf("staging %v %v", lat, lon)
	}
	_, err = LoadZBook(write(t, `[site]
incident_id = "antakya-demo"
[dispatch]
tak_server = "no-port"
triage = ["red", "black"]
time_scale = 0
[[dispatch.team]]
id = "a"
callsign = "ALPHA"
mode = "bike"
source = "sim"
at_command_post = true
[[dispatch.team]]
id = "a"
callsign = "alpha"
mode = "foot"
source = "phone"
`))
	if err == nil {
		t.Fatal("bad dispatch config accepted")
	}
	for _, want := range []string{"tak_server", `triage "black"`, "time_scale", `mode "bike"`, `id "a" is used twice`, `callsign "alpha" is used twice`, `source "phone"`, "at_command_post is for TAK teams"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	c, err = LoadZBook("../../configs/zbook/sar.toml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Dispatch.TimeScale != 5 || len(c.Dispatch.Teams) != 5 || c.Dispatch.StagingLat == 0 || !c.Dispatch.Extrication || c.Dispatch.RescueTimeScale != 30 {
		t.Fatalf("release config %+v", c.Dispatch)
	}
}
