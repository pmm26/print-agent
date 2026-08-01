package escpos

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"print-agent/internal/config"
)

// Template names accepted by the API. Anything else is rejected at job
// acceptance time — there is no endpoint for raw ESC/POS bytes.
const (
	TemplateCustomerReceipt = "customer-receipt"
	TemplateKitchenTicket   = "kitchen-ticket"
	TemplateBarTicket       = "bar-ticket"
	TemplateTestPage        = "test-page"
)

// Renderer converts structured document data into ESC/POS bytes for a
// specific printer configuration.
type Renderer struct{}

func NewRenderer() *Renderer { return &Renderer{} }

type renderFunc func(b *Builder, data json.RawMessage, opts RenderOptions) error

// RenderOptions carries per-render flags that are not part of the payload.
type RenderOptions struct {
	Reprint bool
}

var templates = map[string]renderFunc{
	TemplateCustomerReceipt: renderCustomerReceipt,
	TemplateKitchenTicket:   renderKitchenTicket,
	TemplateBarTicket:       renderBarTicket,
	TemplateTestPage:        renderTestPage,
}

// KnownTemplate reports whether name is a valid template.
func KnownTemplate(name string) bool {
	_, ok := templates[name]
	return ok
}

// TemplateNames returns the sorted list of valid template names.
func TemplateNames() []string {
	names := make([]string, 0, len(templates))
	for n := range templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Render produces the ESC/POS document for the named template.
func (r *Renderer) Render(template string, data json.RawMessage, printer config.PrinterConfig, opts RenderOptions) ([]byte, error) {
	fn, ok := templates[template]
	if !ok {
		return nil, fmt.Errorf("unknown template %q", template)
	}
	b, err := NewBuilder(printer.Encoding, printer.CharactersPerLine)
	if err != nil {
		return nil, err
	}
	b.Init()
	if opts.Reprint {
		b.Align(AlignCenter).Bold(true).Text("*** REPRINT ***").Bold(false).Align(AlignLeft)
	}
	if err := fn(b, data, opts); err != nil {
		return nil, fmt.Errorf("render %s: %w", template, err)
	}
	b.Feed(4).Cut()
	return b.Bytes(), nil
}

// Item is one order line shared by all ticket templates.
type Item struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	// PriceCents is the unit price in minor currency units.
	PriceCents int64  `json:"priceCents"`
	Notes      string `json:"notes,omitempty"`
}

func (i Item) qty() int {
	if i.Quantity <= 0 {
		return 1
	}
	return i.Quantity
}

type receiptData struct {
	StoreName     string    `json:"storeName,omitempty"`
	HeaderLines   []string  `json:"headerLines,omitempty"`
	OrderNumber   string    `json:"orderNumber"`
	CreatedAt     time.Time `json:"createdAt,omitzero"`
	Items         []Item    `json:"items"`
	TotalCents    int64     `json:"totalCents,omitempty"`
	Total         int64     `json:"total,omitempty"` // legacy alias for totalCents
	Currency      string    `json:"currency,omitempty"`
	PaymentMethod string    `json:"paymentMethod,omitempty"`
	FooterLines   []string  `json:"footerLines,omitempty"`
	OpenDrawer    bool      `json:"openDrawer,omitempty"`
}

func (d receiptData) totalCents() int64 {
	if d.TotalCents != 0 {
		return d.TotalCents
	}
	return d.Total
}

func money(cents int64, currency string) string {
	if currency == "" {
		currency = "€"
	}
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, cents/100, cents%100, currency)
}

func renderTimestamp(b *Builder, t time.Time) {
	if !t.IsZero() {
		b.Text(t.Local().Format("02/01/2006 15:04"))
	}
}

func renderCustomerReceipt(b *Builder, raw json.RawMessage, _ RenderOptions) error {
	var d receiptData
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	b.Align(AlignCenter)
	if d.StoreName != "" {
		b.Bold(true).TextDouble(d.StoreName).Bold(false)
	}
	for _, l := range d.HeaderLines {
		b.Text(l)
	}
	b.Align(AlignLeft).Separator()
	b.Bold(true).Text("Pedido " + d.OrderNumber).Bold(false)
	renderTimestamp(b, d.CreatedAt)
	b.Separator()
	for _, it := range d.Items {
		left := fmt.Sprintf("%dx %s", it.qty(), it.Name)
		b.TwoColumns(left, money(it.PriceCents*int64(it.qty()), d.Currency))
		if it.Notes != "" {
			b.Text("   " + it.Notes)
		}
	}
	b.Separator()
	b.Bold(true).DoubleSize(true)
	b.TwoColumns("TOTAL", money(d.totalCents(), d.Currency))
	b.DoubleSize(false).Bold(false)
	if d.PaymentMethod != "" {
		b.Text("Pago: " + d.PaymentMethod)
	}
	if len(d.FooterLines) > 0 {
		b.Separator().Align(AlignCenter)
		for _, l := range d.FooterLines {
			b.Text(l)
		}
		b.Align(AlignLeft)
	}
	if d.OpenDrawer {
		b.DrawerPulse()
	}
	return nil
}

// station ticket shared by kitchen and bar: big order number, items with
// quantities and notes, no prices.
func renderStationTicket(title string) renderFunc {
	return func(b *Builder, raw json.RawMessage, _ RenderOptions) error {
		var d receiptData
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		b.Align(AlignCenter).Bold(true).Text(title).Bold(false)
		b.DoubleSize(true).Bold(true).TextDouble("#" + d.OrderNumber).Bold(false).DoubleSize(false)
		renderTimestamp(b, d.CreatedAt)
		b.Align(AlignLeft).Separator()
		for _, it := range d.Items {
			b.Bold(true).TextDouble(fmt.Sprintf("%dx %s", it.qty(), it.Name)).Bold(false)
			if it.Notes != "" {
				b.Text("   >> " + it.Notes)
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

type testPageData struct {
	PrinterID string `json:"printerId,omitempty"`
	Line      string `json:"line,omitempty"`
}

func renderTestPage(b *Builder, raw json.RawMessage, _ RenderOptions) error {
	var d testPageData
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	b.Align(AlignCenter).Bold(true).Text("PRINT AGENT").Bold(false)
	b.Text("Test page")
	if d.PrinterID != "" {
		b.TextDouble(d.PrinterID)
	}
	b.Align(AlignLeft).Separator()
	b.Text(time.Now().Format("02/01/2006 15:04:05"))
	b.Text("Charset: á é í ó ú ñ Ñ ¿ ¡ €")
	b.Text("Normal text 1234567890")
	b.Bold(true).Text("Bold text").Bold(false)
	b.DoubleSize(true).TextDouble("Double size").DoubleSize(false)
	b.Align(AlignRight).Text("right").Align(AlignCenter).Text("center").Align(AlignLeft).Text("left")
	if d.Line != "" {
		b.Separator().Text(d.Line)
	}
	b.Separator().Text("OK")
	return nil
}
