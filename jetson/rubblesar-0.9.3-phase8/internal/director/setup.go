package director

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"rubblesar/internal/live"
	"rubblesar/internal/obs"
)

// OBSSetup creates the scenes a recorder records: on the ZBook, the presenter
// page and the live operator view as browser sources; elsewhere (the WinTAK
// laptop) a window capture to point at WinTAK.
func OBSSetup(ctx context.Context, rc RecorderConfig, webBase string, out io.Writer) error {
	if rc.Kind != "obs" {
		return fmt.Errorf("E-DIR-003: %s is not an OBS recorder", rc.Name)
	}
	c, err := obs.Connect(rc.URL, rc.Password, 5*time.Second)
	if err != nil {
		return fmt.Errorf("E-DIR-003: %s (%s): %w", rc.Name, rc.URL, err)
	}
	defer c.Close()
	fmt.Fprintf(out, "    OBS %s (obs-websocket %s) at %s\n", c.OBSVersion, c.WSVersion, rc.URL)
	kinds, err := c.InputKinds(ctx)
	if err != nil {
		return fmt.Errorf("E-DIR-003: %s: %w", rc.Name, err)
	}
	has := map[string]bool{}
	for _, k := range kinds {
		has[k] = true
	}
	scenes, _, err := c.Scenes(ctx)
	if err != nil {
		return fmt.Errorf("E-DIR-003: %s: %w", rc.Name, err)
	}
	exists := map[string]bool{}
	for _, s := range scenes {
		exists[s] = true
	}
	base := strings.TrimRight(webBase, "/")
	type want struct{ scene, input, url string }
	var wants []want
	local := strings.Contains(rc.URL, "127.0.0.1") || strings.Contains(rc.URL, "localhost")
	if local {
		if rc.SceneReplay != "" {
			wants = append(wants, want{rc.SceneReplay, "RubbleSAR presenter page", base + "/ops/?present=1&theme=dark"})
		}
		if rc.SceneLive != "" && rc.SceneLive != rc.SceneReplay {
			wants = append(wants, want{rc.SceneLive, "RubbleSAR operator view page", base + "/ops/?theme=dark"})
		}
	}
	for _, w := range wants {
		if !has["browser_source"] {
			return fmt.Errorf("E-DIR-003: %s: this OBS has no browser source (install OBS from its PPA or Flatpak, which include it)", rc.Name)
		}
		if !exists[w.scene] {
			if err := c.CreateScene(ctx, w.scene); err != nil {
				return fmt.Errorf("E-DIR-003: %s: %w", rc.Name, err)
			}
			exists[w.scene] = true
			fmt.Fprintf(out, "    created scene %q\n", w.scene)
		}
		settings := map[string]any{"url": w.url, "width": cutW, "height": cutH, "fps": 30, "fps_custom": true, "reroute_audio": false,
			"shutdown": false, "restart_when_active": false, "css": ""}
		err := c.CreateInput(ctx, w.scene, w.input, "browser_source", settings)
		var re *obs.RequestError
		if errors.As(err, &re) && re.Code == 601 { // exists: point it at the page again
			err = c.SetInputSettings(ctx, w.input, settings)
		}
		if err != nil {
			return fmt.Errorf("E-DIR-003: %s: %w", rc.Name, err)
		}
		fmt.Fprintf(out, "    scene %q shows %s (browser source %q, 1920x1080)\n", w.scene, w.url, w.input)
	}
	if !local {
		kind := ""
		for _, k := range []string{"window_capture", "xcomposite_input", "pipewire-window-capture-source", "monitor_capture", "pipewire-screen-capture-source"} {
			if has[k] {
				kind = k
				break
			}
		}
		for _, s := range []string{rc.SceneLive, rc.SceneReplay} {
			if s == "" || exists[s] {
				if s != "" {
					fmt.Fprintf(out, "    scene %q exists (leave it showing the WinTAK window)\n", s)
				}
				continue
			}
			if err := c.CreateScene(ctx, s); err != nil {
				return fmt.Errorf("E-DIR-003: %s: %w", rc.Name, err)
			}
			exists[s] = true
			if kind != "" {
				if err := c.CreateInput(ctx, s, s+" window", kind, map[string]any{}); err != nil {
					fmt.Fprintf(out, "    WARNING: %v\n", err)
				}
			}
			fmt.Fprintf(out, "    created scene %q with a %s source: in OBS on that machine, open the source's properties and choose the WinTAK window\n", s, nonEmpty(kind, "(no capture)"))
		}
	}
	return nil
}

// TakTeamsLine says which TAK teams (rescue-dispatch's source = "tak") have a
// device reporting now: only those are sent casualties, so the TAK chapter of a
// live take needs one, and each online one must answer or lose its casualty.
func TakTeamsLine(teams []*live.Team) CheckLine {
	var on, off []string
	for _, t := range teams {
		if t == nil || t.Source != "tak" {
			continue
		}
		if t.State == "offline" || t.State == "" {
			off = append(off, t.Callsign)
		} else {
			on = append(on, t.Callsign)
		}
	}
	l := CheckLine{What: "TAK teams"}
	switch {
	case len(on)+len(off) == 0:
		l.Warn = true
		l.Detail = "none in rescue-dispatch's teams: no TAK device will be sent a casualty (add a [[dispatch.team]] with source = \"tak\" and the device's callsign to /etc/rubblesar/sar.toml, then sudo systemctl restart rescue-dispatch)"
	case len(on) == 0:
		l.Warn = true
		l.Detail = "none online (" + strings.Join(off, ", ") + " offline): the TAK chapter will show the map but no casualty sent to a device. Set the device's callsign to one of these exactly (or add yours to /etc/rubblesar/sar.toml), give it a position at the incident and wait for its first position report"
	default:
		l.Detail = strings.Join(on, ", ") + " online: each will be sent casualties; answer ok within tak_ack_timeout (3 min) and done when finished, or close that app"
		if len(off) > 0 {
			l.Detail += "; offline: " + strings.Join(off, ", ")
		}
		if len(on) > 1 {
			l.Warn = true
		}
	}
	return l
}

// CheckLine is one line of `rubblesar demo check`.
type CheckLine struct {
	What, Detail string
	Err          error
	Warn         bool
}

// Check tests what the takes need.
func Check(ctx context.Context, cfg *Config, w *Web, out io.Writer) []CheckLine {
	var lines []CheckLine
	add := func(l CheckLine) {
		lines = append(lines, l)
		st := "ok  "
		if l.Err != nil {
			st = "FAIL"
		} else if l.Warn {
			st = "WARN"
		}
		msg := l.Detail
		if l.Err != nil {
			msg = l.Err.Error()
		}
		fmt.Fprintf(out, "  [%s] %-24s %s\n", st, l.What, msg)
	}
	add(CheckLine{What: "settings", Detail: cfg.Path})
	sb, err := LoadStoryboard(cfg.Demo.Storyboard)
	if err != nil {
		add(CheckLine{What: "storyboard", Err: err})
	} else {
		add(CheckLine{What: "storyboard", Detail: fmt.Sprintf("%s: %d chapters", sb.Source, len(sb.Chapters))})
	}
	if st, err := w.ReplayState(); err != nil {
		add(CheckLine{What: "sar-web", Err: err})
	} else {
		d := fmt.Sprintf("%s: %d recorded runs", w.Base, len(st.Runs))
		add(CheckLine{What: "sar-web", Detail: d, Warn: len(st.Runs) == 0})
	}
	if n, err := w.Presenters(); err != nil {
		add(CheckLine{What: "presenter page", Err: err})
	} else if n == 0 {
		add(CheckLine{What: "presenter page", Warn: true, Detail: "none open: OBS's presenter scene (rubblesar demo obs-setup), or a browser at " + w.Base + "/ops/?present=1"})
	} else {
		add(CheckLine{What: "presenter page", Detail: fmt.Sprintf("%d open", n)})
	}
	for _, rc := range cfg.Recorders {
		if rc.Enabled != nil && !*rc.Enabled {
			add(CheckLine{What: "recorder " + rc.Name, Detail: "disabled (enabled = false)"})
			continue
		}
		r := NewRecorder(rc)
		for _, take := range []string{"live", "replay"} {
			if !rc.On(take) {
				continue
			}
			d, err := r.Check(ctx, take)
			add(CheckLine{What: fmt.Sprintf("recorder %s (%s)", rc.Name, take), Detail: d, Err: err})
		}
		r.Close()
	}
	if pic, err := w.Picture("live"); err != nil {
		add(CheckLine{What: "TAK teams", Warn: true, Detail: "not known: " + err.Error()})
	} else {
		add(TakTeamsLine(pic.Teams))
	}
	if p, err := exec.LookPath("ffmpeg"); err != nil {
		add(CheckLine{What: "ffmpeg", Warn: true, Detail: "not installed: needed for the rough cut (sudo apt install ffmpeg)"})
	} else {
		add(CheckLine{What: "ffmpeg", Detail: p})
	}
	if cfg.Demo.TTS != "" {
		prog := strings.Fields(cfg.Demo.TTS)[0]
		if _, err := exec.LookPath(prog); err != nil {
			add(CheckLine{What: "text to speech", Warn: true, Detail: prog + " not found"})
		} else {
			add(CheckLine{What: "text to speech", Detail: cfg.Demo.TTS})
		}
	}
	return lines
}
