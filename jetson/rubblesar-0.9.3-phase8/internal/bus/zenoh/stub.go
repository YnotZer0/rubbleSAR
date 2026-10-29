//go:build nozenoh

// Package zenoh, built with -tags nozenoh: a stub that compiles without
// libzenohc. Every Open fails with E-BUS-002. Used for unit tests on machines
// where zenoh-c is not installed yet.
package zenoh

import (
	"context"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
)

// Available reports whether this binary can open zenoh sessions.
const Available = false

// LibraryVersion returns "none" in stub builds.
func LibraryVersion() string { return "none" }

// Session is never constructed in stub builds.
type Session struct{ bus.Memory }

// Open always fails in stub builds.
func Open(context.Context, config.Zenoh) (*Session, error) {
	return nil, diag.New("E-BUS-002", "this binary was built with -tags nozenoh")
}

// Opener adapts Open for bus.OpenRetry.
func Opener(z config.Zenoh) bus.OpenFunc {
	return func(ctx context.Context) (bus.Bus, error) {
		_, err := Open(ctx, z)
		return nil, err
	}
}
