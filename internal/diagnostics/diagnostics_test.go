package diagnostics

import (
	"path/filepath"
	"testing"
	"time"

	"print-agent/internal/storage"
)

func TestPrinterEventsFilterPaginationAndRetention(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(db, "events.db")
	now := time.Now().UTC().Truncate(time.Second)
	for _, event := range []struct {
		typ, printer, run, message string
		at                         time.Time
	}{
		{"print_run.failed", "kitchen", "run_3", "write timeout", now.Add(-time.Minute)},
		{"printer.disconnected", "kitchen", "", "link lost", now.Add(-2 * time.Minute)},
		{"print_run.transmitted", "bar", "run_1", "100 bytes", now.Add(-3 * time.Minute)},
		{"job.accepted", "", "", "not a printer event", now.Add(-4 * time.Minute)},
		{"printer.error", "kitchen", "", "expired", now.Add(-EventRetention - time.Minute)},
	} {
		if err := service.PersistEvent(event.typ, event.printer, event.run, event.message, event.at); err != nil {
			t.Fatal(err)
		}
	}
	filtered, err := service.PrinterEvents(EventFilter{Now: now, PrinterID: "kitchen", EventType: "print_run.failed", Query: "timeout", Limit: 10})
	if err != nil || len(filtered) != 1 || filtered[0].RunUID != "run_3" {
		t.Fatalf("filtered = %#v, %v", filtered, err)
	}
	page, err := service.PrinterEvents(EventFilter{Now: now, Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %#v, %v", page, err)
	}
	older, err := service.PrinterEvents(EventFilter{Now: now, Before: &page[1].CreatedAt, BeforeID: page[1].ID, Limit: 2})
	if err != nil || len(older) != 1 || older[0].PrinterID != "bar" {
		t.Fatalf("older = %#v, %v", older, err)
	}
}
