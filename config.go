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

type Tun struct {
	Name       string `json:"name,omitempty"`
	LocalAddr  string `json:"localAddr"`
	RemoteAddr string `json:"remoteAddr"`
}

type Config struct {
	Client       `json:"client,omitempty"`
	Server       `json:"server,omitempty"`
	Tun          `json:"tun"`
	IsServerSide bool `json:"isServer"`
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
	if cfg.Tun.Name == "" {
		cfg.Tun.Name = "multipath-veth0"
	}
	return
}
