package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewServerUsesDefaultServeMux(t *testing.T) {
	SetGauge(RuntimeInfo, 1, L("role", "test"), L("fec", true), L("paths", 1))

	server, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer server.Close()

	if server.server.Handler != http.DefaultServeMux {
		t.Fatalf("server handler = %p, want http.DefaultServeMux %p", server.server.Handler, http.DefaultServeMux)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), "multipath_") {
		t.Fatalf("metrics output missing multipath series:\n%s", recorder.Body.String())
	}
}
