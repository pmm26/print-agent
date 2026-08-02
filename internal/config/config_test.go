package config

import "testing"

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
				ID:                "kitchen",
				DisplayName:       "Kitchen",
				Transport:         TransportBluetoothSerial,
				DeviceAddress:     test.address,
				Endpoint:          "COM7",
				BaudRate:          9600,
				DataBits:          8,
				StopBits:          1,
				Parity:            ParityNone,
				CharactersPerLine: 32,
			}
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
