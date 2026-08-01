//go:build linux

// Package linux is the platform driver scaffold for Linux (spec Phase 5).
//
// Planned implementation:
//   - Discovery/pairing state via BlueZ over D-Bus (org.bluez.Device1:
//     Paired, Connected, Address, Name, Class) — this backs ListCandidates
//     and VerifyConnected.
//   - Transport via a direct RFCOMM socket (AF_BLUETOOTH/BTPROTO_RFCOMM to
//     MAC + channel, golang.org/x/sys/unix) instead of managing /dev/rfcomm*
//     device files with the deprecated rfcomm(1) tool — override NewTransport
//     with a socket-backed transport.Transport.
//   - OpenSystemBluetoothSettings: best-effort exec of the desktop's
//     Bluetooth panel (gnome-control-center bluetooth, blueman-manager, …).
//   - Store printers by MAC, never by device path.
//
// Until then the embedded UnimplementedDriver trusts a manually configured
// endpoint (e.g. a pre-bound /dev/rfcomm0) over the shared serial transport,
// with no discovery and no link verification.
package linux

import "print-agent/internal/platform"

type Driver struct {
	platform.UnimplementedDriver
}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "linux" }
