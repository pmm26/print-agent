//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"print-agent/internal/config"
	"print-agent/internal/platform"
)

type managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

// dbusBlueZ owns a private system-bus connection. A private connection is
// necessary because the profile broker exports callbacks and receives file
// descriptors for the lifetime of the process.
type dbusBlueZ struct {
	mu        sync.Mutex
	conn      *dbus.Conn
	pairingMu sync.Mutex
}

// profileRegistration identifies both the D-Bus connection and the current
// owner of org.bluez. bluetoothd can restart while the system-bus connection
// stays alive; its unique owner changing means profiles must be registered
// again.
type profileRegistration struct {
	conn  *dbus.Conn
	owner string
}

func (b *dbusBlueZ) connection(ctx context.Context) (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && b.conn.Connected() {
		return b.conn, nil
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect to system D-Bus: %w", err)
	}
	b.conn = conn
	return conn, nil
}

func (b *dbusBlueZ) Devices(ctx context.Context) ([]bluezDevice, error) {
	objects, err := b.managedObjects(ctx)
	if err != nil {
		return nil, err
	}
	return parseManagedDevices(objects), nil
}

func (b *dbusBlueZ) adapterPath(ctx context.Context) (dbus.ObjectPath, error) {
	objects, err := b.managedObjects(ctx)
	if err != nil {
		return "", err
	}
	var fallback dbus.ObjectPath
	for path, interfaces := range objects {
		props, ok := interfaces[adapterInterface]
		if !ok {
			continue
		}
		if fallback == "" {
			fallback = path
		}
		powered, _ := variantValue[bool](props, "Powered")
		if powered {
			return path, nil
		}
	}
	if fallback != "" {
		return "", errors.New("Bluetooth adapter is powered off")
	}
	return "", errors.New("no Bluetooth adapter found")
}

func (b *dbusBlueZ) StartDiscovery(ctx context.Context, connectionType config.ConnectionPreference) error {
	path, err := b.adapterPath(ctx)
	if err != nil {
		return err
	}
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	transportType := "auto"
	if connectionType == config.ConnectionRFCOMM {
		transportType = "bredr"
	}
	if connectionType == config.ConnectionBLE {
		transportType = "le"
	}
	if err := conn.Object(bluezService, path).CallWithContext(ctx,
		adapterInterface+".SetDiscoveryFilter", 0, map[string]dbus.Variant{
			"Transport": dbus.MakeVariant(transportType),
		}).Err; err != nil {
		return fmt.Errorf("set Bluetooth discovery transport %s: %w", transportType, err)
	}
	err = conn.Object(bluezService, path).CallWithContext(
		ctx, adapterInterface+".StartDiscovery", 0).Err
	if err != nil && !isBlueZError(err, "org.bluez.Error.InProgress") {
		return fmt.Errorf("start Bluetooth discovery: %w", err)
	}
	return nil
}

func (b *dbusBlueZ) DisconnectDevice(ctx context.Context, address string) error {
	device, err := b.deviceByAddress(ctx, address)
	if err != nil {
		return fmt.Errorf("%w: %v", platform.ErrBluetoothDeviceNotFound, err)
	}
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	err = conn.Object(bluezService, device.Path).CallWithContext(ctx, deviceInterface+".Disconnect", 0).Err
	if isBlueZError(err, "org.bluez.Error.NotConnected") {
		return nil
	}
	return classifyBluetoothManagementError(err)
}

func (b *dbusBlueZ) ForgetDevice(ctx context.Context, address string) error {
	device, err := b.deviceByAddress(ctx, address)
	if err != nil {
		return fmt.Errorf("%w: %v", platform.ErrBluetoothDeviceNotFound, err)
	}
	path, err := b.adapterPath(ctx)
	if err != nil {
		return err
	}
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	return classifyBluetoothManagementError(conn.Object(bluezService, path).CallWithContext(ctx, adapterInterface+".RemoveDevice", 0, device.Path).Err)
}

func classifyBluetoothManagementError(err error) error {
	if err == nil {
		return nil
	}
	if isBlueZError(err, "org.bluez.Error.NotAuthorized") || isBlueZError(err, "org.freedesktop.DBus.Error.AccessDenied") {
		return fmt.Errorf("%w: %v", platform.ErrBluetoothNotAuthorized, err)
	}
	return err
}

func (b *dbusBlueZ) StopDiscovery(ctx context.Context) error {
	path, err := b.adapterPath(ctx)
	if err != nil {
		return err
	}
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	err = conn.Object(bluezService, path).CallWithContext(
		ctx, adapterInterface+".StopDiscovery", 0).Err
	if err != nil && !isBlueZError(err, "org.bluez.Error.NotReady") &&
		!isBlueZError(err, "org.bluez.Error.Failed") {
		return fmt.Errorf("stop Bluetooth discovery: %w", err)
	}
	return nil
}

// PairDevice uses a short-lived application agent so pairing also works on a
// headless Linux host. The chosen PIN is returned for legacy printers;
// confirmation-only devices are accepted because the exact address was
// selected from the loopback-only management UI.
func (b *dbusBlueZ) PairDevice(ctx context.Context, address, pin string) error {
	b.pairingMu.Lock()
	defer b.pairingMu.Unlock()
	pairCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	device, err := b.deviceByAddress(pairCtx, address)
	if err != nil {
		return fmt.Errorf("%w: %v", platform.ErrBluetoothDeviceNotFound, err)
	}
	if device.Paired || device.Bonded {
		return nil
	}
	conn, err := b.connection(pairCtx)
	if err != nil {
		return fmt.Errorf("%w: connect to BlueZ: %v", platform.ErrBluetoothUnavailable, err)
	}
	agent := &bluezPairingAgent{pin: strings.TrimSpace(pin)}
	if agent.pin == "" {
		agent.pin = "0000"
	}
	if err := conn.Export(agent, pairingAgentPath, agentInterface); err != nil {
		return classifyPairingError("export Bluetooth pairing agent", err)
	}
	defer conn.Export(nil, pairingAgentPath, agentInterface)

	manager := conn.Object(bluezService, dbus.ObjectPath("/org/bluez"))
	if err := manager.CallWithContext(pairCtx, agentManager+".RegisterAgent", 0,
		pairingAgentPath, "KeyboardDisplay").Err; err != nil {
		return classifyPairingError("register Bluetooth pairing agent", err)
	}
	defer manager.Call(agentManager+".UnregisterAgent", 0, pairingAgentPath)

	err = conn.Object(bluezService, device.Path).CallWithContext(
		pairCtx, deviceInterface+".Pair", 0).Err
	if err != nil && !isBlueZError(err, "org.bluez.Error.AlreadyExists") {
		return classifyPairingError("pair Bluetooth device "+address, err)
	}
	return nil
}

type bluezPairingAgent struct{ pin string }

func (*bluezPairingAgent) Release() *dbus.Error { return nil }

func (a *bluezPairingAgent) RequestPinCode(dbus.ObjectPath) (string, *dbus.Error) {
	return a.pin, nil
}

func (a *bluezPairingAgent) RequestPasskey(dbus.ObjectPath) (uint32, *dbus.Error) {
	var passkey uint32
	for _, char := range a.pin {
		if char < '0' || char > '9' {
			return 0, dbus.MakeFailedError(errors.New("Bluetooth PIN must be numeric for passkey pairing"))
		}
		passkey = passkey*10 + uint32(char-'0')
	}
	return passkey, nil
}

func (*bluezPairingAgent) DisplayPinCode(dbus.ObjectPath, string) *dbus.Error { return nil }
func (*bluezPairingAgent) DisplayPasskey(dbus.ObjectPath, uint32, uint16) *dbus.Error {
	return nil
}
func (*bluezPairingAgent) RequestConfirmation(dbus.ObjectPath, uint32) *dbus.Error {
	return nil
}
func (*bluezPairingAgent) RequestAuthorization(dbus.ObjectPath) *dbus.Error { return nil }
func (*bluezPairingAgent) AuthorizeService(dbus.ObjectPath, string) *dbus.Error {
	return nil
}
func (*bluezPairingAgent) Cancel() *dbus.Error { return nil }

func (b *dbusBlueZ) managedObjects(ctx context.Context) (managedObjects, error) {
	conn, err := b.connection(ctx)
	if err != nil {
		return nil, err
	}
	var objects managedObjects
	call := conn.Object(bluezService, dbus.ObjectPath("/")).CallWithContext(
		ctx, objectManager+".GetManagedObjects", 0)
	if err := call.Store(&objects); err != nil {
		return nil, err
	}
	return objects, nil
}

func parseManagedDevices(objects managedObjects) []bluezDevice {
	devices := make([]bluezDevice, 0)
	for path, interfaces := range objects {
		props, ok := interfaces[deviceInterface]
		if !ok {
			continue
		}
		dev := bluezDevice{Path: path}
		dev.Address, _ = variantValue[string](props, "Address")
		dev.Name, _ = variantValue[string](props, "Alias")
		if dev.Name == "" {
			dev.Name, _ = variantValue[string](props, "Name")
		}
		dev.Icon, _ = variantValue[string](props, "Icon")
		dev.Class, _ = variantValue[uint32](props, "Class")
		dev.Paired, _ = variantValue[bool](props, "Paired")
		dev.Bonded, _ = variantValue[bool](props, "Bonded")
		dev.Connected, _ = variantValue[bool](props, "Connected")
		dev.UUIDs, _ = variantValue[[]string](props, "UUIDs")
		devices = append(devices, dev)
	}
	return devices
}

func variantValue[T any](props map[string]dbus.Variant, name string) (T, bool) {
	var zero T
	v, ok := props[name]
	if !ok {
		return zero, false
	}
	value, ok := v.Value().(T)
	return value, ok
}

func (b *dbusBlueZ) deviceByAddress(ctx context.Context, address string) (bluezDevice, error) {
	devices, err := b.Devices(ctx)
	if err != nil {
		return bluezDevice{}, err
	}
	for _, dev := range devices {
		candidate, parseErr := normalizeAddress(dev.Address)
		if parseErr == nil && candidate == address {
			return dev, nil
		}
	}
	return bluezDevice{}, fmt.Errorf("Bluetooth device %s is not available from BlueZ", address)
}

func (b *dbusBlueZ) connectProfile(ctx context.Context, path dbus.ObjectPath) error {
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	return conn.Object(bluezService, path).CallWithContext(
		ctx, deviceInterface+".ConnectProfile", 0, serialPortUUID).Err
}

func (b *dbusBlueZ) connectDevice(ctx context.Context, path dbus.ObjectPath) error {
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	return conn.Object(bluezService, path).CallWithContext(
		ctx, deviceInterface+".Connect", 0).Err
}

func (b *dbusBlueZ) disconnectProfile(ctx context.Context, path dbus.ObjectPath) error {
	conn, err := b.connection(ctx)
	if err != nil {
		return err
	}
	return conn.Object(bluezService, path).CallWithContext(
		ctx, deviceInterface+".DisconnectProfile", 0, serialPortUUID).Err
}

func (b *dbusBlueZ) registerProfile(ctx context.Context, path dbus.ObjectPath, profile any, previous any) (any, error) {
	conn, err := b.connection(ctx)
	if err != nil {
		return nil, err
	}
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, bluezService).Store(&owner); err != nil {
		return nil, fmt.Errorf("find BlueZ D-Bus owner: %w", err)
	}
	registration := profileRegistration{conn: conn, owner: owner}
	if previous == registration {
		return registration, nil
	}
	if !conn.SupportsUnixFDs() {
		return nil, fmt.Errorf("system D-Bus connection does not support Unix file descriptor passing")
	}
	if err := conn.Export(profile, path, "org.bluez.Profile1"); err != nil {
		return nil, fmt.Errorf("export BlueZ SPP profile: %w", err)
	}
	options := map[string]dbus.Variant{
		"Name": dbus.MakeVariant("Print Agent SPP"),
		"Role": dbus.MakeVariant("client"),
		// Several inexpensive printers expose an unauthenticated SPP service
		// and do not retain a BlueZ bond. The connection is still outbound to
		// the exact MAC selected by the operator.
		"RequireAuthentication": dbus.MakeVariant(false),
		"RequireAuthorization":  dbus.MakeVariant(false),
		"AutoConnect":           dbus.MakeVariant(false),
	}
	err = conn.Object(bluezService, dbus.ObjectPath("/org/bluez")).CallWithContext(ctx,
		"org.bluez.ProfileManager1.RegisterProfile", 0, path, serialPortUUID, options).Err
	if err != nil {
		return nil, fmt.Errorf("register BlueZ SPP profile: %w", err)
	}
	return registration, nil
}

type gattTarget struct {
	path      dbus.ObjectPath
	mtu       uint16
	writeType string
}

func (b *dbusBlueZ) ConnectGATT(ctx context.Context, address string) (gattConnection, error) {
	address, err := normalizeAddress(address)
	if err != nil {
		return nil, err
	}
	device, err := b.deviceByAddress(ctx, address)
	if err != nil {
		return nil, err
	}
	if !device.Connected {
		if connectErr := b.connectDevice(ctx, device.Path); connectErr != nil && !isBlueZError(connectErr, "org.bluez.Error.AlreadyConnected") {
			// Dual-mode receipt printers can establish their LE/GATT link and
			// resolve services, then make Device1.Connect return
			// BREDR.ProfileUnavailable because their advertised classic SPP
			// bearer is not usable. Trust the resulting device state instead of
			// discarding a successful BLE connection because its classic bearer
			// failed afterward.
			refreshed, refreshErr := b.deviceByAddress(ctx, address)
			if refreshErr != nil || !refreshed.Connected {
				return nil, fmt.Errorf("connect BLE device %s: %w", address, connectErr)
			}
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		target, err := b.findGATTTarget(waitCtx, device.Path)
		if err == nil {
			return &dbusGATTConnection{backend: b, target: target}, nil
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("resolve BLE print characteristic for %s: %w", address, waitCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (b *dbusBlueZ) findGATTTarget(ctx context.Context, devicePath dbus.ObjectPath) (gattTarget, error) {
	objects, err := b.managedObjects(ctx)
	if err != nil {
		return gattTarget{}, err
	}
	return parseGATTTarget(objects, devicePath)
}

func parseGATTTarget(objects managedObjects, devicePath dbus.ObjectPath) (gattTarget, error) {
	services := make(map[dbus.ObjectPath]bool)
	for path, interfaces := range objects {
		props, ok := interfaces["org.bluez.GattService1"]
		if !ok {
			continue
		}
		uuid, _ := variantValue[string](props, "UUID")
		device, _ := variantValue[dbus.ObjectPath](props, "Device")
		if device == devicePath && strings.EqualFold(uuid, blePrintServiceUUID) {
			services[path] = true
		}
	}
	for path, interfaces := range objects {
		props, ok := interfaces["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		uuid, _ := variantValue[string](props, "UUID")
		service, _ := variantValue[dbus.ObjectPath](props, "Service")
		if !services[service] || !strings.EqualFold(uuid, blePrintCharUUID) {
			continue
		}
		flags, _ := variantValue[[]string](props, "Flags")
		writeType := "request"
		if !containsString(flags, "write") {
			writeType = "command"
		}
		if writeType == "command" && !containsString(flags, "write-without-response") {
			continue
		}
		mtu, _ := variantValue[uint16](props, "MTU")
		if mtu < 23 {
			mtu = 23
		}
		return gattTarget{path: path, mtu: mtu, writeType: writeType}, nil
	}
	return gattTarget{}, fmt.Errorf("BLE print characteristic %s not resolved", blePrintCharUUID)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type dbusGATTConnection struct {
	backend *dbusBlueZ
	target  gattTarget
}

func (c *dbusGATTConnection) MTU() int { return int(c.target.mtu) }

func (c *dbusGATTConnection) WriteValue(ctx context.Context, data []byte) error {
	conn, err := c.backend.connection(ctx)
	if err != nil {
		return err
	}
	options := map[string]dbus.Variant{"type": dbus.MakeVariant(c.target.writeType)}
	return conn.Object(bluezService, c.target.path).CallWithContext(ctx,
		"org.bluez.GattCharacteristic1.WriteValue", 0, data, options).Err
}

func (c *dbusGATTConnection) Close() error { return nil }

func isBlueZError(err error, name string) bool {
	var dbusErr dbus.Error
	return errors.As(err, &dbusErr) && dbusErr.Name == name
}

func classifyPairingError(action string, err error) error {
	kind := platform.ErrBluetoothPairFailed
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		isBlueZError(err, "org.bluez.Error.AuthenticationTimeout"):
		kind = platform.ErrBluetoothPairTimeout
	case isBlueZError(err, "org.bluez.Error.InProgress"),
		isBlueZError(err, "org.bluez.Error.AlreadyExists"):
		kind = platform.ErrBluetoothPairInProgress
	case isBlueZError(err, "org.bluez.Error.AuthenticationCanceled"),
		isBlueZError(err, "org.bluez.Error.AuthenticationFailed"),
		isBlueZError(err, "org.bluez.Error.AuthenticationRejected"):
		kind = platform.ErrBluetoothPairRejected
	case isBlueZError(err, "org.bluez.Error.NotReady"):
		kind = platform.ErrBluetoothUnavailable
	}
	return fmt.Errorf("%w: %s: %v", kind, action, err)
}
