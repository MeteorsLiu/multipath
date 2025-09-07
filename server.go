package main

import (
	"context"
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewServer(ctx context.Context, cfg Config) (closeFn func(), err error) {
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("failed to init server: no listen addr")
	}

	sche := cfs.NewCFSScheduler(true)

	tunInterface, err := tun.CreateTUN(cfg.TunName, conn.MTUSize)
	if err != nil {
		return nil, err
	}

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	tcp.ListenConn(ctx, nil, cfg.ListenAddr, tunModule.In())

	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}

// 2018
