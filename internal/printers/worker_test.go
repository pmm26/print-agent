package printers

import (
	"context"
	"errors"
	"testing"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/events"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

type resolvingDriver struct {
	platform.UnimplementedDriver
	verified []string
}

func waitWorkerState(t *testing.T, w *worker, want ConnectionState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if w.snapshotState() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker state = %s, want %s", w.snapshotState(), want)
}

func TestAutomaticReconnectDisabledParksUntilManualReconnect(t *testing.T) {
	mock := transport.NewMock("mock://kitchen")
	mock.SetConnectFailure(errors.New("powered off"))
	w := newWorker(config.PrinterConfig{ID: "kitchen", Transport: config.TransportMock,
		Endpoint: "mock://kitchen", AutoReconnect: false}, nil, nil,
		&platform.UnimplementedDriver{}, events.NewDiscardPublisher(), func(config.PrinterConfig) transport.Transport { return mock })
	ctx, cancel := context.WithCancel(context.Background())
	w.start(ctx)
	defer func() { cancel(); w.stop() }()
	waitWorkerState(t, w, StateOperatorAction, time.Second)
	mock.SetConnectFailure(nil)
	w.ReconnectNow()
	waitWorkerState(t, w, StateConnected, time.Second)
}

func TestManualReconnectInterruptsBackoff(t *testing.T) {
	mock := transport.NewMock("mock://kitchen")
	mock.SetConnectFailure(errors.New("powered off"))
	w := newWorker(config.PrinterConfig{ID: "kitchen", Transport: config.TransportMock,
		Endpoint: "mock://kitchen", AutoReconnect: true}, nil, nil,
		&platform.UnimplementedDriver{}, events.NewDiscardPublisher(), func(config.PrinterConfig) transport.Transport { return mock })
	ctx, cancel := context.WithCancel(context.Background())
	w.start(ctx)
	defer func() { cancel(); w.stop() }()
	waitWorkerState(t, w, StateReconnectWait, time.Second)
	started := time.Now()
	mock.SetConnectFailure(nil)
	w.ReconnectNow()
	waitWorkerState(t, w, StateConnected, time.Second)
	if time.Since(started) >= reconnectDelay(1) {
		t.Fatal("manual reconnect did not interrupt backoff")
	}
}

func TestReconnectDelaySchedule(t *testing.T) {
	want := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 30 * time.Second}
	for index, expected := range want {
		if got := reconnectDelay(index + 1); got != expected {
			t.Fatalf("attempt %d delay = %s, want %s", index+1, got, expected)
		}
	}
}

func (d *resolvingDriver) EnsureConnected(context.Context, config.PrinterConfig) (string, error) {
	return "ble://AA:BB:CC:DD:EE:FF", nil
}
func (d *resolvingDriver) VerifyConnected(_ context.Context, cfg config.PrinterConfig) error {
	d.verified = append(d.verified, cfg.Endpoint)
	return nil
}

func TestWorkerKeepsResolvedEndpointForVerification(t *testing.T) {
	driver := &resolvingDriver{}
	var factoryEndpoint string
	mock := transport.NewMock("ble://AA:BB:CC:DD:EE:FF")
	w := newWorker(config.PrinterConfig{
		ID: "kitchen", Transport: config.TransportBluetoothSerial, Endpoint: "rfcomm://AA:BB:CC:DD:EE:FF",
	}, nil, nil, driver, nil, func(cfg config.PrinterConfig) transport.Transport {
		factoryEndpoint = cfg.Endpoint
		return mock
	})
	if err := w.connectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if factoryEndpoint != "ble://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("factory endpoint = %q", factoryEndpoint)
	}
	if got := w.connectionConfig().Endpoint; got != "ble://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("active config endpoint = %q", got)
	}
	w.state = StateConnected
	if !w.connectLoop(context.Background()) {
		t.Fatal("connected worker unexpectedly stopped")
	}
	for _, endpoint := range driver.verified {
		if endpoint != "ble://AA:BB:CC:DD:EE:FF" {
			t.Fatalf("verified stale endpoint %q", endpoint)
		}
	}
	if got := w.status().Endpoint; got != "ble://AA:BB:CC:DD:EE:FF" {
		t.Fatalf("status endpoint = %q", got)
	}
	w.closeTransport(StateDisconnected, "test done")
}
