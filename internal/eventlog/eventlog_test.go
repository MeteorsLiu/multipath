package eventlog

import "testing"

func TestShouldPrintOnlyOperatorStateEvents(t *testing.T) {
	for _, event := range []string{"selector", "ping", "reconnect"} {
		if !shouldPrint(event) {
			t.Fatalf("shouldPrint(%q) = false, want true", event)
		}
	}

	for _, event := range []string{"qos_state", "link_status", "bw", "bandwidth_probe_decision"} {
		if shouldPrint(event) {
			t.Fatalf("shouldPrint(%q) = true, want false", event)
		}
	}
}
