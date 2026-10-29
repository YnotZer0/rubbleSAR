// Package ops is the rubblesar command (phase 6): one command to bring the
// whole system up or down on both machines, see its state on one screen,
// run a mission and write its after-action report.
package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// ZBookServices in start order.
var ZBookServices = []string{"zenohd", "casualty-analyst", "mission-planner", "rubblesar-taky", "rescue-dispatch", "sar-web"}

// System runs the machine-level commands (a fake in tests).
type System interface {
	// Systemctl runs "systemctl ACTION UNITS..." with the rights it needs.
	Systemctl(ctx context.Context, action string, units ...string) error
	// Active is "systemctl is-active UNIT" ("active", "inactive", "failed", "unknown" ...).
	Active(ctx context.Context, unit string) string
	// SSH runs a command on target (user@host) without asking for a password.
	SSH(ctx context.Context, target, command string) (string, error)
}

// Exec is the real System.
type Exec struct {
	// Stdin/Stdout/Stderr for sudo's password prompt.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Systemctl runs systemctl, through sudo unless root.
func (e Exec) Systemctl(ctx context.Context, action string, units ...string) error {
	args := append([]string{"systemctl", action}, units...)
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, "sudo", args...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Active asks systemd.
func (e Exec) Active(ctx context.Context, unit string) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}

// SSH runs a command on target with a key (BatchMode: never prompts).
func (e Exec) SSH(ctx context.Context, target, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=6", "-o", "StrictHostKeyChecking=accept-new", target, command)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%v: %s", err, msg)
	}
	return out.String(), nil
}

// Level of a status row.
type Level int

// Levels, best first.
const (
	OK Level = iota
	Warn
	Fail
)

func (l Level) String() string { return [...]string{"ok", "WARN", "FAIL"}[l] }

// Row is one line of the status.
type Row struct {
	Group, Name string
	Level       Level
	Detail      string
	Fix         string
}

// Status is the whole system's state.
type Status struct {
	Rows     []Row
	Dispatch *dispatch.Status
	Drones   map[string]*sarpb.Telemetry
}

// Problems counts rows at Fail.
func (s *Status) Problems() int {
	n := 0
	for _, r := range s.Rows {
		if r.Level == Fail {
			n++
		}
	}
	return n
}

// Env is what status, up, down and mission need.
type Env struct {
	Cfg *config.ZBook
	Sys System
	// Bus for the drones' telemetry (nil: drones not checked).
	Bus bus.Bus
	// HTTP probes (default: a 3 s client).
	Client *http.Client
	// TelWait: how long to listen for telemetry (default 3 s).
	TelWait time.Duration
}

func (e *Env) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func local(listen string) string {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "127.0.0.1"
	}
	return net.JoinHostPort(h, p)
}

func (e *Env) getJSON(addr, path string, v any) error {
	resp, err := e.client().Get("http://" + local(addr) + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		var st struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(b, &st)
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, st.State)
	}
	return json.Unmarshal(b, v)
}

func (e *Env) post(addr, path string) error {
	c := *e.client()
	c.Timeout = 30 * time.Second
	resp, err := c.Post("http://"+local(addr)+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d %s", addr, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func tcpOpen(addr string) error {
	c, err := net.DialTimeout("tcp", local(addr), 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// zenohAddr is the router's host:port from [zenoh] connect ("tcp/127.0.0.1:7447").
func (e *Env) zenohAddr() string {
	for _, c := range e.Cfg.Zenoh.Connect {
		if strings.HasPrefix(c, "tcp/") {
			return strings.TrimPrefix(c, "tcp/")
		}
	}
	return "127.0.0.1:7447"
}

// probe checks one ZBook service: systemd, then whether it answers.
func (e *Env) probe(ctx context.Context, name string, st *Status) Row {
	cfg := e.Cfg
	r := Row{Group: "ZBook", Name: name}
	active := e.Sys.Active(ctx, name)
	var err error
	switch name {
	case "zenohd":
		err = tcpOpen(e.zenohAddr())
		r.Detail = "router on " + e.zenohAddr()
	case "casualty-analyst":
		var a struct {
			Version    string          `json:"version"`
			State      string          `json:"state"`
			Casualties json.RawMessage `json:"casualties"`
		}
		if err = e.getJSON(cfg.Analyst.Listen, "/v1/status", &a); err == nil {
			r.Detail = fmt.Sprintf("%s, %s", a.State, versionOf(a.Version))
			var list []json.RawMessage
			if json.Unmarshal(a.Casualties, &list) == nil {
				r.Detail = fmt.Sprintf("%d casualties; %s", len(list), versionOf(a.Version))
			}
		}
	case "mission-planner":
		var p struct {
			Version string `json:"version"`
			State   string `json:"state"`
		}
		if err = e.getJSON(cfg.Planner.Listen, "/v1/status", &p); err == nil {
			r.Detail = fmt.Sprintf("%s; %s", p.State, versionOf(p.Version))
		}
	case "rubblesar-taky":
		err = tcpOpen(cfg.Dispatch.TakServer)
		r.Detail = "TAK server on " + cfg.Dispatch.TakServer + " (plain TCP)"
	case "rescue-dispatch":
		var d struct {
			Version  string          `json:"version"`
			Dispatch dispatch.Status `json:"dispatch"`
		}
		if err = e.getJSON(cfg.Dispatch.Listen, "/v1/status", &d); err == nil {
			ds := d.Dispatch
			st.Dispatch = &ds
			idle, off := 0, 0
			for _, t := range ds.Teams {
				switch t.State {
				case "idle":
					idle++
				case "offline":
					off++
				}
			}
			tak := "TAK NOT CONNECTED"
			if ds.TAK.Connected {
				tak = "TAK connected"
				if len(ds.TAK.Users) > 0 {
					tak += " (users " + strings.Join(ds.TAK.Users, ", ") + ")"
				}
			}
			streets := "NO STREET FILE"
			if ds.Roads.Loaded {
				streets = fmt.Sprintf("%s streets %.0f km", ds.Roads.Source, ds.Roads.LengthKm)
			}
			r.Detail = fmt.Sprintf("%s; %s; %d teams (%d free, %d offline); %s", tak, streets, len(ds.Teams), idle, off, versionOf(d.Version))
			if !ds.TAK.Connected || !ds.Roads.Loaded {
				r.Level = Warn
				r.Fix = "TAK: sudo systemctl status rubblesar-taky; streets: scripts/zbook/32-roads.sh"
			}
		}
	case "sar-web":
		var w struct {
			Version string `json:"version"`
			Maps    []struct {
				Incident string `json:"incident"`
				Tiles    int    `json:"tiles"`
			} `json:"maps"`
			Reports int `json:"reports"`
		}
		if err = e.getJSON(cfg.Web.Listen, "/v1/status", &w); err == nil {
			m := "NO MAP"
			for _, x := range w.Maps {
				if x.Incident == cfg.Site.IncidentID {
					m = fmt.Sprintf("map %d tiles", x.Tiles)
				}
			}
			_, port, _ := net.SplitHostPort(cfg.Web.Listen)
			r.Detail = fmt.Sprintf("%s; %d reports; http://%s:%s/", m, w.Reports, firstAddress(), port)
			if m == "NO MAP" {
				r.Level = Warn
				r.Fix = "scripts/zbook/36-map.sh draws the offline map"
			}
		}
	}
	switch {
	case active != "active":
		r.Level, r.Detail = Fail, active
		r.Fix = "rubblesar up (or: sudo systemctl start " + name + "; journalctl -u " + name + " -n 50)"
		if active == "inactive" || active == "unknown" {
			r.Detail = "not running"
		}
		if (active == "unknown" || strings.Contains(active, "could not be found")) && name == "sar-web" {
			r.Fix = "scripts/zbook/30-install.sh installs it"
		}
	case err != nil:
		r.Level = Fail
		r.Detail = "running but not answering: " + err.Error()
		r.Fix = "journalctl -u " + name + " -n 50"
	}
	return r
}

func versionOf(v string) string {
	f := strings.Fields(v)
	if len(f) >= 2 {
		return f[1]
	}
	return v
}

// firstAddress is this machine's first address on the network.
func firstAddress() string {
	ifs, _ := net.Interfaces()
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 || strings.HasPrefix(in.Name, "docker") || strings.HasPrefix(in.Name, "br-") || strings.HasPrefix(in.Name, "veth") {
			continue
		}
		addrs, _ := in.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return "localhost"
}

// Telemetry listens for the drones' telemetry for up to wait (or until every expected drone reported).
func Telemetry(ctx context.Context, b bus.Bus, incident string, expect []string, wait time.Duration) map[string]*sarpb.Telemetry {
	out := map[string]*sarpb.Telemetry{}
	if b == nil {
		return out
	}
	var mu sync.Mutex
	got := make(chan struct{}, 1)
	sub, err := b.Subscribe(keys.TelemetryAll(incident), func(x bus.Sample) {
		var t sarpb.Telemetry
		if msg.Unmarshal(x.Payload, &t) != nil || t.UavId == "" {
			return
		}
		mu.Lock()
		out[t.UavId] = &t
		all := true
		for _, id := range expect {
			all = all && out[id] != nil
		}
		mu.Unlock()
		if all {
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		return out
	}
	defer sub.Close()
	select {
	case <-ctx.Done():
	case <-got:
	case <-time.After(wait):
	}
	mu.Lock()
	defer mu.Unlock()
	cp := map[string]*sarpb.Telemetry{}
	for k, v := range out {
		cp[k] = v
	}
	return cp
}

// GetStatus checks everything.
func GetStatus(ctx context.Context, e *Env) *Status {
	st := &Status{}
	for _, s := range ZBookServices {
		st.Rows = append(st.Rows, e.probe(ctx, s, st))
	}
	// The Jetson over SSH.
	if j := e.Cfg.Ops.Jetson; j != "" {
		svc := e.Cfg.Ops.JetsonService
		out, err := e.Sys.SSH(ctx, j, "systemctl is-active "+svc+"; cat /opt/rubblesar/VERSION 2>/dev/null || true")
		r := Row{Group: "Jetson (" + j + ")", Name: svc}
		lines := strings.Fields(out)
		switch {
		case err != nil && len(lines) == 0:
			r.Level, r.Detail = Warn, "cannot reach it over SSH: "+short(err.Error(), 90)
			r.Fix = "scripts/zbook/38-jetson-link.sh --jetson " + j
		case len(lines) > 0 && lines[0] == "active":
			r.Detail = "active"
			if len(lines) > 1 {
				r.Detail += ", " + lines[1]
			}
		default:
			r.Level, r.Detail = Fail, strings.Join(lines, " ")
			r.Fix = "rubblesar up (or on the Jetson: sudo systemctl start " + svc + ")"
		}
		st.Rows = append(st.Rows, r)
	}
	// The drones, over Zenoh.
	wait := e.TelWait
	if wait <= 0 {
		wait = 3 * time.Second
	}
	if e.Bus != nil {
		st.Drones = Telemetry(ctx, e.Bus, e.Cfg.Site.IncidentID, e.Cfg.Check.ExpectUAVs, wait)
		ids := append([]string(nil), e.Cfg.Check.ExpectUAVs...)
		for id := range st.Drones {
			if !contains(ids, id) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			t := st.Drones[id]
			r := Row{Group: "Drones", Name: id}
			if t == nil {
				r.Level, r.Detail = Fail, "no telemetry"
				r.Fix = "is edge-eye running on the Jetson? (rubblesar up, or on the Jetson: sudo systemctl status edge-eye)"
			} else {
				sw := t.Software
				if sw == "" {
					sw = "edge-eye 0.6 or older (no version in telemetry)"
				}
				r.Detail = fmt.Sprintf("%s, battery %.0f%%, %s", strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_")), t.BatteryPct, sw)
			}
			st.Rows = append(st.Rows, r)
		}
	} else {
		st.Rows = append(st.Rows, Row{Group: "Drones", Name: "zenoh", Level: Fail, Detail: "no connection to zenohd: drones not checked", Fix: "rubblesar up"})
	}
	return st
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func short(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

// Text renders the status.
func (s *Status) Text() string {
	var b strings.Builder
	group := ""
	for _, r := range s.Rows {
		if r.Group != group {
			group = r.Group
			fmt.Fprintf(&b, "\n%s\n", group)
		}
		fmt.Fprintf(&b, "  %-18s %-5s %s\n", r.Name, r.Level, r.Detail)
		if r.Level != OK && r.Fix != "" {
			fmt.Fprintf(&b, "  %-18s       fix: %s\n", "", r.Fix)
		}
	}
	if n := s.Problems(); n > 0 {
		fmt.Fprintf(&b, "\nRESULT: %d problem(s)\n", n)
	} else {
		b.WriteString("\nRESULT: everything up\n")
	}
	return b.String()
}
