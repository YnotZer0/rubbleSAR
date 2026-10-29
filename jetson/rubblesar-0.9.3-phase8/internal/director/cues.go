package director

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CueSheet is a take's record (cues.json): when each recorder started and
// where its file is, when each chapter and beat happened, so the recordings
// can be lined up and cut.
type CueSheet struct {
	Take      string       `json:"take"`
	Kind      string       `json:"kind"` // live, replay
	Run       string       `json:"run"`
	RunDir    string       `json:"run_dir,omitempty"`
	Started   time.Time    `json:"started"`
	Ended     time.Time    `json:"ended"`
	Rehearsal bool         `json:"rehearsal,omitempty"`
	Recorders []Recording  `json:"recorders"`
	Chapters  []ChapterCue `json:"chapters,omitempty"`
	Beats     []BeatCue    `json:"beats,omitempty"`
	Events    []string     `json:"events,omitempty"`
	Notes     []string     `json:"notes,omitempty"`
}

// Recording is one recorder's part in a take.
type Recording struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`
	Started time.Time `json:"started,omitempty"`
	Stopped time.Time `json:"stopped,omitempty"`
	// Output: the file, on the recorder's own machine (copy files from other machines to the ZBook for the cut).
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ChapterCue is when a chapter played.
type ChapterCue struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Started   time.Time `json:"started"`
	Seconds   float64   `json:"seconds"`
	Narration string    `json:"narration"`
	Segments  []Segment `json:"segments"`
	Insert    *Insert   `json:"insert,omitempty"`
	RunAt     int64     `json:"run_at,omitempty"` // the replay's position when the chapter began (unix ms)
}

// BeatCue is when a beat happened (wall clock: the run's time in a live take).
type BeatCue struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	Ref  string    `json:"ref,omitempty"`
	Text string    `json:"text,omitempty"`
}

// Main is the recording the times of the narration refer to: the ZBook's OBS
// (named "zbook"), else the first one that recorded; zero: none (a rehearsal).
func (cs *CueSheet) Main() *Recording {
	for i := range cs.Recorders {
		if cs.Recorders[i].Name == "zbook" && !cs.Recorders[i].Started.IsZero() {
			return &cs.Recorders[i]
		}
	}
	for i := range cs.Recorders {
		if !cs.Recorders[i].Started.IsZero() && cs.Recorders[i].Kind == "obs" {
			return &cs.Recorders[i]
		}
	}
	return nil
}

// Zero is the moment times in the narration and subtitles count from.
func (cs *CueSheet) Zero() time.Time {
	if m := cs.Main(); m != nil {
		return m.Started
	}
	return cs.Started
}

// Save writes cues.json into dir.
func (cs *CueSheet) Save(dir string) error {
	b, _ := json.MarshalIndent(cs, "", "  ")
	return os.WriteFile(filepath.Join(dir, "cues.json"), append(b, '\n'), 0o644)
}

// LoadCueSheet reads a take's cues.json.
func LoadCueSheet(dir string) (*CueSheet, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cues.json"))
	if err != nil {
		return nil, fmt.Errorf("E-DIR-006: %s is not a take (no cues.json): %w", dir, err)
	}
	var cs CueSheet
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("E-DIR-006: %s/cues.json: %w", dir, err)
	}
	return &cs, nil
}

// tc is a timecode: h:mm:ss.mmm (SRT uses a comma).
func tc(d time.Duration, comma bool) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	sep := "."
	if comma {
		sep = ","
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

func short(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// WriteScript writes narration.txt, narration.srt and shotlist.md for a replay take.
func (cs *CueSheet) WriteScript(dir string, live *CueSheet) error {
	zero := cs.Zero()
	var txt, srt, shot strings.Builder
	main := "the take"
	if m := cs.Main(); m != nil {
		main = fmt.Sprintf("the %s recording (%s)", m.Name, nonEmpty(m.Output, "file name in OBS"))
	}
	fmt.Fprintf(&txt, "RubbleSAR demo: narration for take %s (run %s)\nTimes are from the start of %s.\n", cs.Take, cs.Run, main)
	fmt.Fprintf(&txt, "Read at about the pace of the captions; each chapter lasts as long as shown.\n\n")
	fmt.Fprintf(&shot, "# Shot list: take %s\n\nRun %s, replayed. Times are from the start of %s.\n\n", cs.Take, cs.Run, main)
	n := 0
	var total time.Duration
	for i, c := range cs.Chapters {
		at := c.Started.Sub(zero)
		end := at + time.Duration(c.Seconds*float64(time.Second))
		total = end
		fmt.Fprintf(&txt, "[%s - %s] %d. %s\n%s\n\n", short(at), short(end), i+1, c.Title, c.Narration)
		fmt.Fprintf(&shot, "## %d. %s (%s to %s, %.0f s)\n\n%s\n\n", i+1, c.Title, short(at), short(end), c.Seconds, c.Narration)
		if c.Insert != nil {
			fmt.Fprintf(&shot, "- Insert: %s\n", insertWords(c.Insert, live))
		}
		shot.WriteString("\n")
		for _, s := range c.Segments {
			n++
			a := at + time.Duration(s.Start*float64(time.Second))
			b := at + time.Duration(s.End*float64(time.Second))
			fmt.Fprintf(&srt, "%d\n%s --> %s\n%s\n\n", n, tc(a, true), tc(b, true), wrapCaption(s.Text, 46))
		}
	}
	fmt.Fprintf(&txt, "Total: %s\n", short(total))
	fmt.Fprintf(&shot, "Total: %s.\n", short(total))
	for _, f := range []struct{ name, body string }{{"narration.txt", txt.String()}, {"narration.srt", srt.String()}, {"shotlist.md", shot.String()}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			return fmt.Errorf("E-DIR-006: %w", err)
		}
	}
	return nil
}

// insertWords says where an insert's footage is in the live take's recording.
func insertWords(in *Insert, live *CueSheet) string {
	at := time.UnixMilli(in.At)
	s := fmt.Sprintf("%.0f s of the %s recording from the live take, from %s (%s), placed %.0f s into the chapter", in.Seconds, in.Recorder, in.Beat, at.Format("15:04:05"), in.Delay)
	if live != nil {
		for _, r := range live.Recorders {
			if r.Name == in.Recorder && !r.Started.IsZero() {
				return s + fmt.Sprintf(": file %s at %s", nonEmpty(r.Output, "?"), tc(at.Sub(r.Started), false))
			}
		}
	}
	return s + " (record a live take with this recorder, or place it by hand)"
}

// wrapCaption breaks a subtitle into at most two lines near the middle.
func wrapCaption(s string, width int) string {
	if len(s) <= width {
		return s
	}
	w := strings.Fields(s)
	best, bestD := 0, len(s)
	n := 0
	for i := 0; i < len(w)-1; i++ {
		n += len(w[i]) + 1
		if d := abs(n - len(s)/2); d < bestD {
			best, bestD = i+1, d
		}
	}
	return strings.Join(w[:best], " ") + "\n" + strings.Join(w[best:], " ")
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// Takes lists the takes in a folder, newest first.
func Takes(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "cues.json")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
