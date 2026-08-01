package escpos

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"print-agent/internal/config"
)

func testPrinter() config.PrinterConfig {
	p := config.PrinterConfig{ID: "test", Encoding: "CP858", CharactersPerLine: 32}
	p.ApplyDefaults()
	return p
}

func TestWrap(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  []string
	}{
		{"hello", 10, []string{"hello"}},
		{"hello world foo", 11, []string{"hello world", "foo"}},
		{"averylongwordwithoutspaces", 10, []string{"averylongw", "ordwithout", "spaces"}},
		{"a\nb", 10, []string{"a", "b"}},
		{"", 10, []string{""}},
	}
	for _, c := range cases {
		got := wrap(c.in, c.width)
		if len(got) != len(c.want) {
			t.Fatalf("wrap(%q,%d) = %q, want %q", c.in, c.width, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("wrap(%q,%d)[%d] = %q, want %q", c.in, c.width, i, got[i], c.want[i])
			}
		}
	}
}

func TestSpanishEncodingCP858(t *testing.T) {
	cp, err := lookupCodePage("CP858")
	if err != nil {
		t.Fatal(err)
	}
	// CP850/858 byte values for the required Spanish characters.
	want := map[rune]byte{
		'á': 0xA0, 'é': 0x82, 'í': 0xA1, 'ó': 0xA2, 'ú': 0xA3,
		'ñ': 0xA4, 'Ñ': 0xA5, '¿': 0xA8, '¡': 0xAD, '€': 0xD5,
	}
	for r, b := range want {
		got := encodeText(cp, string(r))
		if len(got) != 1 || got[0] != b {
			t.Errorf("encode %q = %#x, want %#x", r, got, b)
		}
	}
	// Unmappable runes must degrade to '?' rather than fail.
	if got := encodeText(cp, "日"); string(got) != "?" {
		t.Errorf("unmappable rune = %q, want ?", got)
	}
}

func TestTwoColumnsWidth(t *testing.T) {
	b, err := NewBuilder("CP437", 32)
	if err != nil {
		t.Fatal(err)
	}
	b.TwoColumns("2x Paella", "24.00 €")
	line, _, found := bytes.Cut(b.Bytes(), []byte{'\n'})
	if !found {
		t.Fatal("no line terminator")
	}
	if len(line) != 32 {
		t.Errorf("line width = %d, want 32: %q", len(line), line)
	}
	if !bytes.HasPrefix(line, []byte("2x Paella")) || !bytes.HasSuffix(line, []byte("24.00 ?")) { // € unmappable in CP437
		t.Errorf("unexpected line: %q", line)
	}
}

func TestTwoColumnsUsesEffectiveDoubleWidth(t *testing.T) {
	b, err := NewBuilder("CP858", 32)
	if err != nil {
		t.Fatal(err)
	}
	b.DoubleSize(true).TwoColumns("TOTAL", "123.45 EUR")
	line, _, ok := bytes.Cut(b.Bytes()[3:], []byte{'\n'})
	if !ok {
		t.Fatal("no line terminator")
	}
	if len(line) > 16 {
		t.Fatalf("double-width line has %d characters, want <= 16: %q", len(line), line)
	}
}

func TestMoneyHandlesMinInt64(t *testing.T) {
	if got := money(math.MinInt64, "EUR"); got != "-92233720368547758.08 EUR" {
		t.Fatalf("money(MinInt64) = %q", got)
	}
}

func TestRenderTemplatesProduceValidDocs(t *testing.T) {
	r := NewRenderer()
	receipt, _ := json.Marshal(map[string]any{
		"orderNumber": "1256",
		"storeName":   "Café Ñandú",
		"items": []map[string]any{
			{"name": "Tortilla española", "quantity": 2, "priceCents": 1250, "notes": "sin cebolla"},
		},
		"totalCents":    2500,
		"paymentMethod": "cash",
	})
	station, _ := json.Marshal(map[string]any{
		"orderNumber": "1256",
		"items":       []map[string]any{{"name": "Tortilla española", "quantity": 2, "notes": "sin cebolla"}},
	})
	for _, tmpl := range TemplateNames() {
		data := json.RawMessage(`{"line":"test"}`)
		if tmpl == TemplateCustomerReceipt {
			data = receipt
		}
		if tmpl == TemplateKitchenTicket || tmpl == TemplateBarTicket {
			data = station
		}
		doc, err := r.Render(tmpl, data, testPrinter(), RenderOptions{})
		if err != nil {
			t.Fatalf("render %s: %v", tmpl, err)
		}
		if !bytes.HasPrefix(doc, []byte{0x1B, '@'}) {
			t.Errorf("%s: missing ESC @ init", tmpl)
		}
		if !bytes.Contains(doc, []byte{0x1D, 'V', 66, 3}) {
			t.Errorf("%s: missing cut command", tmpl)
		}
	}
}

func TestReprintMarker(t *testing.T) {
	r := NewRenderer()
	data, _ := json.Marshal(map[string]any{"orderNumber": "9", "items": []any{map[string]any{"name": "Water", "quantity": 1}}})
	doc, err := r.Render(TemplateKitchenTicket, data, testPrinter(), RenderOptions{Reprint: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc, []byte("*** REPRINT ***")) {
		t.Error("reprint marker missing")
	}
}

func TestTextControlBytesAreSanitized(t *testing.T) {
	cp, err := lookupCodePage("CP858")
	if err != nil {
		t.Fatal(err)
	}
	got := encodeText(cp, "safe\x00\x1b\x1d\x10text")
	for _, control := range []byte{0x00, 0x1b, 0x1d, 0x10} {
		if bytes.Contains(got, []byte{control}) {
			t.Fatalf("encoded text contains control byte %#x", control)
		}
	}
}

func TestUnknownTemplateAndEncoding(t *testing.T) {
	r := NewRenderer()
	if _, err := r.Render("raw-bytes", nil, testPrinter(), RenderOptions{}); err == nil {
		t.Error("unknown template must fail")
	}
	p := testPrinter()
	p.Encoding = "EBCDIC"
	if _, err := r.Render(TemplateTestPage, nil, p, RenderOptions{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported encoding") {
		t.Errorf("bad encoding error = %v", err)
	}
}
