//go:build darwin

// Package host selects the platform.Driver matching the build target. It is
// a separate leaf package (rather than a platform.New function) because the
// OS driver packages import platform for its types — the selector must sit
// above both to avoid an import cycle.
package host

import (
	"print-agent/internal/platform"
	"print-agent/internal/platform/darwin"
)

// New returns the driver for the OS this binary was built for.
func New() platform.Driver { return darwin.New() }
