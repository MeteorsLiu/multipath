package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
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

	tunInterface, err := tun.CreateTUN(cfg.Tun.Name, conn.MTUSize)
	if err != nil {
		return nil, err
	}
	execCommand("ip", "a", "add", cfg.LocalAddr, "peer", cfg.RemoteAddr, "dev", cfg.Tun.Name)
	execCommand("ip", "l", "set", cfg.Tun.Name, "up")

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	for _, path := range cfg.Client.Remotes {
		if cfg.IsTCP {
			tcp.DialConn(ctx, manager, path.RemoteAddr, tunModule.In())
			continue
		}
		udpmux.DialConn(ctx, manager, path.RemoteAddr, tunModule.In())
	}

	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}

func execCommand(cmd string, args ...string) {
	current := exec.Command(cmd, args...)
	current.Stdout = os.Stdout
	current.Stderr = os.Stderr
	current.Run()
}
