package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	var cfgFile string
	flag.StringVar(&cfgFile, "config", "", "Config File")
	flag.Parse()

	cfg, err := ParseConfig(cfgFile)
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.IsServerSide {
		NewServer(ctx, cfg)
	} else {
		NewClient(ctx, cfg)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	<-sigCh
}
