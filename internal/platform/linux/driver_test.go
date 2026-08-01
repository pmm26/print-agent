//go:build linux

package linux

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"

	"print-agent/internal/config"
	"print-agent/internal/platform"
)

type fakeDeviceSource struct {
	devices []bluezDevice
	err     error
}

type fakeBluetoothController struct {
	started bool
	stopped bool
	address string
	pin     string
	err     error
	onPair  func()
}

func (f *fakeBluetoothController) StartDiscovery(context.Context) error {
	f.started = true
	return f.err
}

func (f *fakeBluetoothController) StopDiscovery(context.Context) error {
	f.stopped = true
	return f.err
}

func (f *fakeBluetoothController) PairDevice(_ context.Context, address, pin string) error {
	f.address = address
	f.pin = pin
	if f.onPair != nil {
		f.onPair()
	}
	return f.err
}

func (f *fakeDeviceSource) Devices(context.Context) ([]bluezDevice, error) {
	return f.devices, f.err
}

type fakeProfileConnector struct {
	connected map[string]bool
	known     map[string]bool
	connect   func(context.Context, string) (rfcommSocket, error)
	discErr   error
}

func (f *fakeProfileConnector) Connect(ctx context.Context, address string) (rfcommSocket, error) {
	if f.connect == nil {
		return nil, errors.New("unexpected connect")
	}
	return f.connect(ctx, address)
}
func (f *fakeProfileConnector) Disconnect(context.Context, string) error { return f.discErr }
func (f *fakeProfileConnector) ConnectionState(address string) (bool, bool) {
	return f.connected[address], f.known[address]
}

func testDriver(source deviceSource, profiles profileConnector) *Driver {
	return &Driver{devices: source, profiles: profiles, routes: make(map[string]string)}
}

func TestListCandidatesFiltersClassifiesAndSorts(t *testing.T) {
	source := &fakeDeviceSource{devices: []bluezDevice{
		{Address: "00:00:00:00:00:03", Name: "Headphones", Icon: "audio-headphones", Paired: true, UUIDs: []string{serialPortUUID}},
		{Address: "00:00:00:00:00:02", Name: "Generic SPP", Bonded: true, UUIDs: []string{serialPortUUID}},
		{Address: "aa-bb-cc-dd-ee-ff", Name: "Receipt", Icon: "printer", Paired: true, Connected: true},
		{Address: "00:00:00:00:00:04", Name: "Connected unbonded printer", Icon: "printer", Connected: true, UUIDs: []string{serialPortUUID}},
		{Address: "00:00:00:00:00:01", Name: "Unpaired and disconnected", Icon: "printer", UUIDs: []string{serialPortUUID}},
		{Address: "invalid", Name: "Broken", Icon: "printer", Paired: true},
	}}
	driver := testDriver(source, &fakeProfileConnector{})

	candidates, err := driver.ListCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidates = %#v, want three usable devices", candidates)
	}
	if candidates[0].Endpoint != "rfcomm://00:00:00:00:00:04" || !candidates[0].IsPrinter || !candidates[0].Connected {
		t.Errorf("connected unbonded candidate = %#v", candidates[0])
	}
	if candidates[1].Endpoint != "rfcomm://AA:BB:CC:DD:EE:FF" || !candidates[1].IsPrinter || !candidates[1].Connected {
		t.Errorf("printer candidate = %#v", candidates[1])
	}
	if candidates[2].Endpoint != "rfcomm://00:00:00:00:00:02" || candidates[2].IsPrinter {
		t.Errorf("SPP candidate = %#v", candidates[2])
	}
}

func TestListCandidatesReportsBlueZFailure(t *testing.T) {
	driver := testDriver(&fakeDeviceSource{err: errors.New("bus down")}, &fakeProfileConnector{})
	_, err := driver.ListCandidates(context.Background())
	if err == nil || !strings.Contains(err.Error(), "query BlueZ devices") {
		t.Fatalf("error = %v", err)
	}
}

func TestListBluetoothDevicesIncludesUnpairedAndAnnotatesEndpoints(t *testing.T) {
	driver := testDriver(&fakeDeviceSource{devices: []bluezDevice{
		{Address: "00:00:00:00:00:03", Name: "Nearby phone", Icon: "phone"},
		{Address: "aa-bb-cc-dd-ee-ff", Name: "Counter", Icon: "printer", Paired: true, UUIDs: []string{serialPortUUID}},
		{Address: "00:00:00:00:00:01", Name: "New printer", Icon: "printer"},
	}}, &fakeProfileConnector{})

	devices, err := driver.ListBluetoothDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 3 {
		t.Fatalf("devices = %#v", devices)
	}
	if devices[0].Address != "AA:BB:CC:DD:EE:FF" || !devices[0].Paired ||
		devices[0].Endpoint != "rfcomm://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("paired printer = %#v", devices[0])
	}
	if devices[1].Name != "New printer" || devices[1].Endpoint != "" {
		t.Fatalf("unpaired printer = %#v", devices[1])
	}
	if devices[2].Name != "Nearby phone" {
		t.Fatalf("generic discovered device = %#v", devices[2])
	}
}

func TestBluetoothActionsDelegateAndNormalizeAddress(t *testing.T) {
	source := &fakeDeviceSource{devices: []bluezDevice{{
		Address: "AA:BB:CC:DD:EE:FF", Name: "Classic printer", Icon: "printer", UUIDs: []string{serialPortUUID},
	}}}
	controller := &fakeBluetoothController{onPair: func() { source.devices[0].Paired = true }}
	driver := testDriver(source, &fakeProfileConnector{})
	driver.bluetooth = controller
	if err := driver.StartBluetoothDiscovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.StopBluetoothDiscovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	device, err := driver.PairBluetoothDevice(context.Background(), "aa-bb-cc-dd-ee-ff", "1234")
	if err != nil {
		t.Fatal(err)
	}
	if !device.Paired || device.Endpoint != "rfcomm://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("paired device = %#v", device)
	}
	if !controller.started || !controller.stopped || controller.address != "AA:BB:CC:DD:EE:FF" || controller.pin != "1234" {
		t.Fatalf("controller calls = %#v", controller)
	}
}

func TestPairBluetoothDeviceReturnsReadyUnbondedBLEWithoutPairing(t *testing.T) {
	source := &fakeDeviceSource{devices: []bluezDevice{{
		Address: "5A:4A:95:56:6F:B6", Name: "BlueTooth Printer", Icon: "printer",
		Connected: true, UUIDs: []string{serialPortUUID, blePrintServiceUUID},
	}}}
	controller := &fakeBluetoothController{}
	driver := testDriver(source, &fakeProfileConnector{})
	driver.bluetooth = controller

	device, err := driver.PairBluetoothDevice(context.Background(), "5A:4A:95:56:6F:B6", "0000")
	if err != nil {
		t.Fatal(err)
	}
	if device.Paired || !device.Connected || device.Endpoint != "ble://5A:4A:95:56:6F:B6" {
		t.Fatalf("ready device = %#v", device)
	}
	if controller.address != "" {
		t.Fatalf("pairing was invoked for a ready BLE printer: %#v", controller)
	}
}

func TestPairBluetoothDeviceValidatesAddressAndPresence(t *testing.T) {
	driver := testDriver(&fakeDeviceSource{}, &fakeProfileConnector{})
	driver.bluetooth = &fakeBluetoothController{}
	if _, err := driver.PairBluetoothDevice(context.Background(), "bad", ""); !errors.Is(err, platform.ErrInvalidBluetoothAddress) {
		t.Fatalf("invalid address error = %v", err)
	}
	if _, err := driver.PairBluetoothDevice(context.Background(), "AA:BB:CC:DD:EE:FF", ""); !errors.Is(err, platform.ErrBluetoothDeviceNotFound) {
		t.Fatalf("missing device error = %v", err)
	}
}

func TestClassifyPairingError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"deadline", context.DeadlineExceeded, platform.ErrBluetoothPairTimeout},
		{"authentication timeout", dbus.Error{Name: "org.bluez.Error.AuthenticationTimeout"}, platform.ErrBluetoothPairTimeout},
		{"in progress", dbus.Error{Name: "org.bluez.Error.InProgress"}, platform.ErrBluetoothPairInProgress},
		{"rejected", dbus.Error{Name: "org.bluez.Error.AuthenticationRejected"}, platform.ErrBluetoothPairRejected},
		{"adapter off", dbus.Error{Name: "org.bluez.Error.NotReady"}, platform.ErrBluetoothUnavailable},
		{"other", dbus.Error{Name: "org.bluez.Error.Failed"}, platform.ErrBluetoothPairFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := classifyPairingError("test", tt.err); !errors.Is(err, tt.want) {
				t.Fatalf("classified error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEnsureConnectedCanonicalizesAndValidates(t *testing.T) {
	source := &fakeDeviceSource{devices: []bluezDevice{
		{Address: "AA:BB:CC:DD:EE:FF", Paired: true, UUIDs: []string{serialPortUUID}},
		{Address: "00:11:22:33:44:55", UUIDs: []string{serialPortUUID}},
		{Address: "00:11:22:33:44:66", Icon: "printer", Connected: true, UUIDs: []string{serialPortUUID}},
	}}
	driver := testDriver(source, &fakeProfileConnector{})

	endpoint, err := driver.EnsureConnected(context.Background(), config.PrinterConfig{
		Endpoint: "aa:bb:cc:dd:ee:ff",
	})
	if err != nil || endpoint != "rfcomm://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("endpoint, err = %q, %v", endpoint, err)
	}
	_, err = driver.EnsureConnected(context.Background(), config.PrinterConfig{Endpoint: "00:11:22:33:44:55"})
	if err == nil || !strings.Contains(err.Error(), "neither paired") {
		t.Fatalf("unpaired error = %v", err)
	}
	endpoint, err = driver.EnsureConnected(context.Background(), config.PrinterConfig{Endpoint: "00:11:22:33:44:66"})
	if err != nil || endpoint != "rfcomm://00:11:22:33:44:66" {
		t.Fatalf("connected unbonded endpoint, err = %q, %v", endpoint, err)
	}
	_, err = driver.EnsureConnected(context.Background(), config.PrinterConfig{Endpoint: "00:11:22:33:44:77"})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("missing error = %v", err)
	}
	_, err = driver.EnsureConnected(context.Background(), config.PrinterConfig{Endpoint: "/dev/rfcomm7"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("legacy endpoint error = %v", err)
	}
}

func TestVerifyConnectedPrefersProfileState(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	source := &fakeDeviceSource{devices: []bluezDevice{{Address: address, Connected: true}}}
	profiles := &fakeProfileConnector{
		connected: map[string]bool{address: false},
		known:     map[string]bool{address: true},
	}
	driver := testDriver(source, profiles)
	cfg := config.PrinterConfig{Endpoint: rfcommEndpoint(address)}
	if err := driver.VerifyConnected(context.Background(), cfg); !errors.Is(err, platform.ErrNotConnected) {
		t.Fatalf("VerifyConnected = %v, want ErrNotConnected", err)
	}
	profiles.connected[address] = true
	if err := driver.VerifyConnected(context.Background(), cfg); err != nil {
		t.Fatalf("connected VerifyConnected = %v", err)
	}
}

func TestVerifyConnectedFallsBackToBlueZAndDegradesGracefully(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	profiles := &fakeProfileConnector{connected: map[string]bool{}, known: map[string]bool{}}
	source := &fakeDeviceSource{devices: []bluezDevice{{Address: address, Connected: false}}}
	driver := testDriver(source, profiles)
	cfg := config.PrinterConfig{DeviceAddress: address}
	if err := driver.VerifyConnected(context.Background(), cfg); !errors.Is(err, platform.ErrNotConnected) {
		t.Fatalf("VerifyConnected = %v", err)
	}
	source.err = errors.New("BlueZ restarted")
	if state, err := driver.LinkState(context.Background(), cfg); err == nil || state != platform.LinkUnknown {
		t.Fatalf("unknown state = %s, %v", state, err)
	}
}

func TestNewTransportSelectsRFCOMMAndGATT(t *testing.T) {
	driver := testDriver(&fakeDeviceSource{}, &fakeProfileConnector{})
	if _, ok := driver.NewTransport(config.PrinterConfig{Endpoint: "rfcomm://AA:BB:CC:DD:EE:FF"}).(*rfcommTransport); !ok {
		t.Fatal("RFCOMM endpoint did not select rfcommTransport")
	}
	if _, ok := driver.NewTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}).(*gattTransport); !ok {
		t.Fatal("BLE endpoint did not select gattTransport")
	}
}

func TestOpenBluetoothSettingsFallback(t *testing.T) {
	var launched []string
	driver := &Driver{
		lookPath: func(name string) (string, error) {
			if name == "gnome-control-center" || name == "blueman-manager" {
				return "/bin/" + name, nil
			}
			return "", errors.New("missing")
		},
		launch: func(_ context.Context, path string, args ...string) error {
			launched = append(launched, path+" "+strings.Join(args, " "))
			if strings.Contains(path, "gnome") {
				return errors.New("no display")
			}
			return nil
		},
	}
	if err := driver.OpenSystemBluetoothSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/gnome-control-center bluetooth", "/bin/blueman-manager "}
	if !reflect.DeepEqual(launched, want) {
		t.Fatalf("launched = %#v, want %#v", launched, want)
	}
}

func TestParseManagedDevicesUsesAliasAndTypedProperties(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	objects := managedObjects{
		path: {deviceInterface: {
			"Address":   dbus.MakeVariant("AA:BB:CC:DD:EE:FF"),
			"Name":      dbus.MakeVariant("Original"),
			"Alias":     dbus.MakeVariant("Counter"),
			"Class":     dbus.MakeVariant(uint32(0x40680)),
			"Paired":    dbus.MakeVariant(true),
			"Connected": dbus.MakeVariant(true),
			"UUIDs":     dbus.MakeVariant([]string{serialPortUUID}),
		}},
	}
	devices := parseManagedDevices(objects)
	if len(devices) != 1 || devices[0].Name != "Counter" || devices[0].Path != path || !isPrinterDevice(devices[0]) {
		t.Fatalf("devices = %#v", devices)
	}
}

func TestEndpointParsing(t *testing.T) {
	for _, input := range []string{"AA:BB:CC:DD:EE:FF", "rfcomm://aa-bb-cc-dd-ee-ff", "ble://aa:bb:cc:dd:ee:ff"} {
		got, err := parseRFCOMMEndpoint(input)
		if err != nil || got != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("parseRFCOMMEndpoint(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := parseRFCOMMEndpoint("rfcomm://not-a-mac"); err == nil {
		t.Fatal("invalid MAC accepted")
	}
}

func TestEndpointForDevicePrefersBLEForUnbondedPrinter(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	dualMode := bluezDevice{Icon: "printer", UUIDs: []string{serialPortUUID, blePrintServiceUUID}}
	if got := endpointForDevice(dualMode, address); got != bleEndpoint(address) {
		t.Fatalf("unbonded endpoint = %q, want BLE", got)
	}
	dualMode.Paired = true
	if got := endpointForDevice(dualMode, address); got != rfcommEndpoint(address) {
		t.Fatalf("paired endpoint = %q, want RFCOMM", got)
	}
}
