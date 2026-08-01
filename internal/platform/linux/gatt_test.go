//go:build linux

package linux

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"print-agent/internal/config"
	"print-agent/internal/transport"
)

type fakeGATTConnector struct {
	mu       sync.Mutex
	connects int
	connect  func(context.Context, string) (gattConnection, error)
}

func (f *fakeGATTConnector) ConnectGATT(ctx context.Context, address string) (gattConnection, error) {
	f.mu.Lock()
	f.connects++
	f.mu.Unlock()
	return f.connect(ctx, address)
}

type fakeGATTConnection struct {
	mu        sync.Mutex
	mtu       int
	writes    [][]byte
	writeFunc func(context.Context, []byte) error
	closed    bool
}

func (f *fakeGATTConnection) MTU() int { return f.mtu }
func (f *fakeGATTConnection) WriteValue(ctx context.Context, data []byte) error {
	if f.writeFunc != nil {
		return f.writeFunc(ctx, data)
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	f.mu.Unlock()
	return nil
}
func (f *fakeGATTConnection) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func TestGATTTransportChunksAtMTUMinusThree(t *testing.T) {
	connection := &fakeGATTConnection{mtu: 240}
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return connection, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}, connector)
	tr.chunkDelay = 0
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Write(context.Background(), make([]byte, 500)); err != nil {
		t.Fatal(err)
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if len(connection.writes) != 3 || len(connection.writes[0]) != 237 || len(connection.writes[1]) != 237 || len(connection.writes[2]) != 26 {
		t.Fatalf("writes = %#v, want chunks 237/237/26", connection.writes)
	}
}

func TestGATTTransportUsesDefaultMTUAndHandlesEmptyWrite(t *testing.T) {
	connection := &fakeGATTConnection{mtu: 0}
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return connection, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{DeviceAddress: "AA:BB:CC:DD:EE:FF"}, connector)
	tr.chunkDelay = 0
	_ = tr.Connect(context.Background())
	if err := tr.Write(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.Write(context.Background(), make([]byte, 21)); err != nil {
		t.Fatal(err)
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if len(connection.writes) != 2 || len(connection.writes[0]) != 20 || len(connection.writes[1]) != 1 {
		t.Fatalf("default-MTU writes = %#v", connection.writes)
	}
}

func TestGATTTransportReportsAcceptedChunksOnFailure(t *testing.T) {
	connection := &fakeGATTConnection{mtu: 240}
	calls := 0
	connection.writeFunc = func(_ context.Context, data []byte) error {
		calls++
		if calls == 2 {
			return errors.New("link lost")
		}
		return nil
	}
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return connection, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}, connector)
	tr.chunkDelay = 0
	_ = tr.Connect(context.Background())
	err := tr.Write(context.Background(), make([]byte, 300))
	var writeErr *transport.WriteError
	if !errors.As(err, &writeErr) || writeErr.BytesWritten != 237 {
		t.Fatalf("Write = %#v, want 237 accepted bytes", err)
	}
	connection.mu.Lock()
	closed := connection.closed
	connection.mu.Unlock()
	if !closed {
		t.Fatal("failed GATT write did not close the connection")
	}
}

func TestGATTTransportClassifiesWriteTimeout(t *testing.T) {
	connection := &fakeGATTConnection{mtu: 240}
	connection.writeFunc = func(ctx context.Context, _ []byte) error {
		<-ctx.Done()
		return ctx.Err()
	}
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return connection, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}, connector)
	tr.writeTimeout = 20 * time.Millisecond
	_ = tr.Connect(context.Background())
	err := tr.Write(context.Background(), []byte("ticket"))
	if !errors.Is(err, transport.ErrWriteTimeout) {
		t.Fatalf("Write = %v, want ErrWriteTimeout", err)
	}
}

func TestGATTTransportTreatsInFlightCancellationAsAmbiguous(t *testing.T) {
	connection := &fakeGATTConnection{mtu: 240}
	started := make(chan struct{})
	connection.writeFunc = func(ctx context.Context, _ []byte) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return connection, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}, connector)
	_ = tr.Connect(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	err := tr.Write(ctx, []byte("ticket"))
	if !errors.Is(err, transport.ErrWriteTimeout) {
		t.Fatalf("Write = %v, want ambiguous timeout classification", err)
	}
}

func TestGATTTransportConnectIsIdempotentAndReconnects(t *testing.T) {
	connector := &fakeGATTConnector{connect: func(context.Context, string) (gattConnection, error) {
		return &fakeGATTConnection{mtu: 23}, nil
	}}
	tr := newGATTTransport(config.PrinterConfig{Endpoint: "ble://AA:BB:CC:DD:EE:FF"}, connector)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	connector.mu.Lock()
	connects := connector.connects
	connector.mu.Unlock()
	if connects != 1 {
		t.Fatalf("connects = %d, want 1", connects)
	}
	_ = tr.Close()
	_ = tr.Close()
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	connector.mu.Lock()
	connects = connector.connects
	connector.mu.Unlock()
	if connects != 2 {
		t.Fatalf("connects after close = %d, want 2", connects)
	}
}

func TestParseGATTTargetScopesCharacteristicToPrinterService(t *testing.T) {
	device := dbus.ObjectPath("/device")
	service := dbus.ObjectPath("/device/service18f0")
	characteristic := dbus.ObjectPath("/device/service18f0/char2af1")
	objects := managedObjects{
		service: {"org.bluez.GattService1": {
			"UUID": dbus.MakeVariant(blePrintServiceUUID), "Device": dbus.MakeVariant(device),
		}},
		characteristic: {"org.bluez.GattCharacteristic1": {
			"UUID": dbus.MakeVariant(blePrintCharUUID), "Service": dbus.MakeVariant(service),
			"Flags": dbus.MakeVariant([]string{"write-without-response", "write"}), "MTU": dbus.MakeVariant(uint16(240)),
		}},
		"/other/service/char": {"org.bluez.GattCharacteristic1": {
			"UUID": dbus.MakeVariant(blePrintCharUUID), "Service": dbus.MakeVariant(dbus.ObjectPath("/other/service")),
			"Flags": dbus.MakeVariant([]string{"write"}),
		}},
	}
	target, err := parseGATTTarget(objects, device)
	if err != nil {
		t.Fatal(err)
	}
	if target.path != characteristic || target.mtu != 240 || target.writeType != "request" {
		t.Fatalf("target = %#v", target)
	}
}

func TestParseGATTTargetRequiresWritableCharacteristic(t *testing.T) {
	device := dbus.ObjectPath("/device")
	service := dbus.ObjectPath("/device/service")
	objects := managedObjects{
		service: {"org.bluez.GattService1": {
			"UUID": dbus.MakeVariant(blePrintServiceUUID), "Device": dbus.MakeVariant(device),
		}},
		"/device/service/char": {"org.bluez.GattCharacteristic1": {
			"UUID": dbus.MakeVariant(blePrintCharUUID), "Service": dbus.MakeVariant(service),
			"Flags": dbus.MakeVariant([]string{"notify"}),
		}},
	}
	if _, err := parseGATTTarget(objects, device); err == nil {
		t.Fatal("non-writable characteristic accepted")
	}
}

func TestHardwareGATTConnect(t *testing.T) {
	address := os.Getenv("PRINT_AGENT_TEST_BLE_ADDRESS")
	if address == "" {
		t.Skip("set PRINT_AGENT_TEST_BLE_ADDRESS for a read-only hardware connection check")
	}
	backend := &dbusBlueZ{}
	connection, err := backend.ConnectGATT(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if connection.MTU() < 23 {
		t.Fatalf("negotiated MTU = %d", connection.MTU())
	}
}
