// Package version reports build information for every RubbleSAR binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at build time with -ldflags "-X rubblesar/internal/version.Version=...".
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	// ZenohC is the zenoh-c version the binary was linked against ("none" for pure-Go tools).
	ZenohC = "none"
)

// Phase is the delivery phase this code base implements.
const Phase = 8

// Short is "tool VERSION (commit C)", for telemetry and status lines.
func Short(tool string) string {
	return fmt.Sprintf("%s %s (commit %s)", tool, Version, commit())
}

func commit() string {
	c := Commit
	if c == "unknown" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 8 {
					c = s.Value[:8]
				}
			}
		}
	}
	return c
}

// String returns a one-line build description.
func String(tool string) string {
	return fmt.Sprintf("%s %s (phase %d, commit %s, built %s, %s %s/%s, zenoh-c %s)",
		tool, Version, Phase, commit(), BuildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH, ZenohC)
}
