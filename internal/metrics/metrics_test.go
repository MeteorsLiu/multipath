package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestRegistryWritesPrometheusText(t *testing.T) {
	registry := NewRegistry()
	registry.AddCounter(TUNPacketsTotal, 2, L("direction", "read"))
	registry.AddCounter(TUNPacketsTotal, 3, L("direction", "read"))
	registry.AddCounter(FECFlushTotal, 1, L("session", 99), L("source_span", 2))
	registry.SetGauge(LaneRTTMs, 42, L("session", 99), L("lane", 3), L("leg", "udp"))
	registry.SetGauge(RuntimeInfo, 1, L("role", "client"), L("fec", true), L("paths", 2))

	got := gatherText(t, registry)
	for _, want := range []string{
		"# TYPE multipath_tun_packets_total counter\n",
		"multipath_tun_packets_total{direction=\"read\"} 5\n",
		"# TYPE multipath_runtime_info gauge\n",
		"multipath_fec_flush_total{session=\"99\",source_span=\"2\"} 1\n",
		"multipath_lane_rtt_ms{lane=\"3\",leg=\"udp\",session=\"99\"} 42\n",
		"multipath_runtime_info{fec=\"true\",paths=\"2\",role=\"client\"} 1\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, got)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	registry := NewRegistry()
	registry.AddCounter(ProtocolFramesTotal, 1,
		L("direction", "rx"),
		L("type", "a\"b\\c\n"),
		L("session", 1),
		L("lane", 2),
		L("leg", "udp"),
	)

	got := gatherText(t, registry)
	if !strings.Contains(got, `type="a\"b\\c\n"`) {
		t.Fatalf("label was not escaped correctly:\n%s", got)
	}
}

func gatherText(t *testing.T, registry *Registry) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(registry.Gatherer(), promhttp.HandlerOpts{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	return recorder.Body.String()
}
