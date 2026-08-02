//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	win "golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const bluetoothMaxNameSize = 248

var (
	modBthprops                  = win.NewLazySystemDLL("bthprops.cpl")
	procBluetoothFindFirstDevice = modBthprops.NewProc("BluetoothFindFirstDevice")
	procBluetoothFindNextDevice  = modBthprops.NewProc("BluetoothFindNextDevice")
	procBluetoothFindDeviceClose = modBthprops.NewProc("BluetoothFindDeviceClose")
)

type bluetoothSearchParams struct {
	Size                uint32
	ReturnAuthenticated int32
	ReturnRemembered    int32
	ReturnUnknown       int32
	ReturnConnected     int32
	IssueInquiry        int32
	TimeoutMultiplier   uint8
	_                   [unsafe.Sizeof(uintptr(0)) - 1]byte
	Radio               win.Handle
}

type bluetoothDeviceInfo struct {
	Size          uint32
	_             uint32
	Address       uint64
	Class         uint32
	Connected     int32
	Remembered    int32
	Authenticated int32
	LastSeen      win.Systemtime
	LastUsed      win.Systemtime
	Name          [bluetoothMaxNameSize]uint16
}

func nativePortRecords(ctx context.Context) ([]portRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	guids, err := win.SetupDiClassGuidsFromNameEx("Ports", "")
	if err != nil {
		return nil, err
	}
	var out []portRecord
	var firstSetErr error
	openedSet := false
	for _, guid := range guids {
		set, err := win.SetupDiGetClassDevsEx(&guid, "", 0, win.DIGCF_PRESENT, 0, "")
		if err != nil {
			if firstSetErr == nil {
				firstSetErr = err
			}
			continue
		}
		openedSet = true
		for index := 0; ; index++ {
			if err := ctx.Err(); err != nil {
				set.Close()
				return nil, err
			}
			dev, err := set.EnumDeviceInfo(index)
			if err != nil {
				if errors.Is(err, win.ERROR_NO_MORE_ITEMS) {
					break
				}
				set.Close()
				return nil, fmt.Errorf("enumerate Windows COM devices: %w", err)
			}
			portName, err := portNameFromDevice(set, dev)
			if err != nil || portName == "" {
				continue
			}
			instanceID, _ := set.DeviceInstanceID(dev)
			record := portRecord{
				Name:         portName,
				InstanceID:   instanceID,
				Address:      addressFromInstanceID(instanceID),
				FriendlyName: deviceStringProperty(set, dev, win.SPDRP_FRIENDLYNAME),
				Description:  deviceStringProperty(set, dev, win.SPDRP_DEVICEDESC),
				Enumerator:   deviceStringProperty(set, dev, win.SPDRP_ENUMERATOR_NAME),
			}
			out = append(out, record)
		}
		set.Close()
	}
	if !openedSet && firstSetErr != nil {
		return nil, fmt.Errorf("open Windows COM device information: %w", firstSetErr)
	}
	return out, nil
}

func portNameFromDevice(set win.DevInfo, dev *win.DevInfoData) (string, error) {
	key, err := win.SetupDiOpenDevRegKey(set, dev, win.DICS_FLAG_GLOBAL, 0, win.DIREG_DEV, win.KEY_READ)
	if err != nil {
		return "", err
	}
	defer win.RegCloseKey(key)
	value, _, err := registry.Key(key).GetStringValue("PortName")
	return value, err
}

func deviceStringProperty(set win.DevInfo, dev *win.DevInfoData, prop win.SPDRP) string {
	value, err := set.DeviceRegistryProperty(dev, prop)
	if err != nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func nativeBluetoothRecords(ctx context.Context) ([]bluetoothRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := bluetoothSearchParams{
		ReturnAuthenticated: 1,
		ReturnRemembered:    1,
		ReturnConnected:     1,
	}
	params.Size = uint32(unsafe.Sizeof(params))
	var info bluetoothDeviceInfo
	info.Size = uint32(unsafe.Sizeof(info))

	handle, _, callErr := procBluetoothFindFirstDevice.Call(
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&info)),
	)
	if handle == 0 {
		if errors.Is(callErr, win.ERROR_NO_MORE_ITEMS) || errors.Is(callErr, win.ERROR_NOT_FOUND) {
			return nil, nil
		}
		return nil, callErr
	}
	defer procBluetoothFindDeviceClose.Call(handle)

	var out []bluetoothRecord
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, bluetoothRecordFromNative(info))
		info = bluetoothDeviceInfo{Size: uint32(unsafe.Sizeof(info))}
		ok, _, err := procBluetoothFindNextDevice.Call(handle, uintptr(unsafe.Pointer(&info)))
		if ok == 0 {
			if errors.Is(err, win.ERROR_NO_MORE_ITEMS) {
				break
			}
			return nil, fmt.Errorf("enumerate Windows Bluetooth devices: %w", err)
		}
	}
	return out, nil
}

func bluetoothRecordFromNative(info bluetoothDeviceInfo) bluetoothRecord {
	return bluetoothRecord{
		Name:          syscall.UTF16ToString(info.Name[:]),
		Address:       addressFromUint64(info.Address),
		Class:         info.Class,
		Remembered:    info.Remembered != 0,
		Authenticated: info.Authenticated != 0,
		Connected:     info.Connected != 0,
	}
}
