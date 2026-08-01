package escpos

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

// codePage couples a Go charmap encoder with the ESC t n code-page number
// used by most Epson-compatible ESC/POS printers.
type codePage struct {
	enc     encoding.Encoding
	escposN byte
}

// Code-page numbers follow the common Epson mapping. Cheap printer clones
// occasionally deviate; btprobe's charset sweep exists to verify the mapping
// against real hardware before it is trusted in production.
var codePages = map[string]codePage{
	"CP437":        {charmap.CodePage437, 0},
	"CP850":        {charmap.CodePage850, 2},
	"CP858":        {charmap.CodePage858, 19},
	"CP1252":       {charmap.Windows1252, 16},
	"WINDOWS-1252": {charmap.Windows1252, 16},
}

// SupportedEncodings lists the encoding names accepted in printer config.
func SupportedEncodings() []string {
	return []string{"CP437", "CP850", "CP858", "CP1252"}
}

func lookupCodePage(name string) (codePage, error) {
	cp, ok := codePages[strings.ToUpper(name)]
	if !ok {
		return codePage{}, fmt.Errorf("unsupported encoding %q (supported: %s)",
			name, strings.Join(SupportedEncodings(), ", "))
	}
	return cp, nil
}

// encodeText converts UTF-8 text to the target code page, replacing any
// unmappable rune with '?' so a bad character can never abort a receipt.
func encodeText(cp codePage, s string) []byte {
	enc := cp.enc.NewEncoder()
	out := make([]byte, 0, len(s))
	buf := make([]byte, 4)
	for _, r := range s {
		if unicode.IsControl(r) {
			out = append(out, '?')
			continue
		}
		nDst, _, err := enc.Transform(buf, []byte(string(r)), true)
		enc.Reset()
		if err != nil || nDst == 0 {
			out = append(out, '?')
			continue
		}
		out = append(out, buf[:nDst]...)
	}
	return out
}
