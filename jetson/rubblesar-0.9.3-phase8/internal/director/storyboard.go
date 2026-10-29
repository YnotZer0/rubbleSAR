// Package director is the demo director (phase 8, `rubblesar demo`): it
// records a live take of a mission with staged link events, replays a
// recorded run as a narrated presentation in the operator view, controls the
// recorders (OBS on the ZBook and the WinTAK laptop, scrcpy for an ATAK
// phone), renders what the drones saw, and writes the cue sheet, the
// narration, the subtitles and a rough cut of the video.
package director

import (
	"bytes"
	_ "embed"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed storyboard.toml
var defaultStoryboard []byte

// DefaultStoryboard is the storyboard built into this release.
func DefaultStoryboard() []byte { return append([]byte(nil), defaultStoryboard...) }

// Storyboard is the chapters of the narrated replay take.
type Storyboard struct {
	Title          string  `toml:"title"`
	WordsPerMinute float64 `toml:"words_per_minute"`
	// CommandPost names the command post's computer in the video
	// ({{.CommandPost}} in the narration; the panels use it too).
	CommandPost string    `toml:"command_post"`
	Chapters    []Chapter `toml:"chapter"`
	Source      string    `toml:"-"`
}

// Chapter is one part of the video.
type Chapter struct {
	ID           string            `toml:"id"`
	Title        string            `toml:"title"`
	Narration    string            `toml:"narration"`
	Caption      string            `toml:"caption"`
	MinSeconds   float64           `toml:"min_seconds"`
	Card         string            `toml:"card"`
	CardTitle    string            `toml:"card_title"`
	CardSubtitle string            `toml:"card_subtitle"`
	CardLines    []string          `toml:"card_lines"`
	Panel        string            `toml:"panel"`
	Map          string            `toml:"map"`
	MapMS        int               `toml:"map_ms"`
	Replay       string            `toml:"replay"`
	Speed        *float64          `toml:"speed"`
	Focus        string            `toml:"focus"`
	Clock        string            `toml:"clock"`
	Scenes       map[string]string `toml:"scenes"`
	Insert       string            `toml:"insert"`
	Data         map[string]any    `toml:"data"`
}

// DefaultCommandPost is how the video names the command post's computer
// when the storyboard does not say.
const DefaultCommandPost = "a GPU-equipped laptop"

// Panels the presenter page knows.
var Panels = map[string]bool{"": true, "scenario": true, "drones": true, "camera": true, "model": true, "packet": true, "radio": true,
	"zbook": true, "triage": true, "tak": true, "aar": true, "casualty": true, "found": true}

// LoadStoryboard reads a storyboard file ("" = the built-in one).
func LoadStoryboard(path string) (*Storyboard, error) {
	b, src := defaultStoryboard, "built-in storyboard"
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("E-DIR-001: storyboard: %w", err)
		}
		src = path
	}
	var sb Storyboard
	md, err := toml.Decode(string(b), &sb)
	if err != nil {
		return nil, fmt.Errorf("E-DIR-001: %s: %w", src, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		var names []string
		for _, k := range und {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("E-DIR-001: %s: unknown setting(s) %s (typo?)", src, strings.Join(names, ", "))
	}
	sb.Source = src
	if sb.WordsPerMinute <= 0 {
		sb.WordsPerMinute = 150
	}
	if strings.TrimSpace(sb.CommandPost) == "" {
		sb.CommandPost = DefaultCommandPost
	}
	return &sb, sb.Validate()
}

// beatNameRE takes the beat's name from a moment ("first-red+30s" -> "first-red").
var beatNameRE = regexp.MustCompile(`^([a-z][a-z]*(?:-[a-z]+)*)(?:[+-][0-9.]+s?)?$`)

// Beats are the beats the chapters jump to, focus on or take inserts from, in order.
func (sb *Storyboard) Beats() []string {
	var out []string
	seen := map[string]bool{"start": true, "end": true}
	add := func(m string) {
		if g := beatNameRE.FindStringSubmatch(strings.TrimSpace(m)); g != nil && !seen[g[1]] {
			seen[g[1]] = true
			out = append(out, g[1])
		}
	}
	for _, c := range sb.Chapters {
		add(c.Replay)
		add(c.Focus)
		if g := insertRE.FindStringSubmatch(c.Insert); g != nil {
			add(g[2])
		}
	}
	return out
}

// insertRE: "<recorder>[|<recorder>...]@<moment>~<seconds>[@<delay>]"; with
// several recorders the first that recorded the live take is used.
var insertRE = regexp.MustCompile(`^([a-z0-9_-]+(?:\|[a-z0-9_-]+)*)@([a-z0-9:+.-]+)~([0-9.]+)(?:@([0-9.]+))?$`)

// Validate checks the storyboard (not the run: see Plan).
func (sb *Storyboard) Validate() error {
	if len(sb.Chapters) == 0 {
		return fmt.Errorf("E-DIR-001: %s has no chapters", sb.Source)
	}
	seen := map[string]bool{}
	var bad []string
	for i, c := range sb.Chapters {
		name := c.ID
		if name == "" {
			name = fmt.Sprintf("chapter %d", i+1)
			bad = append(bad, name+": no id")
		}
		if seen[c.ID] {
			bad = append(bad, name+": id used twice")
		}
		seen[c.ID] = true
		if !Panels[c.Panel] {
			bad = append(bad, fmt.Sprintf("%s: unknown panel %q", name, c.Panel))
		}
		switch c.Card {
		case "", "title", "section", "end":
		default:
			bad = append(bad, fmt.Sprintf("%s: card must be title, section or end", name))
		}
		if c.Map != "" && !validMap(c.Map) {
			bad = append(bad, fmt.Sprintf("%s: map %q (fit, sites, site:<id>, follow:<drone>, casualty:<beat>, view:<lat>,<lon>,<zoom>)", name, c.Map))
		}
		if c.Insert != "" && !insertRE.MatchString(c.Insert) {
			bad = append(bad, fmt.Sprintf("%s: insert %q (recorder@beat+/-offset~seconds[@delay])", name, c.Insert))
		}
		if strings.TrimSpace(c.Narration) == "" && c.MinSeconds <= 0 {
			bad = append(bad, name+": no narration and no min_seconds")
		}
		for _, t := range append([]string{c.Narration, c.Caption, c.CardTitle, c.CardSubtitle}, c.CardLines...) {
			if _, err := template.New("").Funcs(funcs).Parse(t); err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("E-DIR-001: %s: %s", sb.Source, strings.Join(bad, "; "))
	}
	return nil
}

func validMap(m string) bool {
	switch {
	case m == "fit" || m == "sites":
		return true
	case strings.HasPrefix(m, "site:"), strings.HasPrefix(m, "follow:"), strings.HasPrefix(m, "casualty:"):
		return len(m) > strings.Index(m, ":")+1
	case strings.HasPrefix(m, "view:"):
		var a, b, c float64
		_, err := fmt.Sscanf(strings.TrimPrefix(m, "view:"), "%g,%g,%g", &a, &b, &c)
		return err == nil
	}
	return false
}

var numberWords = []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

var funcs = template.FuncMap{
	// words spells small numbers out ("five buildings").
	"words": func(v any) string {
		n, ok := toInt(v)
		if ok && n >= 0 && n < len(numberWords) {
			return numberWords[n]
		}
		return fmt.Sprint(v)
	},
	"plural": func(n any, one, many string) string {
		if k, ok := toInt(n); ok && k == 1 {
			return one
		}
		return many
	},
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), x == math.Trunc(x)
	case uint32:
		return int(x), true
	case uint64:
		return int(x), true
	}
	return 0, false
}

// Fill puts the run's facts into a text.
func Fill(text string, f *Facts) (string, error) {
	t, err := template.New("").Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, f); err != nil {
		return "", err
	}
	return tidy(b.String()), nil
}

var spaces = regexp.MustCompile(`[ \t\r\n]+`)

func tidy(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// Words counts the words of a text.
func Words(s string) int { return len(strings.Fields(s)) }

// Segment is a caption shown during part of a chapter (also a subtitle).
type Segment struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"` // seconds from the chapter's start
	End   float64 `json:"end"`
}

// maxSegmentWords: a caption longer than this is split (two lines of the caption bar).
const maxSegmentWords = 24

// Segments splits a narration into captions timed by their words over d.
func Segments(narration string, d time.Duration) []Segment {
	var parts []string
	for _, s := range sentences(narration) {
		parts = append(parts, splitLong(s, maxSegmentWords)...)
	}
	total := 0
	for _, p := range parts {
		total += Words(p)
	}
	if total == 0 {
		return nil
	}
	var out []Segment
	at, sec := 0.0, d.Seconds()
	for _, p := range parts {
		share := sec * float64(Words(p)) / float64(total)
		out = append(out, Segment{Text: p, Start: math.Round(at*100) / 100, End: math.Round((at+share)*100) / 100})
		at += share
	}
	return out
}

// sentences splits after . ! ? followed by a space and a capital letter or digit.
func sentences(s string) []string {
	s = tidy(s)
	var out []string
	start := 0
	for i := 0; i < len(s)-2; i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && s[i+1] == ' ' {
			c := s[i+2]
			if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80 {
				out = append(out, strings.TrimSpace(s[start:i+1]))
				start = i + 2
			}
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// splitLong splits a sentence at the comma, colon or semicolon nearest its middle while it is too long.
func splitLong(s string, max int) []string {
	w := strings.Fields(s)
	if len(w) <= max {
		return []string{s}
	}
	best, bestD := -1, len(w)
	for i := 2; i < len(w)-2; i++ {
		last := w[i][len(w[i])-1]
		if last == ',' || last == ';' || last == ':' {
			if d := abs(i + 1 - len(w)/2); d < bestD {
				best, bestD = i, d
			}
		}
	}
	if best < 0 {
		best = len(w)/2 - 1
	}
	a, b := strings.Join(w[:best+1], " "), strings.Join(w[best+1:], " ")
	return append(splitLong(a, max), splitLong(b, max)...)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Duration is how long a chapter lasts: its narration at the storyboard's pace (plus a breath), at least min_seconds.
func (sb *Storyboard) Duration(c Chapter, narration string) time.Duration {
	s := float64(Words(narration))/sb.WordsPerMinute*60 + 1.2
	if s < c.MinSeconds {
		s = c.MinSeconds
	}
	return time.Duration(math.Round(s*10) / 10 * float64(time.Second))
}
