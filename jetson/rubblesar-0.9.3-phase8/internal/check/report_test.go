package check

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReportLifecycle(t *testing.T) {
	var out bytes.Buffer
	r := NewReport("unit", &out)
	r.Context["incident"] = "inc"
	r.Run("CHK-A", "always passes", func() Result { return Result{Status: Pass, Detail: "fine"} })
	r.Run("CHK-B", "warns", func() Result { return Result{Status: Warn, Code: "E-CHK-006", Detail: "offset 300 ms"} })
	if !r.Passed() {
		t.Fatal("warnings must not fail the run")
	}
	r.Run("CHK-C", "fails", func() Result {
		return Result{Status: Fail, Code: "E-CHK-002", Detail: "missing inv-2", Evidence: []string{"seen: scout-1"}}
	})
	r.Skip("CHK-D", "skipped", "depends on CHK-C")
	if r.Passed() {
		t.Fatal("a failure must fail the run")
	}
	dir := t.TempDir()
	path, err := r.Finish(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"[PASS] CHK-A", "[WARN] CHK-B", "[FAIL] CHK-C", "[SKIP] CHK-D",
		"code: E-CHK-002", "seen: scout-1", "hint:", "RESULT: FAIL", "COPY FROM HERE", "COPY TO HERE",
		"incident: inc"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n%s", want, s)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || len(back.Results) != 4 || back.Results[2].Code != "E-CHK-002" {
		t.Fatalf("saved report wrong: %v %+v", err, back)
	}
}

func TestPassingRunHasNoCopyBlock(t *testing.T) {
	var out bytes.Buffer
	r := NewReport("unit", &out)
	r.Run("CHK-A", "ok", func() Result { return Result{Status: Pass} })
	if _, err := r.Finish(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "COPY FROM HERE") || !strings.Contains(out.String(), "RESULT: PASS") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

// A shared report (phase 2) prints evidence and the copy block even when everything passed.
func TestSharedReportPrintsResults(t *testing.T) {
	var out bytes.Buffer
	r := NewReport("share", &out)
	r.Share = true
	r.Run("S-01", "numbers", func() Result {
		return Result{Status: Pass, Detail: "AP50 0.8", Evidence: []string{"hand AP50 0.7"}}
	})
	if _, err := r.Finish(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	txt := out.String()
	for _, want := range []string{"- hand AP50 0.7", "COPY FROM HERE", "    - hand AP50 0.7", "to share the results"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
}
