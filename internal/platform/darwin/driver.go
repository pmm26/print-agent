//go:build darwin

// Package darwin implements the platform driver for macOS. Paired Bluetooth
// SPP printers surface as /dev/cu.<DeviceName> nodes (spaces stripped or
// replaced, depending on OS version); the real link state comes from
// system_profiler, because opening and writing the serial node succeed even
// when the printer is off.
package darwin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

// Driver is the macOS platform driver.
//
// The paired-device snapshot from system_profiler is cached briefly: the
// command takes ~1s and VerifyConnected is called every few seconds per
// printer.
type Driver struct {
	mu       sync.Mutex
	cached   []pairedDevice
	cachedAt time.Time
}

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "darwin" }

// NewTransport uses the shared serial transport over the /dev/cu.* node.
func (d *Driver) NewTransport(cfg config.PrinterConfig) transport.Transport {
	return transport.NewSerial(cfg)
}

// deviceCacheTTL bounds how stale the paired-device snapshot may be.
const deviceCacheTTL = 3 * time.Second

func (d *Driver) devices(ctx context.Context) []pairedDevice {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.cachedAt) > deviceCacheTTL {
		d.cached = pairedDevices(ctx)
		d.cachedAt = time.Now()
	}
	return d.cached
}

// VerifyConnected reports platform.ErrNotConnected only when the paired
// device is positively known to be disconnected. Unknown devices (no MAC
// stored and no name correlation) verify as connected — better optimistic
// than flapping.
func (d *Driver) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	nodeName := strings.TrimPrefix(filepath.Base(cfg.Endpoint), "cu.")
	for _, dev := range d.devices(ctx) {
		match := (cfg.DeviceAddress != "" && strings.EqualFold(dev.address, cfg.DeviceAddress)) ||
			matchesDeviceName(dev.name, nodeName)
		if match {
			if dev.connected {
				return nil
			}
			return platform.ErrNotConnected
		}
	}
	return nil // device not in the paired list: state unknowable
}

func (d *Driver) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	// Fast path: the configured endpoint exists. Opening it triggers the
	// macOS Bluetooth stack to (re)connect the paired device.
	if cfg.Endpoint != "" {
		if _, err := os.Stat(cfg.Endpoint); err == nil {
			return cfg.Endpoint, nil
		}
	}
	// The device node vanished (unpaired, renamed, BT restart). Try to
	// re-resolve by MAC address.
	cands, err := d.ListCandidates(ctx)
	if err != nil {
		return "", fmt.Errorf("endpoint %s missing and candidate scan failed: %w", cfg.Endpoint, err)
	}
	for _, cand := range cands {
		if cfg.DeviceAddress != "" && strings.EqualFold(cand.DeviceAddress, cfg.DeviceAddress) {
			return cand.Endpoint, nil
		}
	}
	return "", fmt.Errorf("endpoint %s not present and no candidate matches device %q — check the printer is paired in Bluetooth settings",
		cfg.Endpoint, cfg.DeviceAddress)
}

func (d *Driver) Disconnect(ctx context.Context, cfg config.PrinterConfig) error {
	return nil // closing the serial port releases the link on macOS
}

func (d *Driver) ListCandidates(ctx context.Context) ([]platform.Candidate, error) {
	devs, err := filepath.Glob("/dev/cu.*")
	if err != nil {
		return nil, err
	}
	paired := d.devices(ctx)
	var out []platform.Candidate
	for _, dev := range devs {
		name := strings.TrimPrefix(filepath.Base(dev), "cu.")
		// Skip endpoints that are definitely not printers.
		if name == "debug-console" || strings.HasPrefix(name, "Bluetooth-Incoming") {
			continue
		}
		cand := platform.Candidate{Endpoint: dev}
		for _, p := range paired {
			if matchesDeviceName(p.name, name) {
				cand.DeviceName = p.name
				cand.DeviceAddress = p.address
				cand.Connected = p.connected
				cand.IsPrinter = p.minorType == "Printer"
				break
			}
		}
		out = append(out, cand)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out, nil
}

func (d *Driver) OpenSystemBluetoothSettings(ctx context.Context) error {
	return exec.CommandContext(ctx, "open", "x-apple.systempreferences:com.apple.BluetoothSettings").Run()
}

type pairedDevice struct {
	name      string
	address   string
	minorType string
	connected bool
}

// pairedDevices reads paired Bluetooth devices via system_profiler. Failures
// degrade gracefully: candidates simply lose their name/MAC annotations.
func pairedDevices(ctx context.Context) []pairedDevice {
	out, err := exec.CommandContext(ctx, "system_profiler", "SPBluetoothDataType", "-json").Output()
	if err != nil {
		return nil
	}
	// system_profiler JSON shape:
	// {"SPBluetoothDataType":[{"device_connected":[{"Name":{...fields}}],
	//                          "device_not_connected":[{"Name":{...}}]}]}
	var doc struct {
		SPBluetoothDataType []map[string]json.RawMessage `json:"SPBluetoothDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil
	}
	var devices []pairedDevice
	for _, section := range doc.SPBluetoothDataType {
		for key, connected := range map[string]bool{
			"device_connected":     true,
			"device_not_connected": false,
		} {
			raw, ok := section[key]
			if !ok {
				continue
			}
			var list []map[string]struct {
				Address   string `json:"device_address"`
				MinorType string `json:"device_minorType"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				continue
			}
			for _, entry := range list {
				for name, props := range entry {
					devices = append(devices, pairedDevice{
						name:      name,
						address:   props.Address,
						minorType: props.MinorType,
						connected: connected,
					})
				}
			}
		}
	}
	return devices
}

// matchesDeviceName reports whether a paired Bluetooth device name maps to
// the given /dev/cu.<node> suffix. macOS derives the node name from the
// device name by removing or replacing spaces, depending on OS version.
func matchesDeviceName(deviceName, nodeName string) bool {
	return strings.ReplaceAll(deviceName, " ", "") == nodeName ||
		strings.ReplaceAll(deviceName, " ", "-") == nodeName
}
