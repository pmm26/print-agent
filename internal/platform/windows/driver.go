//go:build windows

// Package windows is the platform driver scaffold for Windows (spec Phase 4).
//
// Planned implementation:
//   - ListCandidates: enumerate outgoing Bluetooth virtual COM ports and
//     correlate them with paired devices (registry under
//     HKLM\HARDWARE\DEVICEMAP\SERIALCOMM plus SetupAPI/PnP device IDs, which
//     embed the Bluetooth MAC).
//   - EnsureConnected: return the configured COM port; opening it makes the
//     Windows Bluetooth stack reconnect an already-paired SPP printer.
//   - VerifyConnected: query the paired device's connection state via the
//     Bluetooth APIs (BluetoothGetDeviceInfo) if the buffered-write problem
//     exists on Windows too; validate against real hardware first.
//   - OpenSystemBluetoothSettings: exec `start ms-settings:bluetooth`.
//   - The shared serial transport already speaks COM ports, so NewTransport
//     likely stays inherited.
//
// Until then the embedded UnimplementedDriver trusts a manually configured
// COM endpoint over the shared serial transport, with no discovery and no
// link verification.
package windows

import "print-agent/internal/platform"

type Driver struct {
	platform.UnimplementedDriver
}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "windows" }
