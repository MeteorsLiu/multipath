package scheduler

import (
	"errors"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
)

var ErrNoPath = errors.New("failed to get path: no available path")

type Scheduler interface {
	mempool.Writer
	AddPath(path SchedulablePath)
	RemovePath(path SchedulablePath)
}

type SchedulablePath interface {
	String() string
	SetWeight(w int)
	AddConnPath(connPath path.Path)
	RemoveConnPath(connPath path.Path)
}
