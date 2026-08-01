package main

import (
	"context"
	"strings"
	"testing"
)

func TestCmdStatusRejectsPlatformTransportEndpoint(t *testing.T) {
	err := cmdStatus(context.Background(), []string{"rfcomm://AA:BB:CC:DD:EE:FF"})
	if err == nil {
		t.Fatal("cmdStatus returned nil for an RFCOMM endpoint")
	}
	if !strings.Contains(err.Error(), "requires a serial device path") {
		t.Fatalf("cmdStatus error = %q, want serial-device guidance", err)
	}
}
