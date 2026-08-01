// btprobe is the Phase 0 hardware validation harness. It exercises paired
// Bluetooth ESC/POS printers directly, before any agent code is trusted:
//
//	btprobe list                     enumerate candidate serial endpoints
//	btprobe test <endpoint>          print a formatted test page
//	btprobe charset <endpoint>       print the Spanish charset under each code page
//	btprobe multi <ep1> <ep2> ...    concurrent repeated prints on several printers
//	btprobe status <endpoint>        probe DLE EOT real-time status support
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"

	"print-agent/internal/bluetooth"
	"print-agent/internal/config"
	"print-agent/internal/escpos"
	"print-agent/internal/transport"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "list":
		err = cmdList(ctx)
	case "test":
		err = cmdTest(ctx, args)
	case "charset":
		err = cmdCharset(ctx, args)
	case "multi":
		err = cmdMulti(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: btprobe <command>

  list                       enumerate candidate serial endpoints
  test <endpoint>            print a test page (-encoding CP858, -width 32)
  charset <endpoint>         print Spanish chars under CP437/CP850/CP858/CP1252
  multi <ep1> <ep2> [...]    concurrent prints (-rounds 5)
  status <endpoint>          probe DLE EOT real-time status support`)
}

func printerConfig(endpoint, encoding string, width int) config.PrinterConfig {
	cfg := config.PrinterConfig{
		ID:                "probe",
		Endpoint:          endpoint,
		Encoding:          encoding,
		CharactersPerLine: width,
		Transport:         config.TransportBluetoothSerial,
	}
	cfg.ApplyDefaults()
	return cfg
}

func cmdList(ctx context.Context) error {
	conn := bluetooth.NewPlatformConnector()
	cands, err := conn.ListCandidates(ctx)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		fmt.Println("no candidate endpoints found — pair the printer in Bluetooth settings first")
		return nil
	}
	for _, c := range cands {
		state := "unknown"
		if c.DeviceName != "" {
			if c.Connected {
				state = "connected"
			} else {
				state = "paired"
			}
		}
		kind := ""
		if c.IsPrinter {
			kind = "PRINTER"
		}
		fmt.Printf("%-40s  %-10s  %-20s  %-18s %s\n", c.Endpoint, state, c.DeviceName, c.DeviceAddress, kind)
	}
	return nil
}

func cmdTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	encoding := fs.String("encoding", "CP858", "code page")
	width := fs.Int("width", 32, "characters per line")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: btprobe test <endpoint>")
	}
	endpoint := fs.Arg(0)

	cfg := printerConfig(endpoint, *encoding, *width)
	doc, err := escpos.NewRenderer().Render(escpos.TemplateTestPage, nil, cfg, escpos.RenderOptions{})
	if err != nil {
		return err
	}
	return writeOnce(ctx, cfg, doc)
}

func cmdCharset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("charset", flag.ExitOnError)
	width := fs.Int("width", 32, "characters per line")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: btprobe charset <endpoint>")
	}
	endpoint := fs.Arg(0)

	const sample = "á é í ó ú ñ Ñ ¿ ¡ € ü Ü ç"
	var doc []byte
	for _, enc := range escpos.SupportedEncodings() {
		b, err := escpos.NewBuilder(enc, *width)
		if err != nil {
			return err
		}
		b.Init().Bold(true).Text("== " + enc + " ==").Bold(false).Text(sample).Feed(1)
		doc = append(doc, b.Bytes()...)
	}
	b, _ := escpos.NewBuilder("CP437", *width)
	b.Text("pick the block that prints").Text("all characters correctly").Feed(4).Cut()
	doc = append(doc, b.Bytes()...)

	cfg := printerConfig(endpoint, "CP437", *width)
	return writeOnce(ctx, cfg, doc)
}

func cmdMulti(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("multi", flag.ExitOnError)
	rounds := fs.Int("rounds", 5, "prints per printer")
	encoding := fs.String("encoding", "CP858", "code page")
	width := fs.Int("width", 32, "characters per line")
	fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: btprobe multi <endpoint1> <endpoint2> [...]")
	}

	var wg sync.WaitGroup
	results := make([][]string, fs.NArg())
	for i, endpoint := range fs.Args() {
		wg.Go(func() {
			cfg := printerConfig(endpoint, *encoding, *width)
			t := transport.NewSerial(cfg)
			if err := t.Connect(ctx); err != nil {
				results[i] = append(results[i], fmt.Sprintf("connect: FAIL %v", err))
				return
			}
			defer t.Close()
			for r := 1; r <= *rounds; r++ {
				payload := fmt.Sprintf(`{"printerId":"printer %d","line":"round %d of %d at %s"}`,
					i+1, r, *rounds, time.Now().Format("15:04:05"))
				doc, err := escpos.NewRenderer().Render(escpos.TemplateTestPage,
					[]byte(payload), cfg, escpos.RenderOptions{})
				if err != nil {
					results[i] = append(results[i], fmt.Sprintf("round %d render: FAIL %v", r, err))
					return
				}
				start := time.Now()
				if err := t.Write(ctx, doc); err != nil {
					results[i] = append(results[i], fmt.Sprintf("round %d: FAIL %v", r, err))
					return
				}
				results[i] = append(results[i], fmt.Sprintf("round %d: ok (%d bytes, %s)",
					r, len(doc), time.Since(start).Round(time.Millisecond)))
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return
				}
			}
		})
	}
	wg.Wait()
	for i, endpoint := range fs.Args() {
		fmt.Printf("\n%s\n%s\n", endpoint, strings.Repeat("-", len(endpoint)))
		for _, line := range results[i] {
			fmt.Println(" ", line)
		}
	}
	return nil
}

// cmdStatus probes DLE EOT n (real-time status) for n=1..4 and reports
// whether the printer answers. Uses the serial library directly because the
// agent transport is deliberately write-only.
func cmdStatus(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: btprobe status <endpoint>")
	}
	port, err := serial.Open(fs.Arg(0), &serial.Mode{BaudRate: 9600})
	if err != nil {
		return err
	}
	defer port.Close()
	port.SetReadTimeout(2 * time.Second)

	names := map[byte]string{1: "printer status", 2: "offline status", 3: "error status", 4: "paper status"}
	supported := false
	for n := byte(1); n <= 4; n++ {
		if _, err := port.Write([]byte{0x10, 0x04, n}); err != nil {
			return fmt.Errorf("write DLE EOT %d: %w", n, err)
		}
		buf := make([]byte, 8)
		r, _ := port.Read(buf)
		if r > 0 {
			supported = true
			fmt.Printf("DLE EOT %d (%s): response %#x\n", n, names[n], buf[:r])
		} else {
			fmt.Printf("DLE EOT %d (%s): no response\n", n, names[n])
		}
	}
	if supported {
		fmt.Println("\nreal-time status IS supported — statusProbeEnabled can be used for this model")
	} else {
		fmt.Println("\nno real-time status responses — rely on write errors for disconnect detection")
	}
	return nil
}

func writeOnce(ctx context.Context, cfg config.PrinterConfig, doc []byte) error {
	t := transport.NewSerial(cfg)
	fmt.Printf("connecting to %s...\n", cfg.Endpoint)
	start := time.Now()
	if err := t.Connect(ctx); err != nil {
		return err
	}
	defer t.Close()
	fmt.Printf("connected in %s, writing %d bytes...\n", time.Since(start).Round(time.Millisecond), len(doc))
	start = time.Now()
	if err := t.Write(ctx, doc); err != nil {
		return err
	}
	fmt.Printf("transmitted in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}
