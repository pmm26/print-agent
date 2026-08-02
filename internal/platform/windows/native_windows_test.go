//go:build windows

package windows

import (
	"testing"
	"unsafe"
)

func TestNativeBluetoothStructureLayout(t *testing.T) {
	pointerSize := unsafe.Sizeof(uintptr(0))
	wantSearchSize := uintptr(32)
	wantRadioOffset := uintptr(28)
	if pointerSize == 8 {
		wantSearchSize = 40
		wantRadioOffset = 32
	}
	var params bluetoothSearchParams
	if got := unsafe.Sizeof(params); got != wantSearchSize {
		t.Fatalf("bluetoothSearchParams size = %d, want %d", got, wantSearchSize)
	}
	if got := unsafe.Offsetof(params.Radio); got != wantRadioOffset {
		t.Fatalf("bluetoothSearchParams.Radio offset = %d, want %d", got, wantRadioOffset)
	}

	var info bluetoothDeviceInfo
	if got := unsafe.Sizeof(info); got != 560 {
		t.Fatalf("bluetoothDeviceInfo size = %d, want 560", got)
	}
	if got := unsafe.Offsetof(info.Address); got != 8 {
		t.Fatalf("bluetoothDeviceInfo.Address offset = %d, want 8", got)
	}
}
