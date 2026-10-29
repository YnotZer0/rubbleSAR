// Package cli holds small helpers shared by the RubbleSAR command-line tools.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"rubblesar/internal/diag"
	"rubblesar/internal/version"
)

// Exit codes used by every tool.
const (
	ExitOK    = 0
	ExitFail  = 1 // a check failed
	ExitSetup = 2 // the tool could not run (bad config, missing library, ...)
)

// Fatal prints an error with its code, hint and a copy-and-paste block, then exits.
func Fatal(tool string, err error, context ...string) {
	fmt.Fprintf(os.Stderr, "\nERROR: %v\n", err)
	if h := diag.HintOf(err); h != "" {
		fmt.Fprintf(os.Stderr, "hint:  %s\n", h)
	}
	r := diag.Report{Version: version.String(tool)}
	r.Add("command: %s", strings.Join(os.Args, " "))
	for _, c := range context {
		r.Add("%s", c)
	}
	r.AddError("error:   ", err)
	fmt.Fprint(os.Stderr, r.Render())
	fmt.Fprintln(os.Stderr, "Paste everything between the two lines above into the chat to get help.")
	os.Exit(ExitSetup)
}

// SignalContext is cancelled on SIGINT or SIGTERM.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// LoadEnvFile sets variables from a KEY=VALUE file (as used by systemd
// EnvironmentFile) unless they are already set. Missing files are ignored.
func LoadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}

// DefaultConfig returns the first existing path, or the first candidate.
func DefaultConfig(candidates ...string) string {
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}
