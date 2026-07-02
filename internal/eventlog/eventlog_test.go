package eventlog

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintfDoesNotFilterEvents(t *testing.T) {
	var out bytes.Buffer
	origPrefix := logger.Prefix()
	origFlags := logger.Flags()
	logger.SetOutput(&out)
	logger.SetPrefix("")
	logger.SetFlags(0)
	t.Cleanup(func() {
		logger.SetOutput(os.Stderr)
		logger.SetPrefix(origPrefix)
		logger.SetFlags(origFlags)
	})

	for _, event := range []string{"qos_state", "link_status", "bw", "bandwidth_probe_decision", "selector", "ping", "reconnect"} {
		Printf(event, "marker=%s", event)
		want := event + " marker=" + event
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Printf(%q) output missing %q:\n%s", event, want, out.String())
		}
	}
}
