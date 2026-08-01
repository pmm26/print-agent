// print-agent is the local Bluetooth ESC/POS print agent. It exposes a
// loopback HTTP API for the hosted POS and an embedded management dashboard
// at /admin.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"print-agent/internal/app"
)

func main() {
	port := flag.Int("port", 17432, "loopback port to listen on")
	dataDir := flag.String("data-dir", "", "data directory (default: OS config dir /print-agent)")
	console := flag.Bool("console", true, "log to stderr in addition to the log file")
	retention := flag.Int("retention-days", 30, "days to keep finished jobs and events")
	flag.Parse()

	svc, err := app.New(app.Options{
		DataDir:       *dataDir,
		Port:          *port,
		Console:       *console,
		RetentionDays: *retention,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := svc.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
