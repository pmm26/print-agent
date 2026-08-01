//go:build !windows

package windows

import (
	"context"
	"errors"
)

func nativePortRecords(context.Context) ([]portRecord, error) {
	return nil, errors.New("Windows COM port enumeration is unavailable on this OS")
}

func nativeBluetoothRecords(context.Context) ([]bluetoothRecord, error) {
	return nil, errors.New("Windows Bluetooth device enumeration is unavailable on this OS")
}
