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
	flag.Parse()

	svc, err := app.New(app.Options{
		DataDir: *dataDir,
		Port:    *port,
		Console: *console,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		<-signals
		fmt.Fprintln(os.Stderr, "second signal received; forcing shutdown")
		os.Exit(2)
	}()
	if err := svc.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
