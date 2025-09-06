package main

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewClient(ctx context.Context, cfg Config) (func(), error) {
	if len(cfg.Client.Remotes) == 0 {
		return nil, fmt.Errorf("failed to init client: no remote paths found")
	}

	pathMap := newSchedulablePathManager()
	sche := cfs.NewCFSScheduler(false)

	manager := conn.NewManager(conn.WithOnNewPath(func(event conn.ManagerEvent, cw conn.ConnWriter) {
		switch event {
		case conn.ConnAppend:
			debug.PrintStack()
			fmt.Println("new path", cw.String())
			path := cfs.NewPath(path.NewPath(cw))
			pathMap.add(cw.String(), path)
			sche.AddPath(path)
		case conn.ConnRemove:
			if path := pathMap.get(cw.String()); path != nil {
				fmt.Println("remove path", cw.String())
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

	for _, path := range cfg.Client.Remotes {
		udpmux.DialConn(ctx, manager, path.RemoteAddr, tunModule.In())
	}

	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}
