// Package check implements sar-check's acceptance tests. Every check produces
// a Result with a status, an error code on failure and the evidence behind it,
// so a failed run can be pasted back as one block.
package check

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/version"
)

// Status of one check.
type Status string

const (
	Pass Status = "PASS"
	Warn Status = "WARN"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
	// Todo marks something a later setup step installs; it is not a failure.
	Todo Status = "TODO"
)

// Result of one check.
type Result struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Status   Status    `json:"status"`
	Code     diag.Code `json:"code,omitempty"`
	Detail   string    `json:"detail"`
	Evidence []string  `json:"evidence,omitempty"`
	Hint     string    `json:"hint,omitempty"`
	Seconds  float64   `json:"seconds"`
}

// Report is a whole run.
type Report struct {
	Suite    string            `json:"suite"`
	Tool     string            `json:"tool"`
	Host     string            `json:"host"`
	Started  time.Time         `json:"started"`
	Finished time.Time         `json:"finished"`
	Context  map[string]string `json:"context"`
	Results  []Result          `json:"results"`
	out      io.Writer
	progress bool // print a "running" line first (terminals only)
	// Share: the results are numbers to review together (phase 2 accuracy and
	// speed), so evidence is printed for passing checks too and the copy block
	// is printed even when everything passed.
	Share bool `json:"-"`
}

// NewReport starts a sar-check report; results are printed to out as they complete.
func NewReport(suite string, out io.Writer) *Report { return NewToolReport("sar-check", suite, out) }

// NewToolReport starts a report for another tool (e.g. sar-doctor).
func NewToolReport(tool, suite string, out io.Writer) *Report {
	host, _ := os.Hostname()
	r := &Report{Suite: suite, Tool: version.String(tool), Host: host, Started: time.Now().UTC(),
		Context: map[string]string{}, out: out}
	if f, ok := out.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			r.progress = true
		}
	}
	return r
}

// Run times fn and records its result. fn returns the result without timing.
func (r *Report) Run(id, name string, fn func() Result) Result {
	if r.progress {
		fmt.Fprintf(r.out, "  [....] %s %s\r", id, name)
	}
	start := time.Now()
	res := fn()
	res.ID, res.Name = id, name
	res.Seconds = time.Since(start).Round(100 * time.Millisecond).Seconds()
	if res.Status == Fail || res.Status == Warn {
		if res.Hint == "" && res.Code != "" {
			_, res.Hint, _ = diag.Lookup(res.Code)
		}
	}
	r.Results = append(r.Results, res)
	r.printLine(res)
	return res
}

// Skip records a check that could not run.
func (r *Report) Skip(id, name, why string) {
	res := Result{ID: id, Name: name, Status: Skip, Detail: why}
	r.Results = append(r.Results, res)
	r.printLine(res)
}

func (r *Report) printLine(res Result) {
	dots := 34 - len(res.Name)
	if dots < 2 {
		dots = 2
	}
	if r.progress {
		fmt.Fprint(r.out, "\033[K")
	}
	fmt.Fprintf(r.out, "  [%s] %s %s %s %5.1fs  %s\n", res.Status, res.ID, res.Name, strings.Repeat(".", dots), res.Seconds, res.Detail)
	problem := res.Status == Fail || res.Status == Warn
	if problem && res.Code != "" {
		fmt.Fprintf(r.out, "         code: %s\n", res.Code)
	}
	if problem || r.Share {
		for _, e := range res.Evidence {
			if strings.TrimSpace(e) != "" {
				fmt.Fprintf(r.out, "         - %s\n", e)
			}
		}
	}
	if problem && res.Hint != "" {
		fmt.Fprintf(r.out, "         hint: %s\n", res.Hint)
	}
}

// Passed is true when nothing failed (warnings allowed).
func (r *Report) Passed() bool {
	for _, x := range r.Results {
		if x.Status == Fail {
			return false
		}
	}
	return len(r.Results) > 0
}

// Counts returns pass/warn/fail/skip totals.
func (r *Report) Counts() (p, w, f, s int) {
	p, w, f, s, _ = r.CountsAll()
	return
}

// CountsAll also returns the number of TODO results.
func (r *Report) CountsAll() (p, w, f, s, t int) {
	for _, x := range r.Results {
		switch x.Status {
		case Pass:
			p++
		case Warn:
			w++
		case Fail:
			f++
		case Skip:
			s++
		case Todo:
			t++
		}
	}
	return
}

// Finish stamps the end time, saves JSON in dir and prints the summary and,
// on failure or warning, the copy-and-paste block.
func (r *Report) Finish(dir string) (string, error) {
	r.Finished = time.Now().UTC()
	path, err := r.save(dir)
	p, w, f, s, t := r.CountsAll()
	verdict := "PASS"
	if !r.Passed() {
		verdict = "FAIL"
	}
	todo := ""
	if t > 0 {
		todo = fmt.Sprintf(", %d to be installed by later steps", t)
	}
	fmt.Fprintf(r.out, "\nRESULT: %s  (%d passed, %d warnings, %d failed, %d skipped%s)\n", verdict, p, w, f, s, todo)
	if err == nil {
		fmt.Fprintf(r.out, "Report saved: %s\n", path)
	} else {
		fmt.Fprintf(r.out, "Could not save report: %v\n", err)
	}
	switch {
	case f > 0 || w > 0:
		fmt.Fprint(r.out, r.CopyBlock())
		fmt.Fprintln(r.out, "Paste everything between the two lines above into the chat to get help.")
	case r.Share:
		fmt.Fprint(r.out, r.CopyBlock())
		fmt.Fprintln(r.out, "Paste everything between the two lines above into the chat to share the results.")
	}
	return path, err
}

func (r *Report) save(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.json", r.Suite, sanitize(r.Host), r.Started.Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// CopyBlock renders failures and warnings with the run context.
func (r *Report) CopyBlock() string {
	d := diag.Report{Version: r.Tool}
	d.Add("suite:   %s", r.Suite)
	keys := make([]string, 0, len(r.Context))
	for k := range r.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.Add("%-8s %s", k+":", r.Context[k])
	}
	for _, x := range r.Results {
		line := fmt.Sprintf("[%s] %s %s: %s", x.Status, x.ID, x.Name, x.Detail)
		if x.Code != "" {
			line += " (" + string(x.Code) + ")"
		}
		d.Add("%s", line)
		if x.Status == Fail || x.Status == Warn || r.Share {
			for _, e := range x.Evidence {
				if strings.TrimSpace(e) != "" {
					d.Add("    - %s", e)
				}
			}
		}
	}
	return d.Render()
}
