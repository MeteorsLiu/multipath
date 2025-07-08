package batch

import (
	_ "unsafe"

	"golang.org/x/sys/unix"
)

//go:linkname writev golang.org/x/sys/unix.writev
func writev(fd int, iovs []unix.Iovec) (n int, err error)

//go:linkname readv golang.org/x/sys/unix.readv
func readv(fd int, iovs []unix.Iovec) (n int, err error)
