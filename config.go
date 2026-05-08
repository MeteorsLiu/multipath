package main

import (
	"encoding/json"
	"os"
	"time"
)

const (
	defaultTunMTU        = 1440
	defaultPromListen    = "127.0.0.1:0"
	defaultProbeInterval = 200 * time.Millisecond
	defaultProbeTimeout  = 600 * time.Millisecond
)

type PathConfig struct {
	RemoteAddr string `json:"remoteAddr"`
	Weight     int    `json:"weight"`
}

type ClientConfig struct {
	RemotePaths []PathConfig `json:"remotePaths"`
}

type ServerConfig struct {
	ListenAddr string `json:"listen"`
}

type TunConfig struct {
	Name       string   `json:"name,omitempty"`
	LocalAddr  string   `json:"localAddr"`
	RemoteAddr string   `json:"remoteAddr"`
	AllowedIPs []string `json:"allowedIPs"`
	MTU        int      `json:"mtu"`
}

type Config struct {
	Client               ClientConfig `json:"client,omitempty"`
	Server               ServerConfig `json:"server,omitempty"`
	Tun                  TunConfig    `json:"tun"`
	PromListenAddr       string       `json:"promListenAddr"`
	IsServerSide         bool         `json:"isServer"`
	IsTCP                bool         `json:"tcp"`
	FEC                  bool         `json:"fec"`
	FECFlushAlpha        uint32       `json:"fecFlushAlpha,omitempty"`
	FECFlushMinMs        uint32       `json:"fecFlushMinMs,omitempty"`
	FECFlushMaxMs        uint32       `json:"fecFlushMaxMs,omitempty"`
	FECFlushColdStartMs  uint32       `json:"fecFlushColdStartMs,omitempty"`
	FECFlushFixedMs      uint32       `json:"fecFlushFixedMs,omitempty"`
	ProbeIntervalMS      int          `json:"probeIntervalMS"`
	ProbeTimeoutMS       int          `json:"probeTimeoutMS"`
	BandwidthProbeCapBps int64        `json:"bandwidthProbeCapBps"`
}

func ParseConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()

	cfg := Config{FEC: true}
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return Config{}, err
	}
	cfg.setDefaults()
	return cfg, nil
}

func (c *Config) setDefaults() {
	if c.Tun.MTU == 0 {
		c.Tun.MTU = defaultTunMTU
	}
	if c.PromListenAddr == "" {
		c.PromListenAddr = defaultPromListen
	}
	if c.ProbeIntervalMS == 0 {
		c.ProbeIntervalMS = int(defaultProbeInterval / time.Millisecond)
	}
	if c.ProbeTimeoutMS == 0 {
		c.ProbeTimeoutMS = int(defaultProbeTimeout / time.Millisecond)
	}
	if c.BandwidthProbeCapBps == 0 {
		c.BandwidthProbeCapBps = 200_000_000
	}
	for i := range c.Client.RemotePaths {
		if c.Client.RemotePaths[i].Weight <= 0 {
			c.Client.RemotePaths[i].Weight = 1
		}
	}
}

func (c Config) probeInterval() time.Duration {
	return time.Duration(c.ProbeIntervalMS) * time.Millisecond
}

func (c Config) probeTimeout() time.Duration {
	return time.Duration(c.ProbeTimeoutMS) * time.Millisecond
}

func (c Config) bandwidthProbeCapForSend() uint64 {
	if c.BandwidthProbeCapBps <= 0 {
		return 0
	}
	return uint64(c.BandwidthProbeCapBps)
}
