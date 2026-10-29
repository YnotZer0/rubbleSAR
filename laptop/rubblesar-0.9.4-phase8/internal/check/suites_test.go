package check

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
)

// shared is a memory bus whose Close is a no-op, so several "sessions" can share it.
type shared struct{ *bus.Memory }

func (shared) Close() error { return nil }

func memEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	m := bus.NewMemory()
	t.Cleanup(func() { m.Close() })
	cfg := &config.ZBook{}
	cfg.Site = config.Site{IncidentID: "inc", CenterLat: 36.2025, CenterLon: 36.16056, GroundAltMSLm: 80, RadiusM: 805}
	cfg.Zenoh = config.Zenoh{Mode: "client", Connect: []string{"tcp/127.0.0.1:7447"}, OpenTimeout: config.Duration{Duration: 2 * time.Second}}
	cfg.Check = config.Check{ExpectUAVs: []string{"scout-1", "inv-1", "inv-2"}, ExpectRateHz: 1,
		DiscoverTimeout: config.Duration{Duration: 5 * time.Second}, RateWindow: config.Duration{Duration: 5 * time.Second},
		RateTolerancePct: 25, MaxClockOffset: config.Duration{Duration: 250 * time.Millisecond},
		PingTimeout: config.Duration{Duration: 2 * time.Second}}
	var out bytes.Buffer
	return &Env{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Out: &out,
		Open: func(context.Context, config.Zenoh) (bus.Bus, error) { return shared{m}, nil }}, &out
}

func TestLoopbackOnMemory(t *testing.T) {
	e, out := memEnv(t)
	r := Loopback(context.Background(), e)
	if !r.Passed() {
		t.Fatalf("loopback failed:\n%s", out)
	}
}

func TestRouterOnMemory(t *testing.T) {
	e, out := memEnv(t)
	if r := Router(context.Background(), e); !r.Passed() {
		t.Fatalf("router failed:\n%s", out)
	}
}

func TestPhase0SelfTestPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~25 s")
	}
	e, out := memEnv(t)
	r := Phase0(context.Background(), e, Phase0Options{SelfTest: true, Quick: true})
	if !r.Passed() {
		t.Fatalf("phase0 self-test failed:\n%s", out)
	}
	p, w, f, s := r.Counts()
	if p != 8 || f != 0 || s != 0 {
		t.Fatalf("counts pass=%d warn=%d fail=%d skip=%d\n%s", p, w, f, s, out)
	}
	if r.Context["incident"] != "inc-selftest" {
		t.Fatalf("self-test must use its own incident id, got %q", r.Context["incident"])
	}
	if !strings.Contains(out.String(), "fell from 1.00 to") {
		t.Fatalf("throttle detail missing:\n%s", out)
	}
}

func TestPhase0NoDronesFailsClearly(t *testing.T) {
	e, out := memEnv(t)
	e.Cfg.Check.DiscoverTimeout = config.Duration{Duration: time.Second}
	r := Phase0(context.Background(), e, Phase0Options{})
	if r.Passed() {
		t.Fatal("must fail without drones")
	}
	s := out.String()
	for _, want := range []string{"[FAIL] CHK-02", "E-CHK-002", "missing: scout-1, inv-1, inv-2", "[SKIP] CHK-08"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestSelfTestIncident(t *testing.T) {
	if got := SelfTestIncident("antakya-demo"); got != "antakya-demo-selftest" {
		t.Fatal(got)
	}
	long := strings.Repeat("a", 32)
	if got := SelfTestIncident(long); len(got) != 32 || !strings.HasSuffix(got, "-selftest") {
		t.Fatal(got)
	}
}
