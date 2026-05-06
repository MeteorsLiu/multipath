package recv

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/protocol"
)

func TestDebugSuppressesBandwidthProbeFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame protocol.Frame
		want  bool
	}{
		{
			name:  "bandwidth probe",
			frame: protocol.Frame{Type: protocol.TypeBandwidthProbe},
			want:  true,
		},
		{
			name:  "bandwidth probe ack",
			frame: protocol.Frame{Type: protocol.TypeBandwidthProbeAck},
			want:  true,
		},
		{
			name:  "ping",
			frame: protocol.Frame{Type: protocol.TypePING},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := debugSuppressFrame(tt.frame); got != tt.want {
				t.Fatalf("debugSuppressFrame() = %t, want %t", got, tt.want)
			}
		})
	}
}
