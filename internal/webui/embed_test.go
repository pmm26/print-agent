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
		"/operations/jobs", "/operations/queue",
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
