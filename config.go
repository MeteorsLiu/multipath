package main

import (
	"encoding/json"
	"os"
)

type Path struct {
	RemoteAddr string `json:"remoteAddr"`
	Weight     int    `json:"weight"`
}

type Config struct {
	ListenAddr []string `json:"listen"`

	Remotes []Path `json:"remotePaths"`

	TunName string `json:"tun,omitempty"`
}

func ParseConfig(configFile string) (cfg Config, err error) {
	file, err := os.Open(configFile)
	if err != nil {
		return
	}
	defer file.Close()

	err = json.NewDecoder(file).Decode(&cfg)
	if err != nil {
		return
	}
	if cfg.TunName == "" {
		cfg.TunName = "multipath-veth0"
	}
	return
}
