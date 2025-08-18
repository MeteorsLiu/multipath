package main

import (
	"encoding/json"
	"os"
)

type Path struct {
	RemoteAddr string `json:"remoteAddr"`
	Weight     int    `json:"weight"`
}

type Client struct {
	Remotes []Path `json:"remotePaths"`
}

type Server struct {
	ListenAddr string `json:"listen"`
}

type Config struct {
	Client       `json:"client,omitempty"`
	Server       `json:"server,omitempty"`
	TunName      string `json:"tun,omitempty"`
	IsServerSide bool   `json:"isServer"`
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
