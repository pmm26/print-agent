//go:build windows

package host

import (
	"print-agent/internal/platform"
	"print-agent/internal/platform/windows"
)

// New returns the driver for the OS this binary was built for.
func New() platform.Driver { return windows.New() }
