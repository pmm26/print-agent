// Package config holds printer and agent configuration models shared
// across the transport, bluetooth, rendering, and API layers.
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// TransportKind selects how bytes reach the printer.
type TransportKind string

const (
	// TransportBluetoothSerial sends bytes over the platform's Bluetooth
	// printer channel exposed by the active platform driver.
	TransportBluetoothSerial TransportKind = "bluetooth-serial"
	// TransportMock is an in-process fake printer used for development
	// and integration tests. It records bytes instead of printing.
	TransportMock TransportKind = "mock"
)

// Parity values accepted in printer configuration.
const (
	ParityNone = "none"
	ParityOdd  = "odd"
	ParityEven = "even"
)

// PrinterConfig is the persisted configuration for one logical printer.
type PrinterConfig struct {
	ID                string        `json:"id"`
	DisplayName       string        `json:"displayName"`
	Enabled           bool          `json:"enabled"`
	Transport         TransportKind `json:"transport"`
	DeviceAddress     string        `json:"deviceAddress,omitempty"`
	Endpoint          string        `json:"endpoint"`
	BaudRate          int           `json:"baudRate"`
	DataBits          int           `json:"dataBits"`
	StopBits          int           `json:"stopBits"`
	Parity            string        `json:"parity"`
	CharactersPerLine int           `json:"charactersPerLine"`
	Encoding          string        `json:"encoding"`
	AutoReconnect     bool          `json:"autoReconnect"`
	RetiredAt         *time.Time    `json:"retiredAt,omitempty"`
	CreatedAt         time.Time     `json:"createdAt"`
	UpdatedAt         time.Time     `json:"updatedAt"`
}

var printerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ApplyDefaults fills zero-valued fields with sensible defaults for a
// generic 58 mm Bluetooth ESC/POS printer.
func (p *PrinterConfig) ApplyDefaults() {
	if p.Transport == "" {
		p.Transport = TransportBluetoothSerial
	}
	if p.BaudRate == 0 {
		p.BaudRate = 9600
	}
	if p.DataBits == 0 {
		p.DataBits = 8
	}
	if p.StopBits == 0 {
		p.StopBits = 1
	}
	if p.Parity == "" {
		p.Parity = ParityNone
	}
	if p.CharactersPerLine == 0 {
		p.CharactersPerLine = 32
	}
	if p.Encoding == "" {
		p.Encoding = "CP858"
	}
	if p.DisplayName == "" {
		p.DisplayName = p.ID
	}
}

// Validate checks the configuration for values the agent cannot operate with.
func (p *PrinterConfig) Validate() error {
	if !printerIDRe.MatchString(p.ID) {
		return fmt.Errorf("printer id %q must match %s", p.ID, printerIDRe)
	}
	switch p.Transport {
	case TransportBluetoothSerial, TransportMock:
	default:
		return fmt.Errorf("unsupported transport %q", p.Transport)
	}
	if p.Transport == TransportBluetoothSerial && p.Endpoint == "" {
		return errors.New("endpoint is required for bluetooth-serial printers")
	}
	if len(p.DisplayName) > 100 {
		return errors.New("displayName must be at most 100 bytes")
	}
	if len(p.Endpoint) > 512 || len(p.DeviceAddress) > 64 {
		return errors.New("printer endpoint or device address is too long")
	}
	if p.DeviceAddress != "" {
		address := strings.ReplaceAll(strings.TrimSpace(p.DeviceAddress), "-", ":")
		hardwareAddress, err := net.ParseMAC(address)
		if err != nil || len(hardwareAddress) != 6 {
			return errors.New("deviceAddress must be a six-byte Bluetooth address")
		}
	}
	lowerEndpoint := strings.ToLower(p.Endpoint)
	if strings.HasPrefix(lowerEndpoint, "rfcomm://") || strings.HasPrefix(lowerEndpoint, "ble://") {
		endpointAddress := strings.TrimPrefix(strings.TrimPrefix(lowerEndpoint, "rfcomm://"), "ble://")
		normalizedDevice := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p.DeviceAddress), "-", ":"))
		if normalizedDevice != "" && endpointAddress != normalizedDevice {
			return errors.New("deviceAddress must match the Bluetooth address in endpoint")
		}
	}
	if p.BaudRate < 300 || p.BaudRate > 1000000 {
		return fmt.Errorf("baudRate %d out of range", p.BaudRate)
	}
	if p.DataBits < 5 || p.DataBits > 8 {
		return fmt.Errorf("dataBits %d out of range", p.DataBits)
	}
	if p.StopBits != 1 && p.StopBits != 2 {
		return fmt.Errorf("stopBits must be 1 or 2")
	}
	switch p.Parity {
	case ParityNone, ParityOdd, ParityEven:
	default:
		return fmt.Errorf("unsupported parity %q", p.Parity)
	}
	if p.CharactersPerLine < 8 || p.CharactersPerLine > 96 {
		return fmt.Errorf("charactersPerLine %d out of range", p.CharactersPerLine)
	}
	return nil
}

// Settings are agent-level key/value settings persisted in SQLite.
const (
	SettingAllowedOrigin = "allowed_origin"
)
