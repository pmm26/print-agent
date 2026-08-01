package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardRoutesServeIndex(t *testing.T) {
	handler := Handler()
	for _, path := range []string{
		"/setup/pair", "/setup/printers", "/setup/pos",
		"/operations/jobs", "/operations/queue", "/operations/printer-logs",
		"/system/diagnostics", "/system/logs",
		"/dev/pos-simulator",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), `<nav id="dashboard-nav"`) {
				t.Fatal("dashboard index was not served")
			}
			if cache := recorder.Header().Get("Cache-Control"); cache != "no-cache" {
				t.Fatalf("Cache-Control = %q", cache)
			}
		})
	}
}

func TestLoggingRoutesAndURLStateWiring(t *testing.T) {
	index, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`href="/admin/operations/printer-logs"`, `href="/admin/system/logs"`,
		`id="system-log-filters"`, `id="printer-log-filters"`,
		`<select id="printer-log-printer"><option value="">All printers</option></select>`,
	} {
		if !strings.Contains(string(index), expected) {
			t.Fatalf("index missing %s", expected)
		}
	}
	for _, expected := range []string{
		`new URLSearchParams(window.location.search)`, `window.addEventListener("popstate"`,
		`new URLSearchParams({ printerId: id })`,
	} {
		if !strings.Contains(string(app), expected) {
			t.Fatalf("app missing URL-state wiring %s", expected)
		}
	}
}

func TestPrinterDetailModalWiring(t *testing.T) {
	index, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`id="printer-detail-dialog"`, `data-printer-detail-tab="overview"`,
		`data-printer-detail-tab="queue"`, `data-printer-detail-tab="logs"`,
		`id="btn-printer-detail-test"`, `id="btn-printer-detail-reconnect"`,
		`id="btn-printer-detail-configure"`, `id="btn-printer-detail-toggle"`,
		`id="btn-printer-detail-remove"`,
	} {
		if !strings.Contains(string(index), expected) {
			t.Fatalf("printer detail modal missing %s", expected)
		}
	}
	for _, expected := range []string{
		`function syncPrinterDetailFromURL()`, `function refreshPrinterDetailQueue()`,
		`function refreshPrinterDetailLogs()`, `function fetchPrinterLogEvents(`,
		`function printerLogRowsHTML(`, `/admin/operations/queue?`,
		`/admin/operations/printer-logs?`,
	} {
		if !strings.Contains(string(app), expected) {
			t.Fatalf("printer detail behavior missing %s", expected)
		}
	}
}

func TestLinuxBluetoothManagementWiring(t *testing.T) {
	index, _ := webFiles.ReadFile("web/index.html")
	app, _ := webFiles.ReadFile("web/app.js")
	for _, expected := range []string{`id="scan-connection-type"`, `id="ubuntu-stop-command"`,
		`pkill -f '^/usr/bin/gnome-control-center bluetooth$'`, `id="f-connection-preference"`} {
		if !strings.Contains(string(index), expected) {
			t.Fatalf("index missing %s", expected)
		}
	}
	for _, expected := range []string{`data-device-act="disconnect"`, `data-device-act="forget"`,
		`connectionPreference:`, `function activeConnectionType(`, `selectedDeviceProtocols`, `enteredDevicePINs`} {
		if !strings.Contains(string(app), expected) {
			t.Fatalf("app missing %s", expected)
		}
	}
}

func TestJobModalRefreshPreservesDialogState(t *testing.T) {
	app, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`const sameJob = activeJobDetail?.uid === job.uid`,
		`jobRunSignatures.get(body) !== signature`,
		`await openJob(jobUID, { show: false })`,
		`jobDialogRequest++`,
	} {
		if !strings.Contains(string(app), expected) {
			t.Fatalf("job modal refresh missing %s", expected)
		}
	}
}

func TestDashboardRootRedirectsToJobs(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/admin/operations/jobs" {
		t.Fatalf("redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestDashboardAssetsAndUnknownPaths(t *testing.T) {
	handler := Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "dashboardRoutes") {
		t.Fatalf("app.js response = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/not-a-dashboard-route", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", recorder.Code)
	}
}

func TestDashboardRoutesAllowOnlyReads(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/operations/jobs", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("response = %d, Allow %q", recorder.Code, recorder.Header().Get("Allow"))
	}
}
