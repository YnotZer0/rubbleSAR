package check

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"rubblesar/internal/cot"
)

func TestTakServerSuite(t *testing.T) {
	srv, err := cot.ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := TakServer(context.Background(), srv.Addr(), &out)
	if !r.Passed() || strings.Contains(out.String(), "WARN") {
		t.Fatalf("against the test router:\n%s", out.String())
	}
	srv.Close()
	out.Reset()
	r = TakServer(context.Background(), srv.Addr(), &out)
	if r.Passed() || !strings.Contains(out.String(), "[FAIL] T-01") || !strings.Contains(out.String(), "34-taky.sh") {
		t.Fatalf("with no server:\n%s", out.String())
	}
}
