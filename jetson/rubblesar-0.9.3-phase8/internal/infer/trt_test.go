//go:build trt

package infer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rubblesar/internal/diag"
)

// Without a real engine the shim must fail cleanly with a coded error.
func TestTRTOpenErrors(t *testing.T) {
	if _, err := OpenTRT(filepath.Join(t.TempDir(), "missing.engine")); diag.CodeOf(err) != "E-INF-001" {
		t.Fatalf("missing engine: %v", err)
	}
	p := filepath.Join(t.TempDir(), "junk.engine")
	os.WriteFile(p, []byte("not an engine"), 0o644)
	_, err := OpenTRT(p)
	if diag.CodeOf(err) != "E-INF-005" || !strings.Contains(err.Error(), "junk.engine") {
		t.Fatalf("junk engine: %v", err)
	}
	t.Log(err)
}
