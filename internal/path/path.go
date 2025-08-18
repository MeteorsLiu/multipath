package path

import (
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

type Path interface {
	mempool.Writer
	PathID() string
}

type pathImpl struct {
	id         string
	connSender mempool.Writer
}

func NewPath(conn mempool.Writer) Path {
	return &pathImpl{id: uuid.NewString(), connSender: conn}
}

func (p *pathImpl) PathID() string {
	return p.id
}

func (p *pathImpl) Write(b *mempool.Buffer) (err error) {
	return p.connSender.Write(b)
}
