package director

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Layout of the presenter page in a 1920x1080 recording: inserts cover the
// map and panel, between the header and the caption bar, so the captions
// stay visible.
const (
	cutW, cutH         = 1920, 1080
	insertTop, insertH = 56, 848
)

// CutOptions configure the rough cut.
type CutOptions struct {
	// Take: the replay take's folder; Live: the live take's folder ("" = none).
	Take, Live string
	// Main: the replay take's recording (default the ZBook recorder's file in cues.json).
	Main string
	// Clips: recorder name -> its live-take file copied to this ZBook (e.g. wintak=/home/you/wintak.mkv).
	Clips map[string]string
	// Out: the video to write (default <take>/rough-cut.mp4).
	Out    string
	DryRun bool
	Log    io.Writer
}

// CutPlan is what the rough cut is made of.
type CutPlan struct {
	Args     []string
	Out      string
	Inserts  []string
	Voice    int
	Seconds  float64
	Warnings []string
}

// PlanCut works out the ffmpeg command for a rough cut.
func PlanCut(o CutOptions) (*CutPlan, error) {
	cs, err := LoadCueSheet(o.Take)
	if err != nil {
		return nil, err
	}
	if cs.Kind != "replay" || len(cs.Chapters) == 0 {
		return nil, fmt.Errorf("E-DIR-006: %s is not a replay take with chapters", o.Take)
	}
	var live *CueSheet
	if o.Live != "" {
		if live, err = LoadCueSheet(o.Live); err != nil {
			return nil, err
		}
	}
	main := cs.Main()
	if main == nil && o.Main == "" {
		return nil, fmt.Errorf("E-DIR-006: take %s has no recording (a rehearsal?): record one with rubblesar demo replay, or give --main FILE", cs.Take)
	}
	mainFile, mainStart := o.Main, cs.Started
	if main != nil {
		mainStart = main.Started
		if mainFile == "" {
			mainFile = main.Output
		}
	}
	if _, err := os.Stat(mainFile); err != nil {
		return nil, fmt.Errorf("E-DIR-006: the main recording %q is not on this ZBook: %v (copy it here and give --main FILE)", mainFile, err)
	}
	p := &CutPlan{Out: o.Out}
	if p.Out == "" {
		p.Out = filepath.Join(o.Take, "rough-cut.mp4")
	}
	first := cs.Chapters[0].Started.Sub(mainStart).Seconds()
	last := cs.Chapters[len(cs.Chapters)-1]
	end := last.Started.Sub(mainStart).Seconds() + last.Seconds
	if first < 0 {
		first = 0
	}
	p.Seconds = end - first
	args := []string{"-hide_banner", "-y", "-ss", f3(first), "-t", f3(p.Seconds), "-i", mainFile}
	var graph []string
	graph = append(graph, fmt.Sprintf("[0:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=30[v0]", cutW, cutH, cutW, cutH))
	cur := "v0"
	input := 1
	font := firstFile("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf")
	// Inserts from the live take.
	for _, c := range cs.Chapters {
		in := c.Insert
		if in == nil {
			continue
		}
		// The first of the insert's recorders with footage of the live take on this ZBook.
		file, start, name, missing := "", time.Time{}, in.Recorder, ""
		for _, rn := range strings.Split(in.Recorder, "|") {
			f, st := o.Clips[rn], time.Time{}
			if live != nil {
				for _, r := range live.Recorders {
					if r.Name == rn && r.Error == "" {
						st = r.Started
						if f == "" {
							f = r.Output
						}
					}
				}
			}
			if f == "" || st.IsZero() {
				continue
			}
			if _, err := os.Stat(f); err != nil {
				if missing == "" {
					missing = fmt.Sprintf("%s is not on this ZBook (copy it here and give --clip %s=FILE)", f, rn)
				}
				continue
			}
			file, start, name = f, st, rn
			break
		}
		if file == "" && missing != "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("chapter %s: %s: no insert", c.ID, missing))
			continue
		}
		if file == "" {
			first := strings.Split(in.Recorder, "|")[0]
			p.Warnings = append(p.Warnings, fmt.Sprintf("chapter %s: no %s recording from a live take (give --live and --clip %s=FILE): no insert", c.ID, strings.ReplaceAll(in.Recorder, "|", " or "), first))
			continue
		}
		off := time.UnixMilli(in.At).Sub(start).Seconds()
		if off < 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("chapter %s: the %s recording started %.0f s after %s: the insert starts at its beginning", c.ID, name, -off, in.Beat))
			off = 0
		}
		dur := in.Seconds
		if room := c.Seconds - in.Delay; dur > room {
			dur = room
		}
		at := c.Started.Sub(mainStart).Seconds() - first + in.Delay
		args = append(args, "-ss", f3(off), "-t", f3(dur), "-i", file)
		lab := ""
		if font != "" {
			lab = fmt.Sprintf(",drawtext=fontfile=%s:text='%s, recorded live':x=24:y=20:fontsize=30:fontcolor=white:box=1:boxcolor=0x000000AA:boxborderw=10",
				font, ffText(recorderLabel(name)))
		}
		graph = append(graph, fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=0x181b1e,setsar=1,fps=30%s,setpts=PTS-STARTPTS+%s/TB[i%d]",
			input, cutW, insertH, cutW, insertH, lab, f3(at), input))
		next := fmt.Sprintf("v%d", input)
		graph = append(graph, fmt.Sprintf("[%s][i%d]overlay=x=0:y=%d:eof_action=pass:enable='between(t,%s,%s)'[%s]", cur, input, insertTop, f3(at), f3(at+dur), next))
		cur = next
		p.Inserts = append(p.Inserts, fmt.Sprintf("%s: %.0f s of %s from %s, at %s", c.ID, dur, filepath.Base(file), tc(time.Duration(off*float64(time.Second)), false), tc(time.Duration(at*float64(time.Second)), false)))
		input++
	}
	// A voice track, if made (rubblesar demo voice).
	var voices []string
	for i, c := range cs.Chapters {
		w := filepath.Join(o.Take, "voice", fmt.Sprintf("%02d-%s.wav", i+1, c.ID))
		if _, err := os.Stat(w); err != nil {
			continue
		}
		at := c.Started.Sub(mainStart).Seconds() - first
		args = append(args, "-i", w)
		ms := int(at * 1000)
		graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,adelay=%d|%d[a%d]", input, ms, ms, input))
		voices = append(voices, fmt.Sprintf("[a%d]", input))
		input++
	}
	p.Voice = len(voices)
	if len(voices) > 0 {
		graph = append(graph, fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[aout]", strings.Join(voices, ""), len(voices)))
	}
	args = append(args, "-filter_complex", strings.Join(graph, ";"), "-map", "["+cur+"]")
	if len(voices) > 0 {
		args = append(args, "-map", "[aout]", "-c:a", "aac", "-b:a", "160k")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-t", f3(p.Seconds), "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p", "-movflags", "+faststart", p.Out)
	p.Args = args
	return p, nil
}

// recorderLabel is how a recorder is named on the video ("WinTAK", "ATAK").
func recorderLabel(name string) string {
	switch strings.ToLower(name) {
	case "wintak":
		return "WinTAK"
	case "atak":
		return "ATAK"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// Cut makes the rough cut with ffmpeg.
func Cut(ctx context.Context, o CutOptions) (*CutPlan, error) {
	p, err := PlanCut(o)
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		return p, nil
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return p, fmt.Errorf("E-DIR-007: ffmpeg is not installed (sudo apt install ffmpeg)")
	}
	cmd := exec.CommandContext(ctx, ff, p.Args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if o.Log != nil {
		cmd.Stderr = io.MultiWriter(&stderr, o.Log)
	}
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		return p, fmt.Errorf("E-DIR-007: ffmpeg: %v\n%s", err, tail)
	}
	return p, nil
}

func f3(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func ffText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`, `%`, `\%`)
	return r.Replace(s)
}

func firstFile(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// --- Voice ------------------------------------------------------------------------------------------

// Voice speaks each chapter's narration with the [demo] tts command into <take>/voice/.
func Voice(ctx context.Context, take, command string, out io.Writer) ([]string, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("E-DIR-007: no text-to-speech command: set [demo] tts in demo.toml (or record the voice-over from narration.txt yourself)")
	}
	cs, err := LoadCueSheet(take)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(take, "voice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("E-DIR-007: %w", err)
	}
	var made []string
	for i, c := range cs.Chapters {
		w := filepath.Join(dir, fmt.Sprintf("%02d-%s.wav", i+1, c.ID))
		line := strings.ReplaceAll(command, "{out}", shellQuote(w))
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
		cmd.Stdin = strings.NewReader(c.Narration + "\n")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return made, fmt.Errorf("E-DIR-007: tts for chapter %s: %v: %s", c.ID, err, strings.TrimSpace(stderr.String()))
		}
		if _, err := os.Stat(w); err != nil {
			return made, fmt.Errorf("E-DIR-007: tts for chapter %s wrote no %s", c.ID, w)
		}
		made = append(made, w)
		if d, ok := wavSeconds(w); ok && out != nil {
			warn := ""
			if d > c.Seconds {
				warn = fmt.Sprintf(" (longer than the chapter's %.0f s: raise its min_seconds or shorten the narration)", c.Seconds)
			}
			fmt.Fprintf(out, "    %s: %.1f s%s\n", filepath.Base(w), d, warn)
		}
	}
	sort.Strings(made)
	return made, nil
}

// wavSeconds reads a WAV file's length from its header.
func wavSeconds(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, false
	}
	var rate, block uint32
	for i := 12; i+8 <= len(b); {
		id, size := string(b[i:i+4]), le32(b[i+4:i+8])
		body := i + 8
		if id == "fmt " && body+16 <= len(b) {
			rate, block = le32(b[body+4:body+8]), uint32(b[body+12])|uint32(b[body+13])<<8
		}
		if id == "data" && rate > 0 && block > 0 {
			if size == 0xffffffff || int(size) > len(b)-body {
				size = uint32(len(b) - body)
			}
			return float64(size) / float64(block) / float64(rate), true
		}
		i = body + int(size) + int(size&1)
	}
	return 0, false
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
