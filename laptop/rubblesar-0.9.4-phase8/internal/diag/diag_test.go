package diag

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestErrorFormatting(t *testing.T) {
	base := errors.New("dial tcp: refused")
	e := Wrap("E-BUS-010", base, "router %s", "tcp/1.2.3.4:7447")
	if got := e.Error(); got != "E-BUS-010: router tcp/1.2.3.4:7447: dial tcp: refused" {
		t.Fatalf("Error() = %q", got)
	}
	wrapped := fmt.Errorf("context: %w", e)
	if CodeOf(wrapped) != "E-BUS-010" || !strings.Contains(HintOf(wrapped), "7447") || !errors.Is(wrapped, base) {
		t.Fatalf("code/hint/unwrap lost through wrapping")
	}
	if CodeOf(base) != "" || HintOf(base) != "" {
		t.Fatal("plain errors have no code")
	}
	if New("E-CFG-001", "").Error() != "E-CFG-001: config file not found" {
		t.Fatal("empty detail should fall back to the title")
	}
}

func TestEveryCodeHasTitleAndHint(t *testing.T) {
	for _, c := range Codes() {
		title, hint, ok := Lookup(c)
		if !ok || title == "" || hint == "" {
			t.Errorf("%s: incomplete catalogue entry", c)
		}
	}
}

// Every code must be explained in docs/TROUBLESHOOTING.md.
func TestEveryCodeDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/TROUBLESHOOTING.md")
	if err != nil {
		t.Skip("docs not present")
	}
	for _, c := range Codes() {
		if !strings.Contains(string(b), "| "+string(c)+" |") {
			t.Errorf("%s missing from docs/TROUBLESHOOTING.md", c)
		}
	}
}

func TestReportRender(t *testing.T) {
	r := Report{Version: "tool 1.0"}
	r.Add("x: %d", 1)
	r.AddError("error: ", New("E-CHK-002", "missing inv-2"))
	s := r.Render()
	for _, want := range []string{"COPY FROM HERE", "tool:    tool 1.0", "x: 1", "E-CHK-002: missing inv-2", "hint:", "COPY TO HERE"} {
		if !strings.Contains(s, want) {
			t.Errorf("render missing %q:\n%s", want, s)
		}
	}
}
