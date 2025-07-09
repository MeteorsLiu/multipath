package tun

import (
	"github.com/MeteorsLiu/multipath/internal/conn"
)

func New(name string, mtu int) (conn.BatchConn, error) {
	return CreateTUN(name, mtu)
}
