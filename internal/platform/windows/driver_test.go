package windows

import (
	"context"
	"testing"

	"print-agent/internal/config"
	"print-agent/internal/platform"
)

func TestAddressFromInstanceID(t *testing.T) {
	tests := map[string]string{
		`BTHENUM\DEV_001122AABBCC\7&123&0&BLUETOOTHDEVICE_001122AABBCC`:                                            "00:11:22:AA:BB:CC",
		`BTHENUM\{00001101-0000-1000-8000-00805F9B34FB}_VID&00010000_DEV_FFEEDDCCBBAA`:                             "FF:EE:DD:CC:BB:AA",
		`BTHENUM\{00001101-0000-1000-8000-00805f9b34fb}_VID&0001009e_PID&4024\7&241f7ad1&0&4C875D28F57A_C00000000`: "4C:87:5D:28:F5:7A",
		`USB\VID_0403&PID_6001\ABCDEF`: "",
	}
	for input, want := range tests {
		if got := addressFromInstanceID(input); got != want {
			t.Fatalf("addressFromInstanceID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHelpersClassifyAndCleanWindowsPorts(t *testing.T) {
	port := portRecord{
		Name:         "com7",
		FriendlyName: "Standard Serial over Bluetooth link (COM7)",
		Description:  "Bluetooth Receipt Printer",
		Enumerator:   "BTHENUM",
	}
	if normalizeCOMPort(port.Name) != "COM7" {
		t.Fatalf("COM port was not normalized")
	}
	if !isBluetoothPort(port) {
		t.Fatalf("Bluetooth COM port was not recognized")
	}
	if got := displayNameForPort(port); got != "Standard Serial over Bluetooth link" {
		t.Fatalf("displayNameForPort = %q", got)
	}
	if !looksLikePrinter(port.Description) {
		t.Fatalf("printer name heuristic failed")
	}
	if !isPrinterClass(0x0680) {
		t.Fatalf("printer class heuristic failed")
	}
}

func TestListCandidatesCorrelatesBluetoothMetadata(t *testing.T) {
	driver := &Driver{
		ports: func(context.Context) ([]portRecord, error) {
			return []portRecord{
				{Name: "COM9", FriendlyName: "Receipt Printer (COM9)", Enumerator: "BTHENUM", Address: "AA:BB:CC:DD:EE:FF"},
				{Name: "COM1", FriendlyName: "Communications Port (COM1)", Enumerator: "ACPI"},
			}, nil
		},
		bluetooth: func(context.Context) ([]bluetoothRecord, error) {
			return []bluetoothRecord{{Name: "Kitchen Printer", Address: "AA:BB:CC:DD:EE:FF", Class: 0x0680, Connected: true}}, nil
		},
	}
	cands, err := driver.ListCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %#v", cands)
	}
	c := cands[0]
	if c.Endpoint != "COM9" || c.DeviceName != "Kitchen Printer" || c.DeviceAddress != "AA:BB:CC:DD:EE:FF" || !c.Connected || !c.IsPrinter {
		t.Fatalf("candidate = %#v", c)
	}
}

func TestEnsureConnectedResolvesMovedCOMPortByAddress(t *testing.T) {
	driver := &Driver{
		ports: func(context.Context) ([]portRecord, error) {
			return []portRecord{{Name: "COM11", Enumerator: "BTHENUM", Address: "AA:BB:CC:DD:EE:FF"}}, nil
		},
	}
	endpoint, err := driver.EnsureConnected(context.Background(), config.PrinterConfig{
		Endpoint:      "COM4",
		DeviceAddress: "aa-bb-cc-dd-ee-ff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "COM11" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestLinkStateUsesBluetoothWhenKnownAndCOMPresenceFallback(t *testing.T) {
	driver := &Driver{
		ports: func(context.Context) ([]portRecord, error) {
			return []portRecord{{Name: "COM8", Enumerator: "BTHENUM", Address: "AA:BB:CC:DD:EE:FF"}}, nil
		},
		bluetooth: func(context.Context) ([]bluetoothRecord, error) {
			return []bluetoothRecord{{Address: "AA:BB:CC:DD:EE:FF", Authenticated: true, Connected: false}}, nil
		},
	}
	state, err := driver.LinkState(context.Background(), config.PrinterConfig{Endpoint: "COM8"})
	if err != nil {
		t.Fatal(err)
	}
	if state != platform.LinkDisconnected {
		t.Fatalf("state = %s", state)
	}
	if err := driver.VerifyConnected(context.Background(), config.PrinterConfig{Endpoint: "COM8"}); err != platform.ErrNotConnected {
		t.Fatalf("VerifyConnected err = %v", err)
	}

	driver.bluetooth = nil
	state, err = driver.LinkState(context.Background(), config.PrinterConfig{Endpoint: "COM8"})
	if err != nil || state != platform.LinkConnected {
		t.Fatalf("fallback state = %s, err = %v", state, err)
	}
}
