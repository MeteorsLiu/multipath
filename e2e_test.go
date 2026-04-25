package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestRealE2EScript(t *testing.T) {
	if os.Getenv("MULTIPATH_REAL_E2E") != "1" {
		t.Skip("set MULTIPATH_REAL_E2E=1 to run the Linux netns/TUN e2e test")
	}
	if runtime.GOOS != "linux" {
		t.Skip("real e2e requires Linux network namespaces")
	}

	cmd := exec.Command("bash", "scripts/e2e.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("real e2e failed: %v", err)
	}
}
