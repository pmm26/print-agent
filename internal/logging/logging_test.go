package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appLogging "print-agent/internal/logging"
	"print-agent/internal/storage"
)

func testStore(t *testing.T) *appLogging.Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return appLogging.NewStore(db)
}

func TestHandlerPersistsSameSanitizedStructuredRecord(t *testing.T) {
	store := testStore(t)
	var sink bytes.Buffer
	logger := slog.New(appLogging.NewHandler(slog.NewJSONHandler(&sink,
		&slog.HandlerOptions{Level: slog.LevelDebug}), store))
	logger.Error("request failed with Bearer top-secret", "printer", "kitchen", "printRun", "run_1",
		"authorization", "Bearer top-secret", "headers", map[string]any{
			"Authorization": "Bearer nested-secret", "method": "POST",
		})

	if output := sink.String(); strings.Contains(output, "top-secret") || strings.Contains(output, "nested-secret") {
		t.Fatalf("configured sink leaked a secret: %s", output)
	}
	records, err := store.Query(appLogging.Filter{Now: time.Now().UTC(), Limit: 10})
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	record := records[0]
	if record.Level != "error" || record.PrinterID != "kitchen" || record.RunUID != "run_1" {
		t.Fatalf("record = %#v", record)
	}
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "top-secret") || strings.Contains(string(encoded), "nested-secret") {
		t.Fatalf("stored record leaked a secret: %s", encoded)
	}
}

func TestHandlerStillWritesSinkWhenStoreFails(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := appLogging.NewStore(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	logger := slog.New(appLogging.NewHandler(slog.NewJSONHandler(&sink,
		&slog.HandlerOptions{Level: slog.LevelDebug}), store))
	logger.Warn("store unavailable", "error", "database closed")
	if !strings.Contains(sink.String(), "store unavailable") {
		t.Fatalf("configured sink did not receive record: %s", sink.String())
	}
}

func TestHandlerPreservesAttributeGroupAtBindingTime(t *testing.T) {
	store := testStore(t)
	var sink bytes.Buffer
	logger := slog.New(appLogging.NewHandler(slog.NewJSONHandler(&sink, nil), store))
	logger.WithGroup("request").With("method", "POST").WithGroup("response").Info("done", "status", 200)
	records, err := store.Query(appLogging.Filter{Now: time.Now().UTC(), Limit: 10})
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	attrs := records[0].Attributes
	if attrs["request.method"] != "POST" || attrs["request.response.status"] != float64(200) {
		t.Fatalf("attributes = %#v", attrs)
	}
}

func TestStoreFiltersPaginatesAndEnforcesRetentionAtReadTime(t *testing.T) {
	store := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	records := []appLogging.Record{
		{CreatedAt: now.Add(-time.Minute), Level: "error", Message: "kitchen timeout", PrinterID: "kitchen", RunUID: "run_3", Attributes: map[string]any{"error": "timeout"}},
		{CreatedAt: now.Add(-2 * time.Minute), Level: "warn", Message: "link slow", PrinterID: "kitchen", RunUID: "run_2", Attributes: map[string]any{}},
		{CreatedAt: now.Add(-3 * time.Minute), Level: "debug", Message: "HTTP request", PrinterID: "bar", Attributes: map[string]any{"method": "GET"}},
		{CreatedAt: now.Add(-appLogging.Retention - time.Minute), Level: "error", Message: "expired secret", Attributes: map[string]any{}},
	}
	for _, record := range records {
		if err := store.Insert(record); err != nil {
			t.Fatal(err)
		}
	}
	filtered, err := store.Query(appLogging.Filter{Now: now, Levels: []string{"warn", "error"}, PrinterID: "kitchen", Query: "link", Limit: 10})
	if err != nil || len(filtered) != 1 || filtered[0].Message != "link slow" {
		t.Fatalf("filtered = %#v, %v", filtered, err)
	}
	page, err := store.Query(appLogging.Filter{Now: now, Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("page = %#v, %v", page, err)
	}
	older, err := store.Query(appLogging.Filter{Now: now, Before: &page[1].CreatedAt, BeforeID: page[1].ID, Limit: 2})
	if err != nil || len(older) != 1 || older[0].Message != "HTTP request" {
		t.Fatalf("older = %#v, %v", older, err)
	}
	if err := store.Purge(now); err != nil {
		t.Fatal(err)
	}
	all, err := store.Query(appLogging.Filter{Now: now, Limit: 10})
	if err != nil || len(all) != 3 {
		t.Fatalf("retained = %#v, %v", all, err)
	}
}

func TestRollingFileRemovesRecordsOutsideTwoHours(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "print-agent.log")
	oldLine, _ := json.Marshal(map[string]any{"time": now.Add(-appLogging.Retention - time.Minute), "msg": "old"})
	newLine, _ := json.Marshal(map[string]any{"time": now.Add(-time.Minute), "msg": "new"})
	if err := os.WriteFile(path, append(append(oldLine, '\n'), append(newLine, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyBackup := path + ".1"
	if err := os.WriteFile(legacyBackup, oldLine, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := appLogging.NewRollingFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Purge(now); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "old") || !strings.Contains(string(contents), "new") {
		t.Fatalf("rolling contents = %s", contents)
	}
	if _, err := os.Stat(legacyBackup); !os.IsNotExist(err) {
		t.Fatalf("legacy backup was not removed: %v", err)
	}
}
