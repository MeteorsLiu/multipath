package main

import (
	"context"
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewClient(ctx context.Context, cfg Config) (func(), error) {
	if len(cfg.Client.Remotes) == 0 {
		return nil, fmt.Errorf("failed to init client: no remote paths found")
	}

	sche := cfs.NewCFSScheduler(false)

	tunInterface, err := tun.CreateTUN(cfg.TunName, conn.MTUSize)
	if err != nil {
		return nil, err
	}

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	for _, path := range cfg.Client.Remotes {
		tcp.DialConn(ctx, path.RemoteAddr, tunModule.In())
	}

	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}
