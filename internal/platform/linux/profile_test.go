//go:build linux

package linux

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type fakeProfileBackend struct {
	mu          sync.Mutex
	devices     map[string]bluezDevice
	profile     *profileObject
	token       any
	registers   int
	disconnects int
	connect     func(context.Context, dbus.ObjectPath) error
}

func (f *fakeProfileBackend) deviceByAddress(_ context.Context, address string) (bluezDevice, error) {
	device, ok := f.devices[address]
	if !ok {
		return bluezDevice{}, errors.New("missing device")
	}
	return device, nil
}
func (f *fakeProfileBackend) connectProfile(ctx context.Context, path dbus.ObjectPath) error {
	return f.connect(ctx, path)
}
func (f *fakeProfileBackend) disconnectProfile(context.Context, dbus.ObjectPath) error {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
	return nil
}
func (f *fakeProfileBackend) registerProfile(_ context.Context, _ dbus.ObjectPath, profile any, previous any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token == nil {
		f.token = &struct{}{}
	}
	if previous != f.token {
		f.registers++
		f.profile = profile.(*profileObject)
	}
	return f.token, nil
}

func duplicatePipeWriter(t *testing.T) (*os.File, dbus.UnixFD) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	fd, err := unix.Dup(int(writer.Fd()))
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	return reader, dbus.UnixFD(fd)
}

func TestProfileBrokerConnectRoutesDescriptorAndTracksState(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	backend := &fakeProfileBackend{devices: map[string]bluezDevice{
		address: {Address: address, Path: path, Paired: true},
	}}
	var reader *os.File
	backend.connect = func(_ context.Context, gotPath dbus.ObjectPath) error {
		if gotPath != path {
			t.Fatalf("path = %s, want %s", gotPath, path)
		}
		var fd dbus.UnixFD
		reader, fd = duplicatePipeWriter(t)
		if dbusErr := backend.profile.NewConnection(path, fd, nil); dbusErr != nil {
			t.Fatalf("NewConnection = %v", dbusErr)
		}
		return nil
	}
	broker := newProfileBroker(backend)
	socket, err := broker.Connect(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	if connected, known := broker.ConnectionState(address); !known || !connected {
		t.Fatalf("state = %v, %v", connected, known)
	}
	if _, err := socket.Write([]byte("ticket")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := reader.Read(buf); err != nil || string(buf) != "ticket" {
		t.Fatalf("read = %q, %v", buf, err)
	}
	if dbusErr := backend.profile.RequestDisconnection(path); dbusErr != nil {
		t.Fatal(dbusErr)
	}
	if connected, known := broker.ConnectionState(address); !known || connected {
		t.Fatalf("state after disconnection = %v, %v", connected, known)
	}
	backend.mu.Lock()
	registers := backend.registers
	backend.mu.Unlock()
	if registers != 1 {
		t.Fatalf("profile registrations = %d, want 1", registers)
	}
}

func TestProfileBrokerCancellationRemovesWaiterAndDisconnects(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	backend := &fakeProfileBackend{devices: map[string]bluezDevice{
		address: {Address: address, Path: path, Paired: true},
	}}
	backend.connect = func(context.Context, dbus.ObjectPath) error { return nil }
	broker := newProfileBroker(backend)
	broker.connectTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := broker.Connect(ctx, address)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect = %v, want context.Canceled", err)
	}
	broker.mu.Lock()
	waiters := len(broker.waiters)
	broker.mu.Unlock()
	if waiters != 0 {
		t.Fatalf("waiters = %d, want 0", waiters)
	}
	backend.mu.Lock()
	disconnects := backend.disconnects
	backend.mu.Unlock()
	if disconnects != 1 {
		t.Fatalf("disconnects = %d, want 1", disconnects)
	}
}

func TestProfileBrokerBoundsBackendThatIgnoresContext(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	path := dbus.ObjectPath("/device/blocked")
	backend := &fakeProfileBackend{devices: map[string]bluezDevice{
		address: {Address: address, Path: path, Paired: true},
	}}
	backend.connect = func(context.Context, dbus.ObjectPath) error { select {} }
	broker := newProfileBroker(backend)
	broker.connectTimeout = 20 * time.Millisecond
	start := time.Now()
	_, err := broker.Connect(context.Background(), address)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked backend was not bounded: %s", elapsed)
	}
	start = time.Now()
	if _, err := broker.Connect(context.Background(), address); err == nil || !strings.Contains(err.Error(), "has not returned") {
		t.Fatalf("second Connect = %v, want outstanding-call error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second Connect started another blocked call: %s", elapsed)
	}
}

func TestProfileBrokerConnectsDifferentPrintersConcurrently(t *testing.T) {
	addresses := []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"}
	backend := &fakeProfileBackend{devices: make(map[string]bluezDevice)}
	for i, address := range addresses {
		backend.devices[address] = bluezDevice{
			Address: address,
			Path:    dbus.ObjectPath("/device/" + string(rune('1'+i))),
			Paired:  true,
		}
	}
	backend.connect = func(_ context.Context, path dbus.ObjectPath) error {
		reader, fd := duplicatePipeWriter(t)
		reader.Close()
		if err := backend.profile.NewConnection(path, fd, nil); err != nil {
			return errors.New(err.Error())
		}
		return nil
	}
	broker := newProfileBroker(backend)

	var wg sync.WaitGroup
	errs := make(chan error, len(addresses))
	for _, address := range addresses {
		wg.Go(func() {
			socket, err := broker.Connect(context.Background(), address)
			if err == nil {
				err = socket.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	backend.mu.Lock()
	registers := backend.registers
	backend.mu.Unlock()
	if registers != 1 {
		t.Fatalf("concurrent connects registered profile %d times, want 1", registers)
	}
}

func TestProfileBrokerReregistersAfterBlueZOwnerChanges(t *testing.T) {
	firstAddress := "AA:BB:CC:DD:EE:01"
	secondAddress := "AA:BB:CC:DD:EE:02"
	firstPath := dbus.ObjectPath("/device/1")
	secondPath := dbus.ObjectPath("/device/2")
	backend := &fakeProfileBackend{devices: map[string]bluezDevice{
		firstAddress:  {Address: firstAddress, Path: firstPath, Paired: true},
		secondAddress: {Address: secondAddress, Path: secondPath, Paired: true},
	}}
	backend.connect = func(_ context.Context, path dbus.ObjectPath) error {
		reader, fd := duplicatePipeWriter(t)
		t.Cleanup(func() { reader.Close() })
		if err := backend.profile.NewConnection(path, fd, nil); err != nil {
			return errors.New(err.Error())
		}
		return nil
	}
	broker := newProfileBroker(backend)
	firstSocket, err := broker.Connect(context.Background(), firstAddress)
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.token = &struct{ generation int }{2}
	backend.mu.Unlock()
	secondSocket, err := broker.Connect(context.Background(), secondAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSocket.Close()
	if _, err := firstSocket.Write([]byte("stale")); err == nil {
		t.Fatal("socket from previous BlueZ owner remained writable")
	}
	backend.mu.Lock()
	registers := backend.registers
	backend.mu.Unlock()
	if registers != 2 {
		t.Fatalf("profile registrations = %d, want 2 after owner change", registers)
	}
}

func TestProfileRejectsLateDescriptorAndClosesIt(t *testing.T) {
	backend := &fakeProfileBackend{}
	broker := newProfileBroker(backend)
	reader, fd := duplicatePipeWriter(t)
	_ = reader
	if dbusErr := broker.profile.NewConnection("/late", fd, nil); dbusErr == nil || dbusErr.Name != "org.bluez.Error.Rejected" {
		t.Fatalf("NewConnection error = %#v", dbusErr)
	}
	if _, err := unix.Write(int(fd), []byte("x")); !errors.Is(err, unix.EBADF) {
		t.Fatalf("write after rejection = %v, want EBADF", err)
	}
}

func TestProfileReleaseClosesActiveSockets(t *testing.T) {
	address := "AA:BB:CC:DD:EE:FF"
	path := dbus.ObjectPath("/device")
	backend := &fakeProfileBackend{devices: map[string]bluezDevice{address: {Path: path, Paired: true}}}
	backend.connect = func(context.Context, dbus.ObjectPath) error {
		_, fd := duplicatePipeWriter(t)
		if err := backend.profile.NewConnection(path, fd, nil); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	broker := newProfileBroker(backend)
	socket, err := broker.Connect(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.profile.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := socket.Write([]byte("closed")); err == nil {
		t.Fatal("released socket remained writable")
	}
}
