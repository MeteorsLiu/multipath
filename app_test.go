package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/tun"
)

func TestParseConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"client": {"remotePaths": [{"remoteAddr": "127.0.0.1:9000"}]},
		"tun": {"localAddr": "10.0.0.1", "remoteAddr": "10.0.0.2"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg.Tun.MTU != defaultTunMTU {
		t.Fatalf("MTU = %d, want %d", cfg.Tun.MTU, defaultTunMTU)
	}
	if cfg.Tun.Name != "" {
		t.Fatalf("tun name = %q, want empty for OS auto-selection", cfg.Tun.Name)
	}
	if cfg.PromListenAddr != defaultPromListen {
		t.Fatalf("prom listen = %q, want %q", cfg.PromListenAddr, defaultPromListen)
	}
	if cfg.ProbeIntervalMS != 200 || cfg.ProbeTimeoutMS != 1000 {
		t.Fatalf("probe defaults = %d/%d, want 200/1000", cfg.ProbeIntervalMS, cfg.ProbeTimeoutMS)
	}
	if cfg.Client.RemotePaths[0].Weight != 1 {
		t.Fatalf("weight = %d, want 1", cfg.Client.RemotePaths[0].Weight)
	}
	if !cfg.FEC {
		t.Fatal("FEC = false, want true")
	}
}

func TestParseConfigExplicitFECFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"client": {"remotePaths": [{"remoteAddr": "127.0.0.1:9000"}]},
		"fec": false
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg.FEC {
		t.Fatal("FEC = true, want explicit false")
	}
}

func TestBandwidthProbeCapForSendTreatsNegativeAsNoCap(t *testing.T) {
	cfg := Config{BandwidthProbeCapBps: -1}
	if got := cfg.bandwidthProbeCapForSend(); got != 0 {
		t.Fatalf("bandwidthProbeCapForSend = %d, want 0", got)
	}
}

func TestParseConfigNegativeBandwidthProbeCapDisablesCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"client": {"remotePaths": [{"remoteAddr": "127.0.0.1:9000"}]},
		"bandwidthProbeCapBps": -1
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg.BandwidthProbeCapBps != -1 {
		t.Fatalf("BandwidthProbeCapBps = %d, want -1", cfg.BandwidthProbeCapBps)
	}
	if got := cfg.bandwidthProbeCapForSend(); got != 0 {
		t.Fatalf("bandwidthProbeCapForSend = %d, want 0", got)
	}
}

func TestBandwidthProbeEnvToggle(t *testing.T) {
	t.Setenv("MULTIPATH_DISABLE_BW_PROBE", "1")
	if bandwidthProbeEnabled() {
		t.Fatal("bandwidthProbeEnabled = true with MULTIPATH_DISABLE_BW_PROBE=1")
	}

	t.Setenv("MULTIPATH_DISABLE_BW_PROBE", "0")
	if !bandwidthProbeEnabled() {
		t.Fatal("bandwidthProbeEnabled = false with MULTIPATH_DISABLE_BW_PROBE=0")
	}
}

func TestParseOldConfigShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"promListenAddr": "127.0.0.1:2131",
		"client": {
			"remotePaths": [
				{"remoteAddr": "127.0.0.1:9000", "weight": -1}
			]
		},
		"tun": {
			"name": "mp0",
			"localAddr": "10.0.0.1",
			"remoteAddr": "10.0.0.2",
			"allowedIPs": ["10.0.0.2/32"]
		},
		"tcp": true
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg.PromListenAddr != "127.0.0.1:2131" {
		t.Fatalf("prom listen = %q", cfg.PromListenAddr)
	}
	if cfg.Tun.Name != "mp0" {
		t.Fatalf("tun name = %q, want mp0", cfg.Tun.Name)
	}
	if cfg.Client.RemotePaths[0].Weight != 1 {
		t.Fatalf("weight = %d, want normalized 1", cfg.Client.RemotePaths[0].Weight)
	}
	if !cfg.IsTCP {
		t.Fatal("tcp = false, want true")
	}
	if !cfg.FEC {
		t.Fatal("FEC = false, want default true")
	}
}

func TestParseOldMinimalConfigs(t *testing.T) {
	for name, content := range map[string]string{
		"client": `{
			"client": {
				"remotePaths": [
					{"remoteAddr": "192.168.167.155:29999", "weight": 1}
				]
			}
		}`,
		"server": `{
			"server": {"listen": "0.0.0.0:29999"},
			"isServer": true
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cfg, err := ParseConfig(path)
			if err != nil {
				t.Fatalf("ParseConfig failed: %v", err)
			}
			if cfg.Tun.Name != "" {
				t.Fatalf("tun name = %q, want empty for OS auto-selection", cfg.Tun.Name)
			}
			if !cfg.FEC {
				t.Fatal("FEC = false, want default true")
			}
		})
	}
}

func TestBuildClientRuntime(t *testing.T) {
	cfg := Config{
		Client: ClientConfig{
			RemotePaths: []PathConfig{
				{RemoteAddr: "127.0.0.1:9000", Weight: 2},
			},
		},
	}
	cfg.setDefaults()

	device := tun.NewDevice(&appMemoryTun{}, cfg.Tun.MTU)
	runtime, closers, err := buildClientRuntime(cfg, device)
	if err != nil {
		t.Fatalf("buildClientRuntime failed: %v", err)
	}
	defer closeAll(closers)
	if runtime == nil {
		t.Fatal("runtime is nil")
	}
}

func TestBuildClientRuntimeIgnoresLegacyTCPFlag(t *testing.T) {
	cfg := Config{
		IsTCP: true,
		Client: ClientConfig{
			RemotePaths: []PathConfig{
				{RemoteAddr: "127.0.0.1:9000", Weight: 1},
			},
		},
	}
	cfg.setDefaults()

	device := tun.NewDevice(&appMemoryTun{}, cfg.Tun.MTU)
	runtime, closers, err := buildClientRuntime(cfg, device)
	if err != nil {
		t.Fatalf("buildClientRuntime failed: %v", err)
	}
	defer closeAll(closers)

	if runtime.packetTransport == nil {
		t.Fatal("packetTransport is nil when legacy tcp flag is set")
	}
	if runtime.streamTransport == nil {
		t.Fatal("streamTransport is nil when legacy tcp flag is set")
	}
}

func TestBuildRuntimeMetricsServer(t *testing.T) {
	cfg := Config{
		Client: ClientConfig{
			RemotePaths: []PathConfig{
				{RemoteAddr: "127.0.0.1:9000", Weight: 1},
			},
		},
		PromListenAddr: "127.0.0.1:0",
	}
	cfg.setDefaults()

	device := tun.NewDevice(&appMemoryTun{}, cfg.Tun.MTU)
	runtime, closers, err := buildClientRuntime(cfg, device)
	if err != nil {
		t.Fatalf("buildClientRuntime failed: %v", err)
	}
	defer closeAll(closers)
	if runtime.metricsServer == nil {
		t.Fatal("metricsServer is nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runtime.metricsServer.Run(ctx)
	}()

	resp, err := http.Get("http://" + runtime.metricsServer.Addr() + "/metrics")
	if err != nil {
		cancel()
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		cancel()
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(body), "multipath_runtime_info") {
		cancel()
		t.Fatalf("metrics missing runtime info:\n%s", body)
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("metrics Run err = %v, want context.Canceled", err)
	}
}

type appMemoryTun struct{}

func (a *appMemoryTun) Read(p []byte) (int, error) {
	return 0, os.ErrClosed
}

func (a *appMemoryTun) Write(p []byte) (int, error) {
	return len(p), nil
}

func (a *appMemoryTun) Close() error {
	return nil
}
