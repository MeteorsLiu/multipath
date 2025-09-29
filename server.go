package main

import (
	"context"
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewServer(ctx context.Context, cfg Config) (closeFn func(), err error) {
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("failed to init server: no listen addr")
	}
	prom.SetupServer(cfg.PromListenAddr)

	pathMap := newSchedulablePathManager()
	sche := cfs.NewCFSScheduler()

	manager := path.NewManager(path.WithOnNewPath(func(event path.ManagerEvent, p path.Path) {
		switch event {
		case path.Append:
			schePath, ok := pathMap.getOrSet(p.Remote(), func() scheduler.SchedulablePath {
				return cfs.NewPath(p.Remote())
			})
			schePath.AddConnPath(p)

			if !ok {
				sche.AddPath(schePath)
			}
		case path.Remove:
			schePath := pathMap.get(p.Remote())
			if schePath == nil {
				panic("unexpected behavior, schePath should not be nil")
			}
			schePath.RemoveConnPath(p)
		}
	}))

	mtuSize := udpmux.MTUSize
	if cfg.IsTCP {
		mtuSize = tcp.MTUSize
	}

	tunInterface, err := tun.CreateTUN(cfg.Tun.Name, mtuSize)
	if err != nil {
		return nil, err
	}
	execCommand("ip", "a", "add", cfg.LocalAddr, "peer", cfg.RemoteAddr, "dev", cfg.Tun.Name)
	execCommand("ip", "l", "set", cfg.Tun.Name, "up")

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	if cfg.IsTCP {
		tcp.ListenConn(ctx, manager, cfg.ListenAddr, tunModule.In())
	} else {
		udpmux.ListenConn(ctx, manager, cfg.ListenAddr, tunModule.In())
	}

	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}

// 2018
