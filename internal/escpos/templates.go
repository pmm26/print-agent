package escpos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"print-agent/internal/config"
)

const (
	TemplateCustomerReceipt = "customer-receipt"
	TemplateKitchenTicket   = "kitchen-ticket"
	TemplateBarTicket       = "bar-ticket"
	TemplateTestPage        = "test-page"
)

type Renderer struct{}

func NewRenderer() *Renderer { return &Renderer{} }

type renderFunc func(*Builder, json.RawMessage, RenderOptions) error
type RenderOptions struct {
	Reprint    bool
	RunNumber  int
	AcceptedAt time.Time
}

var templates = map[string]renderFunc{
	TemplateCustomerReceipt: renderCustomerReceipt,
	TemplateKitchenTicket:   renderKitchenTicket,
	TemplateBarTicket:       renderBarTicket,
	TemplateTestPage:        renderTestPage,
}

func KnownTemplate(name string) bool { _, ok := templates[name]; return ok }

func TemplateNames() []string {
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Renderer) Render(template string, data json.RawMessage, printer config.PrinterConfig, opts RenderOptions) ([]byte, error) {
	fn, ok := templates[template]
	if !ok {
		return nil, fmt.Errorf("unknown template %q", template)
	}
	if err := ValidateTemplateData(template, data); err != nil {
		return nil, fmt.Errorf("invalid template data: %w", err)
	}
	b, err := NewBuilder(printer.Encoding, printer.CharactersPerLine)
	if err != nil {
		return nil, err
	}
	b.Init()
	if opts.Reprint {
		marker := "*** REPRINT ***"
		if opts.RunNumber > 0 {
			marker = fmt.Sprintf("*** REPRINT - RUN %d ***", opts.RunNumber)
		}
		b.Align(AlignCenter).Bold(true).Text(marker).Bold(false).Align(AlignLeft)
	}
	if err := fn(b, data, opts); err != nil {
		return nil, fmt.Errorf("render %s: %w", template, err)
	}
	b.Feed(4).Cut()
	return b.Bytes(), nil
}

type Item struct {
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	PriceCents int64  `json:"priceCents,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

type stationData struct {
	OrderNumber string    `json:"orderNumber"`
	CreatedAt   time.Time `json:"createdAt,omitzero"`
	Items       []Item    `json:"items"`
}

type receiptData struct {
	StoreName     string    `json:"storeName,omitempty"`
	HeaderLines   []string  `json:"headerLines,omitempty"`
	OrderNumber   string    `json:"orderNumber"`
	CreatedAt     time.Time `json:"createdAt,omitzero"`
	Items         []Item    `json:"items"`
	TotalCents    *int64    `json:"totalCents"`
	Currency      string    `json:"currency,omitempty"`
	PaymentMethod string    `json:"paymentMethod,omitempty"`
	FooterLines   []string  `json:"footerLines,omitempty"`
	OpenDrawer    bool      `json:"openDrawer,omitempty"`
}

type testPageData struct {
	PrinterID string `json:"printerId,omitempty"`
	Line      string `json:"line,omitempty"`
}

func strictDecode(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return errors.New("data is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validateText(name, value string, maxRunes int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s exceeds %d characters", name, maxRunes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validateItems(items []Item, prices bool) error {
	if len(items) == 0 || len(items) > 100 {
		return errors.New("items must contain between 1 and 100 entries")
	}
	for i, item := range items {
		if err := validateText(fmt.Sprintf("items[%d].name", i), item.Name, 200, true); err != nil {
			return err
		}
		if err := validateText(fmt.Sprintf("items[%d].notes", i), item.Notes, 500, false); err != nil {
			return err
		}
		if item.Quantity < 1 || item.Quantity > 999 {
			return fmt.Errorf("items[%d].quantity must be between 1 and 999", i)
		}
		if prices {
			q := int64(item.Quantity)
			if (item.PriceCents > 0 && item.PriceCents > math.MaxInt64/q) ||
				(item.PriceCents < 0 && item.PriceCents < math.MinInt64/q) {
				return fmt.Errorf("items[%d] price times quantity overflows", i)
			}
		}
	}
	return nil
}

func ValidateTemplateData(template string, raw json.RawMessage) error {
	switch template {
	case TemplateKitchenTicket, TemplateBarTicket:
		var d stationData
		if err := strictDecode(raw, &d); err != nil {
			return err
		}
		if err := validateText("orderNumber", d.OrderNumber, 100, true); err != nil {
			return err
		}
		return validateItems(d.Items, false)
	case TemplateCustomerReceipt:
		var d receiptData
		if err := strictDecode(raw, &d); err != nil {
			return err
		}
		if err := validateText("orderNumber", d.OrderNumber, 100, true); err != nil {
			return err
		}
		if d.TotalCents == nil {
			return errors.New("totalCents is required")
		}
		if len(d.HeaderLines) > 20 || len(d.FooterLines) > 20 {
			return errors.New("headerLines/footerLines may contain at most 20 entries")
		}
		for name, value := range map[string]string{"storeName": d.StoreName, "currency": d.Currency, "paymentMethod": d.PaymentMethod} {
			if err := validateText(name, value, 200, false); err != nil {
				return err
			}
		}
		for i, line := range append(append([]string{}, d.HeaderLines...), d.FooterLines...) {
			if err := validateText(fmt.Sprintf("header/footer line %d", i), line, 200, false); err != nil {
				return err
			}
		}
		return validateItems(d.Items, true)
	case TemplateTestPage:
		var d testPageData
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		if err := strictDecode(raw, &d); err != nil {
			return err
		}
		if err := validateText("printerId", d.PrinterID, 100, false); err != nil {
			return err
		}
		return validateText("line", d.Line, 500, false)
	default:
		return fmt.Errorf("unknown template %q", template)
	}
}

func money(cents int64, currency string) string {
	if currency == "" {
		currency = "€"
	}
	negative := cents < 0
	var magnitude uint64
	if negative {
		magnitude = uint64(-(cents + 1)) + 1
	} else {
		magnitude = uint64(cents)
	}
	prefix := ""
	if negative {
		prefix = "-"
	}
	return fmt.Sprintf("%s%d.%02d %s", prefix, magnitude/100, magnitude%100, currency)
}

func renderTimestamp(b *Builder, value time.Time) {
	if !value.IsZero() {
		b.Text(value.Local().Format("02/01/2006 15:04"))
	}
}

func renderCustomerReceipt(b *Builder, raw json.RawMessage, _ RenderOptions) error {
	var d receiptData
	if err := strictDecode(raw, &d); err != nil {
		return err
	}
	b.Align(AlignCenter)
	if d.StoreName != "" {
		b.Bold(true).TextDouble(d.StoreName).Bold(false)
	}
	for _, line := range d.HeaderLines {
		b.Text(line)
	}
	b.Align(AlignLeft).Separator().Bold(true).Text("Pedido " + d.OrderNumber).Bold(false)
	renderTimestamp(b, d.CreatedAt)
	b.Separator()
	for _, item := range d.Items {
		b.TwoColumns(fmt.Sprintf("%dx %s", item.Quantity, item.Name), money(item.PriceCents*int64(item.Quantity), d.Currency))
		if item.Notes != "" {
			b.Text("   " + item.Notes)
		}
	}
	b.Separator().Bold(true).DoubleSize(true)
	b.TwoColumns("TOTAL", money(*d.TotalCents, d.Currency))
	b.DoubleSize(false).Bold(false)
	if d.PaymentMethod != "" {
		b.Text("Pago: " + d.PaymentMethod)
	}
	if len(d.FooterLines) > 0 {
		b.Separator().Align(AlignCenter)
		for _, line := range d.FooterLines {
			b.Text(line)
		}
		b.Align(AlignLeft)
	}
	if d.OpenDrawer {
		b.DrawerPulse()
	}
	return nil
}

func renderStationTicket(title string) renderFunc {
	return func(b *Builder, raw json.RawMessage, _ RenderOptions) error {
		var d stationData
		if err := strictDecode(raw, &d); err != nil {
			return err
		}
		b.Align(AlignCenter).Bold(true).Text(title).Bold(false)
		b.DoubleSize(true).Bold(true).TextDouble("#" + d.OrderNumber).Bold(false).DoubleSize(false)
		renderTimestamp(b, d.CreatedAt)
		b.Align(AlignLeft).Separator()
		for _, item := range d.Items {
			b.Bold(true).TextDouble(fmt.Sprintf("%dx %s", item.Quantity, item.Name)).Bold(false)
			if item.Notes != "" {
				b.Text("   >> " + item.Notes)
			}
		}
		return nil
	}
}

func renderKitchenTicket(b *Builder, raw json.RawMessage, o RenderOptions) error {
	return renderStationTicket("COCINA")(b, raw, o)
}
func renderBarTicket(b *Builder, raw json.RawMessage, o RenderOptions) error {
	return renderStationTicket("BARRA")(b, raw, o)
}

func renderTestPage(b *Builder, raw json.RawMessage, opts RenderOptions) error {
	var d testPageData
	if len(raw) > 0 {
		if err := strictDecode(raw, &d); err != nil {
			return err
		}
	}
	b.Align(AlignCenter).Bold(true).Text("PRINT AGENT").Bold(false).Text("Test page")
	if d.PrinterID != "" {
		b.TextDouble(d.PrinterID)
	}
	stamp := opts.AcceptedAt
	if stamp.IsZero() {
		stamp = time.Now()
	}
	b.Align(AlignLeft).Separator().Text(stamp.Local().Format("02/01/2006 15:04:05"))
	b.Text("Charset: á é í ó ú ñ Ñ ¿ ¡ €").Text("Normal text 1234567890")
	b.Bold(true).Text("Bold text").Bold(false)
	b.DoubleSize(true).TextDouble("Double size").DoubleSize(false)
	b.Align(AlignRight).Text("right").Align(AlignCenter).Text("center").Align(AlignLeft).Text("left")
	if d.Line != "" {
		b.Separator().Text(d.Line)
	}
	b.Separator().Text("OK")
	return nil
}
