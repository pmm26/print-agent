package config

import (
	"testing"
	"time"
)

func TestPrinterConfigValidatesDeviceAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{name: "empty", address: ""},
		{name: "colon separated", address: "AA:BB:CC:DD:EE:FF"},
		{name: "hyphen separated", address: "aa-bb-cc-dd-ee-ff"},
		{name: "malformed", address: "not-an-address", wantErr: true},
		{name: "eight byte address", address: "AA:BB:CC:DD:EE:FF:00:11", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := PrinterConfig{
				ID:                   "kitchen",
				DisplayName:          "Kitchen",
				Transport:            TransportBluetoothSerial,
				DeviceAddress:        test.address,
				Endpoint:             "COM7",
				ConnectionPreference: ConnectionAuto,
				BaudRate:             9600,
				DataBits:             8,
				StopBits:             1,
				Parity:               ParityNone,
				CharactersPerLine:    32,
			}
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestWebSocketDestinationSecurityAndTimingValidation(t *testing.T) {
	valid := WebSocketDestination{ID: "primary", Enabled: true, Endpoint: "wss://events.example.test/v1",
		AuthType: "bearer", SecretRef: "env:PRINT_AGENT_EVENTS_TOKEN", Categories: []string{"printer", "print_run"},
		ConnectTimeout: 10 * time.Second, Heartbeat: 20 * time.Second, StaleTimeout: 60 * time.Second,
		WriteTimeout: 10 * time.Second, ReconnectMin: 2 * time.Second, ReconnectMax: time.Minute,
		ReconnectJitter: .2, AckTimeout: 30 * time.Second, OutboundQueueCapacity: 512}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid destination: %v", err)
	}
	insecure := valid
	insecure.Endpoint = "ws://events.example.test/v1"
	if err := insecure.Validate(); err == nil {
		t.Fatal("non-loopback cleartext WebSocket endpoint was accepted")
	}
	inlineSecret := valid
	inlineSecret.SecretRef = "clear-text-token"
	if err := inlineSecret.Validate(); err == nil {
		t.Fatal("inline credential was accepted")
	}
	staleBeforeHeartbeat := valid
	staleBeforeHeartbeat.StaleTimeout = 10 * time.Second
	if err := staleBeforeHeartbeat.Validate(); err == nil {
		t.Fatal("stale timeout shorter than heartbeat was accepted")
	}
}
