//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const profileObjectPath = dbus.ObjectPath("/com/printagent/profile/spp")

type profileBackend interface {
	deviceByAddress(context.Context, string) (bluezDevice, error)
	connectProfile(context.Context, dbus.ObjectPath) error
	disconnectProfile(context.Context, dbus.ObjectPath) error
	registerProfile(context.Context, dbus.ObjectPath, any, any) (any, error)
}

type connectResult struct {
	socket rfcommSocket
	err    error
}

type pendingConnection struct {
	address string
	result  chan connectResult
}

// profileBroker owns the one process-wide SPP profile registration and routes
// the file descriptors BlueZ supplies to the transport waiting for each
// device. BlueZ waits for NewConnection to return before ConnectProfile
// returns, so every waiter is buffered and installed before the method call.
type profileBroker struct {
	backend profileBackend
	profile *profileObject

	registerMu      sync.Mutex
	mu              sync.Mutex
	registration    any
	waiters         map[dbus.ObjectPath]*pendingConnection
	active          map[dbus.ObjectPath]*profileSocket
	connectCalls    map[dbus.ObjectPath]*byte
	states          map[string]bool
	connectTimeout  time.Duration
	disconnectLimit time.Duration
}

func newProfileBroker(backend profileBackend) *profileBroker {
	b := &profileBroker{
		backend:         backend,
		waiters:         make(map[dbus.ObjectPath]*pendingConnection),
		active:          make(map[dbus.ObjectPath]*profileSocket),
		connectCalls:    make(map[dbus.ObjectPath]*byte),
		states:          make(map[string]bool),
		connectTimeout:  30 * time.Second,
		disconnectLimit: 2 * time.Second,
	}
	b.profile = &profileObject{broker: b}
	return b
}

func (b *profileBroker) ensureRegistered(ctx context.Context) error {
	b.registerMu.Lock()
	defer b.registerMu.Unlock()

	b.mu.Lock()
	previous := b.registration
	b.mu.Unlock()
	registration, err := b.backend.registerProfile(ctx, profileObjectPath, b.profile, previous)
	if err != nil {
		return err
	}
	var stale []*profileSocket
	b.mu.Lock()
	if b.registration != nil && b.registration != registration {
		// The bus was replaced. BlueZ discarded the old registration and its
		// descriptors; make the local state reflect that before reconnecting.
		for _, socket := range b.active {
			stale = append(stale, socket)
		}
		b.active = make(map[dbus.ObjectPath]*profileSocket)
		b.connectCalls = make(map[dbus.ObjectPath]*byte)
	}
	b.registration = registration
	b.mu.Unlock()
	for _, socket := range stale {
		_ = socket.close(false)
	}
	return nil
}

func (b *profileBroker) Connect(ctx context.Context, address string) (rfcommSocket, error) {
	connectCtx, cancel := context.WithTimeout(ctx, b.connectTimeout)
	defer cancel()
	address, err := normalizeAddress(address)
	if err != nil {
		return nil, err
	}
	device, err := b.backend.deviceByAddress(connectCtx, address)
	if err != nil {
		return nil, err
	}
	if !isConnectionEligible(device) {
		return nil, fmt.Errorf("Bluetooth device %s is neither paired nor an already-connected SPP printer", address)
	}
	if err := b.ensureRegistered(connectCtx); err != nil {
		return nil, err
	}

	pending := &pendingConnection{address: address, result: make(chan connectResult, 1)}
	b.mu.Lock()
	if _, exists := b.active[device.Path]; exists {
		b.mu.Unlock()
		return nil, fmt.Errorf("RFCOMM profile for %s is already connected", address)
	}
	if _, exists := b.waiters[device.Path]; exists {
		b.mu.Unlock()
		return nil, fmt.Errorf("RFCOMM profile for %s is already connecting", address)
	}
	if b.connectCalls[device.Path] != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("previous BlueZ connection call for %s has not returned", address)
	}
	callToken := new(byte)
	b.waiters[device.Path] = pending
	b.connectCalls[device.Path] = callToken
	b.mu.Unlock()

	callResult := make(chan error, 1)
	go func() {
		err := b.backend.connectProfile(connectCtx, device.Path)
		b.mu.Lock()
		if b.connectCalls[device.Path] == callToken {
			delete(b.connectCalls, device.Path)
		}
		b.mu.Unlock()
		callResult <- err
	}()
	select {
	case result := <-pending.result:
		return result.socket, result.err
	case callErr := <-callResult:
		if result, ok := receiveNow(pending.result); ok {
			return result.socket, result.err
		}
		if callErr != nil {
			b.cancelPending(device.Path, pending)
			return nil, fmt.Errorf("connect SPP profile for %s: %w", address, callErr)
		}
		select {
		case result := <-pending.result:
			return result.socket, result.err
		case <-connectCtx.Done():
			b.cancelPending(device.Path, pending)
			return nil, fmt.Errorf("connect SPP profile for %s: %w", address, connectCtx.Err())
		}
	case <-connectCtx.Done():
		b.cancelPending(device.Path, pending)
		return nil, fmt.Errorf("connect SPP profile for %s: %w", address, connectCtx.Err())
	}
}

func receiveNow(ch <-chan connectResult) (connectResult, bool) {
	select {
	case result := <-ch:
		return result, true
	default:
		return connectResult{}, false
	}
}

func (b *profileBroker) cancelPending(path dbus.ObjectPath, pending *pendingConnection) {
	b.removeWaiter(path, pending)
	if result, ok := receiveNow(pending.result); ok && result.socket != nil {
		result.socket.Close()
	}
	b.setState(pending.address, false)
	ctx, cancel := context.WithTimeout(context.Background(), b.disconnectLimit)
	defer cancel()
	_ = b.backend.disconnectProfile(ctx, path)
}

func (b *profileBroker) removeWaiter(path dbus.ObjectPath, pending *pendingConnection) {
	b.mu.Lock()
	if b.waiters[path] == pending {
		delete(b.waiters, path)
	}
	b.mu.Unlock()
}

func (b *profileBroker) setState(address string, connected bool) {
	b.mu.Lock()
	b.states[address] = connected
	b.mu.Unlock()
}

func (b *profileBroker) ConnectionState(address string) (bool, bool) {
	address, err := normalizeAddress(address)
	if err != nil {
		return false, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	connected, known := b.states[address]
	return connected, known
}

func (b *profileBroker) Disconnect(ctx context.Context, address string) error {
	address, err := normalizeAddress(address)
	if err != nil {
		return err
	}
	device, err := b.backend.deviceByAddress(ctx, address)
	if err != nil {
		return err
	}
	b.mu.Lock()
	socket := b.active[device.Path]
	b.mu.Unlock()
	if socket != nil {
		socket.close(false)
	}
	b.setState(address, false)
	err = b.backend.disconnectProfile(ctx, device.Path)
	if isBlueZNotConnected(err) {
		return nil
	}
	return err
}

func isBlueZNotConnected(err error) bool {
	var dbusErr dbus.Error
	return errors.As(err, &dbusErr) && dbusErr.Name == "org.bluez.Error.NotConnected"
}

func (b *profileBroker) closeSocket(socket *profileSocket, notifyBlueZ bool) error {
	b.mu.Lock()
	if b.active[socket.path] == socket {
		delete(b.active, socket.path)
	}
	b.states[socket.address] = false
	b.mu.Unlock()
	err := socket.closeFileOnly()
	if notifyBlueZ {
		ctx, cancel := context.WithTimeout(context.Background(), b.disconnectLimit)
		defer cancel()
		if disconnectErr := b.backend.disconnectProfile(ctx, socket.path); err == nil && !isBlueZNotConnected(disconnectErr) {
			err = disconnectErr
		}
	}
	return err
}

type profileSocket struct {
	file    *os.File
	broker  *profileBroker
	path    dbus.ObjectPath
	address string
	once    sync.Once
	err     error
}

func (s *profileSocket) Write(data []byte) (int, error) { return s.file.Write(data) }
func (s *profileSocket) SetWriteDeadline(deadline time.Time) error {
	return s.file.SetWriteDeadline(deadline)
}
func (s *profileSocket) Close() error { return s.close(true) }

func (s *profileSocket) close(notifyBlueZ bool) error {
	s.once.Do(func() { s.err = s.broker.closeSocket(s, notifyBlueZ) })
	return s.err
}

func (s *profileSocket) closeFileOnly() error {
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

// profileObject is exported on the system bus as org.bluez.Profile1.
type profileObject struct{ broker *profileBroker }

func (p *profileObject) NewConnection(device dbus.ObjectPath, fd dbus.UnixFD, _ map[string]dbus.Variant) *dbus.Error {
	if fd < 0 {
		return dbus.NewError("org.bluez.Error.Rejected", []any{"invalid RFCOMM file descriptor"})
	}
	p.broker.mu.Lock()
	pending := p.broker.waiters[device]
	if pending == nil || p.broker.active[device] != nil {
		p.broker.mu.Unlock()
		_ = os.NewFile(uintptr(fd), "rejected-rfcomm").Close()
		return dbus.NewError("org.bluez.Error.Rejected", []any{"no pending SPP connection"})
	}
	file := os.NewFile(uintptr(fd), rfcommEndpoint(pending.address))
	if file == nil {
		p.broker.mu.Unlock()
		return dbus.NewError("org.bluez.Error.Rejected", []any{"could not own RFCOMM descriptor"})
	}
	socket := &profileSocket{
		file: file, broker: p.broker, path: device, address: pending.address,
	}
	delete(p.broker.waiters, device)
	p.broker.active[device] = socket
	p.broker.states[pending.address] = true
	p.broker.mu.Unlock()
	pending.result <- connectResult{socket: socket}
	return nil
}

func (p *profileObject) RequestDisconnection(device dbus.ObjectPath) *dbus.Error {
	p.broker.mu.Lock()
	socket := p.broker.active[device]
	p.broker.mu.Unlock()
	if socket != nil {
		_ = socket.close(false)
	}
	return nil
}

func (p *profileObject) Release() *dbus.Error {
	p.broker.mu.Lock()
	sockets := make([]*profileSocket, 0, len(p.broker.active))
	for _, socket := range p.broker.active {
		sockets = append(sockets, socket)
		p.broker.states[socket.address] = false
	}
	p.broker.active = make(map[dbus.ObjectPath]*profileSocket)
	p.broker.registration = nil
	p.broker.mu.Unlock()
	for _, socket := range sockets {
		_ = socket.close(false)
	}
	return nil
}
