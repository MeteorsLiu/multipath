package main

import (
	"context"
	"fmt"
	"net"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewServer(ctx context.Context, cfg Config) (closeFn func(), err error) {
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("failed to init server: no listen addr")
	}

	sche := cfs.NewCFSScheduler(true)

	tunInterface, err := tun.CreateTUN(cfg.Tun.Name, conn.MTUSize)
	if err != nil {
		return nil, err
	}
	execCommand("ip", "a", "add", cfg.LocalAddr, "peer", cfg.RemoteAddr, "dev", cfg.Tun.Name)
	execCommand("ip", "l", "set", cfg.Tun.Name, "up")

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	l, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fmt.Println(err)
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				fmt.Println("listen exits: ", err)
				break
			}
			tc := tcp.NewConn(ctx, c, tunModule.In())
			sche.AddPath(cfs.NewPath(path.NewPath(tc)))
		}
	}()

	tunModule.Start()

	return func() {
		l.Close()
		tunInterface.Close()
	}, nil
}

// 2018
