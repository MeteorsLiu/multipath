package path

import (
	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
	"github.com/google/uuid"
)

type Path interface {
	conn.ConnWriter
	PathID() string
}

type pathImpl struct {
	id         string
	connSender conn.ConnWriter
}

func NewPath(conn conn.ConnWriter) Path {
	return &pathImpl{id: uuid.NewString(), connSender: conn}
}

func (p *pathImpl) PathID() string {
	return p.id
}

func (p *pathImpl) String() string {
	return p.connSender.String()
}

func (p *pathImpl) Prober() *prober.Prober {
	return p.connSender.Prober()
}

func (p *pathImpl) Write(b *mempool.Buffer) (err error) {
	return p.connSender.Write(b)
}
