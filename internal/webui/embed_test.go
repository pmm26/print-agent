package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardRoutesServeReactIndex(t *testing.T) {
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
			body := recorder.Body.String()
			if !strings.Contains(body, `<div id="root"></div>`) || !strings.Contains(body, `/admin/assets/`) {
				t.Fatal("compiled React index was not served")
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

func TestDashboardAssetsAreEmbeddedAndImmutable(t *testing.T) {
	assets, err := fs.ReadDir(webFiles, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) == 0 {
		t.Fatal("compiled dashboard contains no assets")
	}

	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/"+assets[0].Name(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("asset response = %d", recorder.Code)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", cache)
	}
}

func TestDashboardIndexRedirectIsNotCached(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if recorder.Code != http.StatusMovedPermanently || recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index response = %d, Cache-Control %q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}

func TestDashboardUnknownPathsReturnNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/not-a-dashboard-route", nil))
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
