// Package config holds printer and agent configuration models shared
// across the transport, bluetooth, rendering, and API layers.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// TransportKind selects how bytes reach the printer.
type TransportKind string

const (
	// TransportBluetoothSerial is an OS-provided serial endpoint backed by
	// a paired Bluetooth SPP device (COM port, /dev/cu.*, /dev/rfcomm*).
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
	ID                 string        `json:"id"`
	DisplayName        string        `json:"displayName"`
	Enabled            bool          `json:"enabled"`
	Transport          TransportKind `json:"transport"`
	DeviceAddress      string        `json:"deviceAddress,omitempty"`
	Endpoint           string        `json:"endpoint"`
	BaudRate           int           `json:"baudRate"`
	DataBits           int           `json:"dataBits"`
	StopBits           int           `json:"stopBits"`
	Parity             string        `json:"parity"`
	PaperWidthMm       int           `json:"paperWidthMm"`
	CharactersPerLine  int           `json:"charactersPerLine"`
	Encoding           string        `json:"encoding"`
	AutoReconnect      bool          `json:"autoReconnect"`
	StatusProbeEnabled bool          `json:"statusProbeEnabled"`
	CreatedAt          time.Time     `json:"createdAt"`
	UpdatedAt          time.Time     `json:"updatedAt"`
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
	if p.PaperWidthMm == 0 {
		p.PaperWidthMm = 58
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
