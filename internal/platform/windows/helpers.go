package windows

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"print-agent/internal/platform"
)

var (
	bluetoothAddressRE = regexp.MustCompile(`(?i)(?:^|[_\\&])DEV_?([0-9A-F]{12})(?:$|[_\\&])`)
	comSuffixRE        = regexp.MustCompile(`(?i)\s*\(COM\d+\)\s*$`)
)

func normalizeOptionalAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	hw, err := net.ParseMAC(value)
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("%w: %s", platform.ErrInvalidBluetoothAddress, value)
	}
	return strings.ToUpper(hw.String()), nil
}

func normalizeCOMPort(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func sameCOMPort(left, right string) bool {
	return normalizeCOMPort(left) == normalizeCOMPort(right)
}

func addressFromInstanceID(instanceID string) string {
	matches := bluetoothAddressRE.FindStringSubmatch(instanceID)
	if len(matches) != 2 {
		return ""
	}
	raw := strings.ToUpper(matches[1])
	return raw[0:2] + ":" + raw[2:4] + ":" + raw[4:6] + ":" + raw[6:8] + ":" + raw[8:10] + ":" + raw[10:12]
}

func displayNameForPort(p portRecord) string {
	for _, value := range []string{p.FriendlyName, p.Description} {
		value = strings.TrimSpace(comSuffixRE.ReplaceAllString(value, ""))
		if value != "" {
			return value
		}
	}
	return ""
}

func isBluetoothPort(p portRecord) bool {
	for _, value := range []string{p.Enumerator, p.InstanceID, p.FriendlyName, p.Description} {
		if strings.Contains(strings.ToLower(value), "bth") || strings.Contains(strings.ToLower(value), "bluetooth") {
			return true
		}
	}
	return p.Address != ""
}

func isPrinterClass(class uint32) bool {
	return ((class>>8)&0x1f == 0x06) && class&0x80 != 0
}

func looksLikePrinter(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "printer") ||
		strings.Contains(value, "receipt") ||
		strings.Contains(value, "pos-") ||
		strings.Contains(value, "esc/pos") ||
		strings.Contains(value, "escpos")
}

func addressFromUint64(value uint64) string {
	if value == 0 {
		return ""
	}
	raw := fmt.Sprintf("%012X", value&0x0000FFFFFFFFFFFF)
	return raw[0:2] + ":" + raw[2:4] + ":" + raw[4:6] + ":" + raw[6:8] + ":" + raw[8:10] + ":" + raw[10:12]
}
