//go:build !darwin && !linux && !windows

package host

import "print-agent/internal/platform"

type otherDriver struct{ platform.UnimplementedDriver }

// New returns the driver for the OS this binary was built for.
func New() platform.Driver { return otherDriver{} }
