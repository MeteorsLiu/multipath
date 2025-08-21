package main

import (
	"context"
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewServer(ctx context.Context, cfg Config) (closeFn func(), err error) {
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("failed to init server: no listen addr")
	}

	pathMap := newSchedulablePathManager()
	sche := cfs.NewCFSScheduler(true)

	manager := conn.NewManager(conn.WithOnNewPath(func(event conn.ManagerEvent, cw conn.ConnWriter) {
		switch event {
		case conn.ConnAppend:
			path := cfs.NewPath(path.NewPath(cw))
			pathMap.add(cw.String(), path)
			sche.AddPath(path)
		case conn.ConnRemove:
			if path := pathMap.get(cw.String()); path != nil {
				sche.RemovePath(path)
				pathMap.remove(cw.String())
			}
		}
	}))

	tunInterface, err := tun.CreateTUN(cfg.TunName, conn.MTUSize)
	if err != nil {
		return nil, err
	}

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	udpmux.ListenConn(ctx, manager, cfg.ListenAddr, tunModule.In())

	return func() {
		tunInterface.Close()
	}, nil
}

// 2018
