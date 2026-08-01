package printers

import (
	"context"
	"testing"

	"print-agent/internal/config"
	"print-agent/internal/platform"
	"print-agent/internal/transport"
)

type resolvingDriver struct {
	platform.UnimplementedDriver
	verified []string
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
