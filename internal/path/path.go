package path

import (
	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
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

func (p *pathImpl) Write(b *mempool.Buffer) (err error) {
	return p.connSender.Write(b)
}

func (p *pathImpl) Remote() string {
	return p.connSender.Remote()
}
