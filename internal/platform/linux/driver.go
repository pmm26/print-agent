//go:build linux

// Package linux implements Bluetooth Serial Port Profile printer support on
// Linux. BlueZ owns pairing and SDP; the agent registers an SPP client profile
// and receives the connected RFCOMM socket from Profile1.NewConnection.
package linux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"print-agent/internal/config"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

const (
	bluezService        = "org.bluez"
	adapterInterface    = "org.bluez.Adapter1"
	deviceInterface     = "org.bluez.Device1"
	agentInterface      = "org.bluez.Agent1"
	agentManager        = "org.bluez.AgentManager1"
	objectManager       = "org.freedesktop.DBus.ObjectManager"
	pairingAgentPath    = dbus.ObjectPath("/com/print_agent/pairing_agent")
	serialPortUUID      = "00001101-0000-1000-8000-00805f9b34fb"
	blePrintServiceUUID = "000018f0-0000-1000-8000-00805f9b34fb"
	blePrintCharUUID    = "00002af1-0000-1000-8000-00805f9b34fb"
	rfcommEndpointBase  = "rfcomm://"
	bleEndpointBase     = "ble://"
)

type bluezDevice struct {
	Path      dbus.ObjectPath
	Address   string
	Name      string
	Icon      string
	Class     uint32
	Paired    bool
	Bonded    bool
	Connected bool
	UUIDs     []string
}

type deviceSource interface {
	Devices(context.Context) ([]bluezDevice, error)
}

type bluetoothController interface {
	StartDiscovery(context.Context, config.ConnectionPreference) error
	StopDiscovery(context.Context) error
	PairDevice(context.Context, string, string) error
	DisconnectDevice(context.Context, string) error
	ForgetDevice(context.Context, string) error
}

type profileConnector interface {
	Connect(context.Context, string) (rfcommSocket, error)
	Disconnect(context.Context, string) error
	ConnectionState(string) (connected, known bool)
}

type gattConnector interface {
	ConnectGATT(context.Context, string) (gattConnection, error)
}

// Driver is the Linux platform driver. Its dependencies are interfaces so the
// discovery, state, transport, and desktop-launch behavior can be tested
// without a Bluetooth adapter or graphical session.
type Driver struct {
	devices   deviceSource
	bluetooth bluetoothController
	profiles  profileConnector
	gatt      gattConnector
	lookPath  func(string) (string, error)
	launch    func(context.Context, string, ...string) error

	routesMu sync.Mutex
	routes   map[string]string
}

func New() *Driver {
	backend := &dbusBlueZ{}
	profiles := newProfileBroker(backend)
	return &Driver{
		devices:   backend,
		bluetooth: backend,
		profiles:  profiles,
		gatt:      backend,
		lookPath:  exec.LookPath,
		launch:    launchDetached,
		routes:    make(map[string]string),
	}
}

var _ platform.BluetoothPairer = (*Driver)(nil)

func (d *Driver) Name() string { return "linux" }

// ListBluetoothDevices returns every valid device BlueZ currently knows
// about, including unpaired devices learned during an active discovery scan.
func (d *Driver) ListBluetoothDevices(ctx context.Context) ([]platform.BluetoothDevice, error) {
	devices, err := d.devices.Devices(ctx)
	if err != nil {
		return nil, fmt.Errorf("query BlueZ devices: %w", err)
	}
	out := make([]platform.BluetoothDevice, 0, len(devices))
	for _, dev := range devices {
		address, err := normalizeAddress(dev.Address)
		if err != nil {
			continue
		}
		out = append(out, publicBluetoothDevice(dev, address))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Paired != out[j].Paired {
			return out[i].Paired
		}
		if out[i].IsPrinter != out[j].IsPrinter {
			return out[i].IsPrinter
		}
		left, right := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if left != right {
			return left < right
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}

func (d *Driver) StartBluetoothDiscovery(ctx context.Context, connectionType config.ConnectionPreference) error {
	if d.bluetooth == nil {
		return errors.New("BlueZ discovery is unavailable")
	}
	return d.bluetooth.StartDiscovery(ctx, connectionType)
}

func (d *Driver) StopBluetoothDiscovery(ctx context.Context) error {
	if d.bluetooth == nil {
		return errors.New("BlueZ discovery is unavailable")
	}
	return d.bluetooth.StopDiscovery(ctx)
}

func (d *Driver) PairBluetoothDevice(ctx context.Context, address, pin string, connectionType config.ConnectionPreference) (platform.BluetoothDevice, error) {
	if d.bluetooth == nil {
		return platform.BluetoothDevice{}, platform.ErrBluetoothUnavailable
	}
	normalized, err := normalizeAddress(address)
	if err != nil {
		return platform.BluetoothDevice{}, fmt.Errorf("%w: %v", platform.ErrInvalidBluetoothAddress, err)
	}
	device, err := d.bluetoothDevice(ctx, normalized)
	if err != nil {
		return platform.BluetoothDevice{}, err
	}
	if connectionType == "" {
		connectionType = config.ConnectionAuto
	}
	if !supportsConnection(device, connectionType) {
		return platform.BluetoothDevice{}, fmt.Errorf("%w: device %s does not support %s", platform.ErrBluetoothProtocolUnsupported, normalized, connectionType)
	}
	ready := publicBluetoothDevice(device, normalized)
	if ready.Endpoint != "" && connectionType != config.ConnectionRFCOMM ||
		(connectionType == config.ConnectionRFCOMM && (device.Paired || device.Bonded)) {
		ready.Endpoint = preferredEndpoint(connectionType, device, normalized)
		return ready, nil
	}
	if err := d.bluetooth.PairDevice(ctx, normalized, pin); err != nil {
		return platform.BluetoothDevice{}, err
	}
	device, err = d.bluetoothDevice(ctx, normalized)
	if err != nil {
		return platform.BluetoothDevice{}, err
	}
	result := publicBluetoothDevice(device, normalized)
	result.Endpoint = preferredEndpoint(connectionType, device, normalized)
	return result, nil
}

func (d *Driver) DisconnectBluetoothDevice(ctx context.Context, address string) error {
	normalized, err := normalizeAddress(address)
	if err != nil {
		return fmt.Errorf("%w: %v", platform.ErrInvalidBluetoothAddress, err)
	}
	return d.bluetooth.DisconnectDevice(ctx, normalized)
}

func (d *Driver) ForgetBluetoothDevice(ctx context.Context, address string) error {
	normalized, err := normalizeAddress(address)
	if err != nil {
		return fmt.Errorf("%w: %v", platform.ErrInvalidBluetoothAddress, err)
	}
	return d.bluetooth.ForgetDevice(ctx, normalized)
}

func (d *Driver) bluetoothDevice(ctx context.Context, address string) (bluezDevice, error) {
	devices, err := d.devices.Devices(ctx)
	if err != nil {
		return bluezDevice{}, fmt.Errorf("%w: query BlueZ devices: %v", platform.ErrBluetoothUnavailable, err)
	}
	for _, device := range devices {
		candidate, parseErr := normalizeAddress(device.Address)
		if parseErr == nil && candidate == address {
			return device, nil
		}
	}
	return bluezDevice{}, fmt.Errorf("%w: %s", platform.ErrBluetoothDeviceNotFound, address)
}

func publicBluetoothDevice(dev bluezDevice, address string) platform.BluetoothDevice {
	item := platform.BluetoothDevice{
		Name:      dev.Name,
		Address:   address,
		Paired:    dev.Paired || dev.Bonded,
		Connected: dev.Connected,
		IsPrinter: isPrinterDevice(dev),
	}
	if hasUUID(dev.UUIDs, serialPortUUID) {
		item.SupportedConnectionTypes = append(item.SupportedConnectionTypes, config.ConnectionRFCOMM)
	}
	if hasUUID(dev.UUIDs, blePrintServiceUUID) {
		item.SupportedConnectionTypes = append(item.SupportedConnectionTypes, config.ConnectionBLE)
	}
	if isUsableCandidate(dev) {
		item.Endpoint = endpointForDevice(dev, address)
	}
	return item
}

func supportsConnection(dev bluezDevice, preference config.ConnectionPreference) bool {
	return preference == "" || preference == config.ConnectionAuto ||
		(preference == config.ConnectionRFCOMM && hasUUID(dev.UUIDs, serialPortUUID)) ||
		(preference == config.ConnectionBLE && hasUUID(dev.UUIDs, blePrintServiceUUID))
}

func preferredEndpoint(preference config.ConnectionPreference, dev bluezDevice, address string) string {
	switch preference {
	case config.ConnectionRFCOMM:
		return rfcommEndpoint(address)
	case config.ConnectionBLE:
		return bleEndpoint(address)
	default:
		return endpointForDevice(dev, address)
	}
}

// ListCandidates returns paired SPP devices and printers which BlueZ already
// reports connected. Some inexpensive dual-mode printers never retain a bond,
// even though BlueZ has an active connection and a resolved SPP service.
// Non-printer audio/input devices are deliberately omitted from the wizard.
func (d *Driver) ListCandidates(ctx context.Context) ([]platform.Candidate, error) {
	devices, err := d.devices.Devices(ctx)
	if err != nil {
		return nil, fmt.Errorf("query BlueZ devices: %w", err)
	}
	out := make([]platform.Candidate, 0, len(devices))
	for _, dev := range devices {
		printer := isPrinterDevice(dev)
		if !isUsableCandidate(dev) {
			continue
		}
		address, err := normalizeAddress(dev.Address)
		if err != nil {
			continue
		}
		out = append(out, platform.Candidate{
			Endpoint:      endpointForDevice(dev, address),
			DeviceName:    dev.Name,
			DeviceAddress: address,
			Connected:     dev.Connected,
			IsPrinter:     printer,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsPrinter != out[j].IsPrinter {
			return out[i].IsPrinter
		}
		if out[i].DeviceName != out[j].DeviceName {
			return strings.ToLower(out[i].DeviceName) < strings.ToLower(out[j].DeviceName)
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out, nil
}

// EnsureConnected validates and canonicalizes the configured endpoint. The
// actual RFCOMM connection is opened by the transport so it can own the socket
// and apply write deadlines. Legacy pre-bound device nodes are rejected.
func (d *Driver) EnsureConnected(ctx context.Context, cfg config.PrinterConfig) (string, error) {
	if isLegacySerialEndpoint(cfg.Endpoint) {
		return "", errors.New("legacy /dev/rfcomm endpoints are unsupported; configure the printer with its rfcomm:// or ble:// Bluetooth address")
	}
	address, err := configAddress(cfg)
	if err != nil {
		return "", err
	}
	devices, err := d.devices.Devices(ctx)
	if err != nil {
		return "", fmt.Errorf("query BlueZ devices: %w", err)
	}
	for _, dev := range devices {
		devAddress, parseErr := normalizeAddress(dev.Address)
		if parseErr != nil || devAddress != address {
			continue
		}
		if !isConnectionEligible(dev) {
			return "", fmt.Errorf("Bluetooth device %s is neither paired nor an already-connected SPP printer; connect or pair it in system Bluetooth settings first", address)
		}
		if len(dev.UUIDs) > 0 && !hasUUID(dev.UUIDs, serialPortUUID) && !isPrinterDevice(dev) {
			return "", fmt.Errorf("Bluetooth device %s does not advertise the Serial Port Profile", address)
		}
		endpoint := preferredEndpoint(cfg.ConnectionPreference, dev, address)
		if !supportsConnection(dev, cfg.ConnectionPreference) {
			return "", fmt.Errorf("%w: Bluetooth device %s does not support preferred %s connection", platform.ErrBluetoothProtocolUnsupported, address, cfg.ConnectionPreference)
		}
		if cfg.ConnectionPreference == config.ConnectionRFCOMM && !(dev.Paired || dev.Bonded) {
			return "", fmt.Errorf("Bluetooth device %s must be paired before using RFCOMM", address)
		}
		d.setRoute(address, endpoint)
		return endpoint, nil
	}
	return "", fmt.Errorf("paired Bluetooth device %s is not available from BlueZ", address)
}

func (d *Driver) VerifyConnected(ctx context.Context, cfg config.PrinterConfig) error {
	state, err := d.LinkState(ctx, cfg)
	if err != nil {
		return err
	}
	if state != platform.LinkConnected {
		return platform.ErrNotConnected
	}
	return nil
}

func (d *Driver) LinkState(ctx context.Context, cfg config.PrinterConfig) (platform.LinkState, error) {
	address, err := configAddress(cfg)
	if err != nil {
		return platform.LinkUnknown, err
	}
	if !strings.HasPrefix(d.resolvedEndpoint(address, cfg.Endpoint), bleEndpointBase) {
		if connected, known := d.profiles.ConnectionState(address); known {
			if connected {
				return platform.LinkConnected, nil
			}
			return platform.LinkDisconnected, nil
		}
	}
	devices, err := d.devices.Devices(ctx)
	if err != nil {
		return platform.LinkUnknown, err
	}
	for _, dev := range devices {
		devAddress, parseErr := normalizeAddress(dev.Address)
		if parseErr == nil && devAddress == address {
			if dev.Connected {
				return platform.LinkConnected, nil
			}
			return platform.LinkDisconnected, nil
		}
	}
	return platform.LinkUnknown, nil
}

func (d *Driver) Disconnect(ctx context.Context, cfg config.PrinterConfig) error {
	if isLegacySerialEndpoint(cfg.Endpoint) {
		return errors.New("legacy /dev/rfcomm endpoints are unsupported")
	}
	address, err := configAddress(cfg)
	if err != nil {
		return err
	}
	if strings.HasPrefix(d.resolvedEndpoint(address, cfg.Endpoint), bleEndpointBase) {
		return nil // the transport does not own the shared BLE device connection
	}
	return d.profiles.Disconnect(ctx, address)
}

func (d *Driver) NewTransport(cfg config.PrinterConfig) transport.Transport {
	if strings.HasPrefix(strings.ToLower(cfg.Endpoint), bleEndpointBase) {
		return newGATTTransport(cfg, d.gatt)
	}
	return newRFCOMMTransport(cfg, d.profiles)
}

// OpenSystemBluetoothSettings starts, but does not wait for, the first
// installed desktop Bluetooth panel.
func (d *Driver) OpenSystemBluetoothSettings(ctx context.Context) error {
	commands := []struct {
		name string
		args []string
	}{
		{"gnome-control-center", []string{"bluetooth"}},
		{"systemsettings6", []string{"kcm_bluetooth"}},
		{"systemsettings5", []string{"kcm_bluetooth"}},
		{"blueman-manager", nil},
	}
	var tried []string
	for _, command := range commands {
		path, err := d.lookPath(command.name)
		if err != nil {
			continue
		}
		tried = append(tried, command.name)
		if err := d.launch(ctx, path, command.args...); err == nil {
			return nil
		} else {
			tried[len(tried)-1] += ": " + err.Error()
		}
	}
	if len(tried) == 0 {
		return errors.New("no supported Bluetooth settings application found (tried gnome-control-center, systemsettings6/5, and blueman-manager)")
	}
	return fmt.Errorf("could not open Bluetooth settings: %s", strings.Join(tried, "; "))
}

func launchDetached(ctx context.Context, path string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap the desktop process without blocking the API
	return nil
}

func configAddress(cfg config.PrinterConfig) (string, error) {
	if cfg.DeviceAddress != "" {
		return normalizeAddress(cfg.DeviceAddress)
	}
	return parseRFCOMMEndpoint(cfg.Endpoint)
}

func normalizeAddress(value string) (string, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(value))
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("invalid Bluetooth MAC address %q", value)
	}
	return strings.ToUpper(hw.String()), nil
}

func parseRFCOMMEndpoint(endpoint string) (string, error) {
	value := strings.TrimSpace(endpoint)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, rfcommEndpointBase) {
		value = value[len(rfcommEndpointBase):]
	} else if strings.HasPrefix(lower, bleEndpointBase) {
		value = value[len(bleEndpointBase):]
	}
	if value == "" {
		return "", errors.New("a Bluetooth device address is required")
	}
	return normalizeAddress(value)
}

func rfcommEndpoint(address string) string { return rfcommEndpointBase + address }
func bleEndpoint(address string) string    { return bleEndpointBase + address }

func isLegacySerialEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "/dev/")
}

func hasUUID(uuids []string, wanted string) bool {
	for _, uuid := range uuids {
		if strings.EqualFold(uuid, wanted) {
			return true
		}
	}
	return false
}

// Bluetooth Class of Device: major class 0x06 is Imaging; bit 0x80 in the
// complete class value is the Imaging/Printer minor capability.
func isPrinterDevice(dev bluezDevice) bool {
	return strings.EqualFold(dev.Icon, "printer") ||
		((dev.Class>>8)&0x1f == 0x06 && dev.Class&0x80 != 0)
}

func isUsableCandidate(dev bluezDevice) bool {
	printer := isPrinterDevice(dev)
	printTransport := hasUUID(dev.UUIDs, serialPortUUID) || hasUUID(dev.UUIDs, blePrintServiceUUID)
	return isConnectionEligible(dev) &&
		(printTransport || printer) &&
		(printer || !isObviouslyNotPrinter(dev))
}

func isConnectionEligible(dev bluezDevice) bool {
	return dev.Paired || dev.Bonded ||
		(isPrinterDevice(dev) && hasUUID(dev.UUIDs, blePrintServiceUUID)) ||
		(dev.Connected && isPrinterDevice(dev) && hasUUID(dev.UUIDs, serialPortUUID))
}

func endpointForDevice(dev bluezDevice, address string) string {
	// Prefer classic SPP for bonded devices. For unbonded cheap printers,
	// their resolved BLE print service is the reliable reconnectable bearer.
	if (dev.Paired || dev.Bonded) && hasUUID(dev.UUIDs, serialPortUUID) {
		return rfcommEndpoint(address)
	}
	if hasUUID(dev.UUIDs, blePrintServiceUUID) {
		return bleEndpoint(address)
	}
	return rfcommEndpoint(address)
}

func (d *Driver) setRoute(address, endpoint string) {
	d.routesMu.Lock()
	if d.routes == nil {
		d.routes = make(map[string]string)
	}
	d.routes[address] = endpoint
	d.routesMu.Unlock()
}

func (d *Driver) resolvedEndpoint(address, configured string) string {
	d.routesMu.Lock()
	endpoint := d.routes[address]
	d.routesMu.Unlock()
	if endpoint != "" {
		return endpoint
	}
	return configured
}

func isObviouslyNotPrinter(dev bluezDevice) bool {
	icon := strings.ToLower(dev.Icon)
	for _, prefix := range []string{"audio-", "input-", "phone", "computer", "network-", "video-"} {
		if strings.HasPrefix(icon, prefix) {
			return true
		}
	}
	// Audio/Video (0x04), Computer (0x01), Phone (0x02), and Peripheral
	// (0x05) major classes are positive evidence against a receipt printer.
	major := (dev.Class >> 8) & 0x1f
	return major == 0x01 || major == 0x02 || major == 0x04 || major == 0x05
}
