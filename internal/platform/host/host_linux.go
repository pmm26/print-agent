//go:build linux

package host

import (
	"print-agent/internal/platform"
	"print-agent/internal/platform/linux"
)

// New returns the driver for the OS this binary was built for.
func New() platform.Driver { return linux.New() }
