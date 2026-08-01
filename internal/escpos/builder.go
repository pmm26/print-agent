// Package escpos renders structured receipt data into ESC/POS byte streams
// for 58 mm thermal printers.
package escpos

import (
	"strings"
	"unicode/utf8"
)

// Alignment values for Builder.Align.
const (
	AlignLeft   = 0
	AlignCenter = 1
	AlignRight  = 2
)

// Builder accumulates ESC/POS commands for one document. It is not safe for
// concurrent use; create one per render.
type Builder struct {
	buf   []byte
	cp    codePage
	width int // characters per line at normal size
	err   error
}

// NewBuilder creates a builder for the given encoding name and line width.
func NewBuilder(encodingName string, width int) (*Builder, error) {
	cp, err := lookupCodePage(encodingName)
	if err != nil {
		return nil, err
	}
	if width <= 0 {
		width = 32
	}
	return &Builder{cp: cp, width: width}, nil
}

// Init resets the printer and selects the configured code page.
func (b *Builder) Init() *Builder {
	b.raw(0x1B, '@')               // ESC @ initialize
	b.raw(0x1B, 't', b.cp.escposN) // ESC t n select code page
	return b
}

// Align sets text alignment (AlignLeft/AlignCenter/AlignRight).
func (b *Builder) Align(a byte) *Builder { return b.raw(0x1B, 'a', a) }

// Bold toggles emphasized mode.
func (b *Builder) Bold(on bool) *Builder {
	v := byte(0)
	if on {
		v = 1
	}
	return b.raw(0x1B, 'E', v)
}

// DoubleSize toggles double-width double-height characters.
func (b *Builder) DoubleSize(on bool) *Builder {
	v := byte(0)
	if on {
		v = 0x30 // GS ! n: width x2 | height x2
	}
	return b.raw(0x1D, '!', v)
}

// Text writes the string wrapped to the line width, ending each line with LF.
func (b *Builder) Text(s string) *Builder {
	for _, line := range wrap(s, b.width) {
		b.buf = append(b.buf, encodeText(b.cp, line)...)
		b.buf = append(b.buf, '\n')
	}
	return b
}

// TextDouble writes text wrapped for double-size characters (half width).
func (b *Builder) TextDouble(s string) *Builder {
	for _, line := range wrap(s, b.width/2) {
		b.buf = append(b.buf, encodeText(b.cp, line)...)
		b.buf = append(b.buf, '\n')
	}
	return b
}

// TwoColumns writes left- and right-aligned text on one line, padding with
// spaces. Overflow wraps the left column and keeps the right column on the
// final line.
func (b *Builder) TwoColumns(left, right string) *Builder {
	rw := utf8.RuneCountInString(right)
	avail := b.width - rw - 1
	if avail < 4 {
		b.Text(left)
		b.Align(AlignRight).Text(right).Align(AlignLeft)
		return b
	}
	lines := wrap(left, avail)
	for i, line := range lines {
		if i == len(lines)-1 {
			pad := b.width - utf8.RuneCountInString(line) - rw
			b.Text(line + strings.Repeat(" ", pad) + right)
		} else {
			b.Text(line)
		}
	}
	return b
}

// Separator writes a full-width dashed line.
func (b *Builder) Separator() *Builder { return b.Text(strings.Repeat("-", b.width)) }

// Feed advances n lines.
func (b *Builder) Feed(n byte) *Builder { return b.raw(0x1B, 'd', n) }

// Cut feeds and performs a partial cut. Printers without a cutter ignore it.
func (b *Builder) Cut() *Builder { return b.raw(0x1D, 'V', 66, 3) } // GS V B n

// DrawerPulse fires the cash-drawer kick connector (pin 2).
func (b *Builder) DrawerPulse() *Builder { return b.raw(0x1B, 'p', 0, 60, 120) }

// Raw appends arbitrary bytes (internal use only; never exposed via the API).
func (b *Builder) Raw(data []byte) *Builder {
	b.buf = append(b.buf, data...)
	return b
}

func (b *Builder) raw(bytes ...byte) *Builder {
	b.buf = append(b.buf, bytes...)
	return b
}

// Bytes returns the accumulated document.
func (b *Builder) Bytes() []byte { return b.buf }

// wrap splits s into lines no longer than width runes, breaking on spaces
// where possible. Explicit newlines in s are honored.
func wrap(s string, width int) []string {
	if width <= 0 {
		width = 32
	}
	var out []string
	for para := range strings.SplitSeq(s, "\n") {
		runes := []rune(para)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		for len(runes) > 0 {
			if len(runes) <= width {
				out = append(out, string(runes))
				break
			}
			cut := width
			for i := width; i > 0; i-- {
				if runes[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, strings.TrimRight(string(runes[:cut]), " "))
			runes = runes[cut:]
			for len(runes) > 0 && runes[0] == ' ' {
				runes = runes[1:]
			}
		}
	}
	return out
}
