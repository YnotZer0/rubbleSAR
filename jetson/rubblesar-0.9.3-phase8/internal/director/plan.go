package director

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"rubblesar/internal/live"
	"rubblesar/internal/web"
)

// Planned is a chapter worked out against a run: its narration filled in,
// how long it lasts, its captions, where the replay goes and what the map,
// the panel and the card show.
type Planned struct {
	Chapter   Chapter           `json:"-"`
	ID        string            `json:"id"`
	Index     int               `json:"index"`
	Title     string            `json:"title"`
	Narration string            `json:"narration"`
	Seconds   float64           `json:"seconds"`
	Segments  []Segment         `json:"segments"`
	Seek      string            `json:"seek,omitempty"`
	SeekAt    int64             `json:"seek_at,omitempty"` // unix ms in the run
	Speed     *float64          `json:"speed,omitempty"`
	Focus     string            `json:"focus,omitempty"`
	Map       *web.MapCue       `json:"map,omitempty"`
	Card      *web.Card         `json:"card,omitempty"`
	Panel     string            `json:"panel,omitempty"`
	Data      map[string]any    `json:"data,omitempty"`
	Scenes    map[string]string `json:"scenes,omitempty"`
	Insert    *Insert           `json:"insert,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// Insert is footage from the live take placed over a chapter in the rough cut.
type Insert struct {
	Recorder string  `json:"recorder"`
	Beat     string  `json:"beat"`
	At       int64   `json:"at"`      // unix ms: where the footage starts (the beat plus its offset)
	Seconds  float64 `json:"seconds"` // how long
	Delay    float64 `json:"delay"`   // seconds into the chapter
}

// Plan is the whole replay take.
type Plan struct {
	Run      string     `json:"run"`
	RunDir   string     `json:"run_dir"`
	Incident string     `json:"incident"`
	Facts    *Facts     `json:"facts"`
	Chapters []*Planned `json:"chapters"`
	Seconds  float64    `json:"seconds"`
	Warnings []string   `json:"warnings,omitempty"`
}

// MakePlan works a storyboard out against a recorded run.
func MakePlan(sb *Storyboard, p *live.Player, f *Facts, m *Media) (*Plan, error) {
	inc := p.Meta.Incident
	if sb.CommandPost != "" {
		f.CommandPost = sb.CommandPost
	}
	pl := &Plan{Run: p.Run, RunDir: p.Dir, Incident: inc, Facts: f}
	refOf := func(beat string) (string, bool) {
		for _, b := range p.Beats() {
			if b.Name == beat {
				return b.Ref, b.Ref != ""
			}
		}
		return "", false
	}
	resolveCas := func(x string) (string, bool) {
		if strings.HasPrefix(x, "C-") {
			return x, true
		}
		base := x
		if i := strings.LastIndexAny(x, "+-"); i > 0 {
			if _, err := time.ParseDuration(x[i+1:]); err == nil {
				base = x[:i]
			}
		}
		return refOf(base)
	}
	common := map[string]any{"run": p.Run, "incident": inc, "media": "/reports/" + inc + "/" + p.Run + "/media/", "report": "/reports/" + inc + "/" + p.Run + "/report.json",
		"facts": f, "first_red": f.FirstRed, "blackout_uav": f.BlackoutUAV}
	if m != nil {
		common["has_media"] = true
	}
	for i, c := range sb.Chapters {
		pc := &Planned{Chapter: c, ID: c.ID, Index: i + 1, Title: c.Title, Panel: c.Panel, Speed: c.Speed, Scenes: c.Scenes}
		var err error
		if pc.Narration, err = Fill(c.Narration, f); err != nil {
			return nil, fmt.Errorf("E-DIR-001: chapter %s: %v", c.ID, err)
		}
		if pc.Title, err = Fill(c.Title, f); err != nil {
			return nil, fmt.Errorf("E-DIR-001: chapter %s: %v", c.ID, err)
		}
		d := sb.Duration(c, pc.Narration)
		pc.Seconds = d.Seconds()
		text := pc.Narration
		if c.Caption != "" {
			if text, err = Fill(c.Caption, f); err != nil {
				return nil, fmt.Errorf("E-DIR-001: chapter %s: %v", c.ID, err)
			}
		}
		pc.Segments = Segments(text, d)
		if c.Card != "" {
			cd := &web.Card{Kind: c.Card}
			cd.Title, _ = Fill(c.CardTitle, f)
			cd.Subtitle, _ = Fill(c.CardSubtitle, f)
			for _, l := range c.CardLines {
				if s, err := Fill(l, f); err == nil && s != "" {
					cd.Lines = append(cd.Lines, s)
				}
			}
			pc.Card = cd
		}
		if c.Replay != "" {
			if t, ok := p.Beat(c.Replay); ok {
				pc.Seek, pc.SeekAt = c.Replay, t.UnixMilli()
			} else {
				pc.Warnings = append(pc.Warnings, fmt.Sprintf("replay %q: no such moment in run %s; the replay goes on from where it is", c.Replay, p.Run))
			}
		}
		if c.Focus != "" {
			if id, ok := resolveCas(c.Focus); ok {
				pc.Focus = id
			} else {
				pc.Warnings = append(pc.Warnings, fmt.Sprintf("focus %q: no such casualty or beat in this run", c.Focus))
			}
		}
		if c.Map != "" {
			mc := &web.MapCue{Ms: c.MapMS}
			if mc.Ms == 0 {
				mc.Ms = 1600
			}
			kind, arg, _ := strings.Cut(c.Map, ":")
			switch kind {
			case "fit", "sites":
				mc.Mode = kind
			case "site", "follow":
				mc.Mode, mc.Target = kind, arg
			case "casualty":
				mc.Mode = "casualty"
				if id, ok := resolveCas(arg); ok {
					mc.Target = id
				} else {
					mc.Mode = "fit"
					pc.Warnings = append(pc.Warnings, fmt.Sprintf("map %q: no such casualty or beat; showing everything", c.Map))
				}
			case "view":
				v := strings.Split(arg, ",")
				mc.Mode = "view"
				mc.Lat, _ = strconv.ParseFloat(v[0], 64)
				mc.Lon, _ = strconv.ParseFloat(v[1], 64)
				mc.Zoom, _ = strconv.ParseFloat(v[2], 64)
			}
			pc.Map = mc
		}
		data := map[string]any{}
		for k, v := range common {
			data[k] = v
		}
		for k, v := range c.Data {
			data[k] = v
		}
		pc.Data = data
		if (c.Panel == "camera" || c.Panel == "model") && m == nil {
			pc.Warnings = append(pc.Warnings, "no media for this run yet: rubblesar demo media --run "+p.Run)
		}
		if c.Insert != "" {
			g := insertRE.FindStringSubmatch(c.Insert)
			in := &Insert{Recorder: g[1], Beat: g[2]}
			in.Seconds, _ = strconv.ParseFloat(g[3], 64)
			if g[4] != "" {
				in.Delay, _ = strconv.ParseFloat(g[4], 64)
			}
			if t, ok := p.Beat(g[2]); ok {
				in.At = t.UnixMilli()
				pc.Insert = in
			} else {
				pc.Warnings = append(pc.Warnings, fmt.Sprintf("insert %q: no such moment in this run, so no insert", c.Insert))
			}
		}
		pl.Chapters = append(pl.Chapters, pc)
		pl.Seconds += pc.Seconds
		for _, w := range pc.Warnings {
			pl.Warnings = append(pl.Warnings, c.ID+": "+w)
		}
	}
	return pl, nil
}

// Cue is the presenter's cue for a chapter, showing caption segment seg.
func (pc *Planned) Cue(count, seg int) web.Cue {
	c := web.Cue{Source: "replay", Chapter: pc.ID, Index: pc.Index, Count: count, Title: pc.Title, Panel: pc.Panel, Data: pc.Data,
		Map: pc.Map, Card: pc.Card, Focus: pc.Focus, Clock: pc.Chapter.Clock}
	if seg >= 0 && seg < len(pc.Segments) {
		c.Caption = pc.Segments[seg].Text
	}
	if pc.Card != nil && pc.Card.Kind != "section" {
		c.Title = "" // a title card carries its own heading; the caption still runs
	}
	return c
}
