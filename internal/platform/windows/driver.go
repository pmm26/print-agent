// Package windows implements the platform driver for Windows. Paired
// Bluetooth SPP printers are exposed by Windows as virtual COM ports; the
// shared serial transport owns the actual byte stream.
package windows

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"print-agent/internal/config"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

type portRecord struct {
	Name         string
	FriendlyName string
	Description  string
	Enumerator   string
	InstanceID   string
	Address      string
}

type bluetoothRecord struct {
	Name          string
	Address       string
	Class         uint32
	Remembered    bool
	Authenticated bool
	Connected     bool
}

type Driver struct {
	ports     func(context.Context) ([]portRecord, error)
	bluetooth func(context.Context) ([]bluetoothRecord, error)
	launch    func(context.Context) error
}

func New() *Driver {
	return &Driver{
		ports:     nativePortRecords,
		bluetooth: nativeBluetoothRecords,
		launch:    openBluetoothSettings,
	}
}

var _ platform.LinkVerifier = (*Driver)(nil)

func (d *Driver) Name() string { return "windows" }

func (d *Driver) NewTransport(cfg config.PrinterConfig) transport.Transport {
	return transport.NewSerial(cfg)
}

func (d *Driver) ListCandidates(ctx context.Context) ([]platform.Candidate, error) {
	ports, err := d.portRecords(ctx)
	if err != nil {
		return nil, err
	}
	bt, _ := d.bluetoothRecords(ctx)
	byAddress := bluetoothByAddress(bt)

	out := make([]platform.Candidate, 0, len(ports))
	for _, p := range ports {
		if !isBluetoothPort(p) {
			continue
		}
		address := p.Address
		dev := bluetoothRecord{}
		if address != "" {
			dev = byAddress[address]
		}
		name := dev.Name
		if name == "" {
			name = displayNameForPort(p)
		}
		class := dev.Class
		out = append(out, platform.Candidate{
			Endpoint:      normalizeCOMPort(p.Name),
			DeviceName:    name,
			DeviceAddress: address,
			Connected:     dev.Connected,
			IsPrinter:     isPrinterClass(class) || looksLikePrinter(name) || looksLikePrinter(p.FriendlyName) || looksLikePrinter(p.Description),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsPrinter != out[j].IsPrinter {
			return out[i].IsPrinter
		}
		left, right := strings.ToLower(out[i].DeviceName), strings.ToLower(out[j].DeviceName)
		if left != right {
			return left < right
		}
		return strings.ToUpper(out[i].Endpoint) < strings.ToUpper(out[j].Endpoint)
	})
	return out, nil
}

func (d *Driver) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	configured := normalizeCOMPort(cfg.Endpoint)
	address, err := normalizeOptionalAddress(cfg.DeviceAddress)
	if err != nil {
		return "", err
	}
	ports, err := d.portRecords(ctx)
	if err != nil {
		if configured != "" {
			return configured, nil
		}
		return "", err
	}
	for _, p := range ports {
		if address != "" && p.Address == address {
			return normalizeCOMPort(p.Name), nil
		}
	}
	if configured != "" {
		for _, p := range ports {
			if sameCOMPort(p.Name, configured) {
				return normalizeCOMPort(p.Name), nil
			}
		}
		return configured, nil
	}
	if address != "" {
		return "", fmt.Errorf("paired Bluetooth device %s has no Windows COM port; open Bluetooth settings and confirm the printer exposes Serial Port Profile", address)
	}
	return "", errors.New("no COM endpoint configured")
}

func (d *Driver) Disconnect(ctx context.Context, cfg config.PrinterConfig) error {
	return nil
}

func (d *Driver) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	state, err := d.LinkState(ctx, cfg)
	if err != nil {
		return err
	}
	if state == platform.LinkDisconnected {
		return platform.ErrNotConnected
	}
	return nil
}

func (d *Driver) LinkState(ctx context.Context, cfg config.PrinterConfig) (platform.LinkState, error) {
	address, err := normalizeOptionalAddress(cfg.DeviceAddress)
	if err != nil {
		return platform.LinkUnknown, err
	}
	ports, portErr := d.portRecords(ctx)
	if portErr != nil {
		return platform.LinkUnknown, portErr
	}
	if address == "" {
		address = addressForEndpoint(ports, cfg.Endpoint)
	}
	if address == "" {
		// A virtual COM port can remain installed while its Bluetooth device is
		// powered off. Port presence alone is never connection evidence.
		return platform.LinkUnknown, nil
	}

	bt, err := d.bluetoothRecords(ctx)
	if err != nil {
		return platform.LinkUnknown, err
	}
	for _, dev := range bt {
		if dev.Address != address {
			continue
		}
		if dev.Connected {
			return platform.LinkConnected, nil
		}
		return platform.LinkDisconnected, nil
	}
	return platform.LinkUnknown, nil
}

func (d *Driver) OpenSystemBluetoothSettings(ctx context.Context) error {
	if d.launch != nil {
		return d.launch(ctx)
	}
	return openBluetoothSettings(ctx)
}

func (d *Driver) portRecords(ctx context.Context) ([]portRecord, error) {
	if d.ports == nil {
		return nil, errors.New("Windows COM port enumeration is unavailable")
	}
	return d.ports(ctx)
}

func (d *Driver) bluetoothRecords(ctx context.Context) ([]bluetoothRecord, error) {
	if d.bluetooth == nil {
		return nil, errors.New("Windows Bluetooth device enumeration is unavailable")
	}
	return d.bluetooth(ctx)
}

func openBluetoothSettings(ctx context.Context) error {
	return exec.CommandContext(ctx, "cmd", "/c", "start", "", "ms-settings:bluetooth").Run()
}

func bluetoothByAddress(records []bluetoothRecord) map[string]bluetoothRecord {
	out := make(map[string]bluetoothRecord, len(records))
	for _, r := range records {
		if r.Address != "" {
			out[r.Address] = r
		}
	}
	return out
}

func addressForEndpoint(ports []portRecord, endpoint string) string {
	for _, p := range ports {
		if sameCOMPort(p.Name, endpoint) {
			return p.Address
		}
	}
	return ""
}
