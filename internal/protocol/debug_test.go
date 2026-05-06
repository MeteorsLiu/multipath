package protocol

import "testing"

func TestDebugSuppressesBandwidthProbeFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
		want  bool
	}{
		{
			name:  "bandwidth probe",
			frame: Frame{Type: TypeBandwidthProbe},
			want:  true,
		},
		{
			name:  "bandwidth probe ack",
			frame: Frame{Type: TypeBandwidthProbeAck},
			want:  true,
		},
		{
			name:  "ping",
			frame: Frame{Type: TypePING},
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
