package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"

	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func NewClient(ctx context.Context, cfg Config) (func(), error) {
	if len(cfg.Client.Remotes) == 0 {
		return nil, fmt.Errorf("failed to init client: no remote paths found")
	}
	prom.SetupServer(cfg.PromListenAddr)

	pathMap := newSchedulablePathManager()
	sche := cfs.NewCFSScheduler()

	manager := path.NewManager(path.WithOnNewPath(func(event path.ManagerEvent, connPath path.Path) {
		switch event {
		case path.Append:
			schePath := pathMap.get(connPath.Remote())
			if schePath == nil {
				panic("unexpected behavior, schePath should not be nil")
			}
			schePath.AddConnPath(connPath)
		case path.Remove:
			schePath := pathMap.get(connPath.Remote())
			if schePath == nil {
				panic("unexpected behavior, schePath should not be nil")
			}
			schePath.RemoveConnPath(connPath)
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

	for _, path := range cfg.Client.Remotes {
		host, _, _ := net.SplitHostPort(path.RemoteAddr)

		schePath := cfs.NewPath(host)
		pathMap.add(host, schePath)
		sche.AddPath(schePath)

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
