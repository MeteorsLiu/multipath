package main

import (
	"context"
	"fmt"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/tun"
)

func asyncDial(sche scheduler.Scheduler, dial func() (conn.ConnWriter, error)) {
	for i := 0; ; i++ {
		conn, err := dial()
		if err == nil {
			sche.AddPath(cfs.NewPath(path.NewPath(conn)))
			break
		}
		sec := min(1<<i, 600)
		dur := time.Duration(sec) * time.Second
		fmt.Printf("dial fail: %v sleep: %v\n", err, dur)
		time.Sleep(dur)
	}
}

func NewClient(ctx context.Context, cfg Config) (func(), error) {
	if len(cfg.Remotes) == 0 {
		return nil, fmt.Errorf("failed to init client: no remote paths found")
	}

	sche := cfs.NewCFSScheduler(false)

	tunInterface, err := tun.CreateTUN(cfg.TunName, conn.MTUSize)
	if err != nil {
		return nil, err
	}

	tunModule := tun.NewHandler(ctx, tunInterface, sche)

	sema := make(chan struct{}, 1)

	for i, pathConfig := range cfg.Remotes {
		dial := func() (conn.ConnWriter, error) {
			return udpmux.DialConn(ctx, cfg.ListenAddr[i], pathConfig.RemoteAddr, tunModule.In())
		}
		go func() {
			asyncDial(sche, dial)
			select {
			case sema <- struct{}{}:
			default:
			}
		}()
	}

	// wait at least one conn
	<-sema
	tunModule.Start()

	return func() {
		tunInterface.Close()
	}, nil
}
