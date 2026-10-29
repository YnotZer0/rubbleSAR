package director

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"rubblesar/internal/live"
	"rubblesar/internal/web"
)

// Web is sar-web on this ZBook, as the director controls it.
type Web struct {
	Base string
	c    *http.Client
}

// NewWeb talks to sar-web at base (http://127.0.0.1:8090).
func NewWeb(base string) *Web {
	return &Web{Base: strings.TrimRight(base, "/"), c: &http.Client{Timeout: 10 * time.Second}}
}

func (w *Web) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.c.Do(req)
	if err != nil {
		return fmt.Errorf("E-DIR-005: sar-web at %s: %w (sudo systemctl status sar-web)", w.Base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if resp.StatusCode == http.StatusNotFound {
			msg += " (sar-web is older than phase 8: scripts/zbook/30-install.sh)"
		}
		return fmt.Errorf("E-DIR-005: sar-web %s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Replay sends a replay command.
func (w *Web) Replay(c web.ReplayCommand) (*live.Replay, error) {
	var r live.Replay
	err := w.do("POST", "/v1/replay", c, &r)
	return &r, err
}

// ReplayState is what sar-web replays.
func (w *Web) ReplayState() (*web.ReplayState, error) {
	var s web.ReplayState
	err := w.do("GET", "/v1/replay", nil, &s)
	return &s, err
}

// Present sets the presenter's cue.
func (w *Web) Present(c web.Cue) (int, error) {
	var r struct {
		Presenters int `json:"presenters"`
	}
	err := w.do("POST", "/v1/present", c, &r)
	return r.Presenters, err
}

// Presenters is how many presenter pages are open (the OBS browser source is one).
func (w *Web) Presenters() (int, error) {
	var r struct {
		Presenters int `json:"presenters"`
	}
	err := w.do("GET", "/v1/present", nil, &r)
	return r.Presenters, err
}

// Picture is the live or replayed picture.
func (w *Web) Picture(src string) (*live.Picture, error) {
	var p live.Picture
	err := w.do("GET", "/v1/picture?src="+src, nil, &p)
	return &p, err
}
