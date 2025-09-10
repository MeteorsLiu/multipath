package main

import (
	"context"
	"fmt"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/tcp"
	"github.com/MeteorsLiu/multipath/internal/path"
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

	sema := make(chan struct{}, 1)

	sema <- struct{}{}

	for _, p := range cfg.Client.Remotes {
		var dial func()
		dial = func() {
			for i := 0; ; i++ {
				c, err := tcp.DialConn(ctx, p.RemoteAddr, tunModule.In())
				if err == nil {
					c.(*tcp.TcpConn).Start(dial)
					sche.AddPath(cfs.NewPath(path.NewPath(c)))
					return
				}
				sec := min(1<<i, 600)
				time.Sleep(time.Duration(sec) * time.Second)
			}
		}
		go func() {
			dial()
			select {
			case <-sema:
			default:
			}
		}()
	}

	sema <- struct{}{}
	fmt.Println("start tun")
	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}
