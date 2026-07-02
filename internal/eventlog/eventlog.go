package eventlog

import (
	"log"
	"os"
)

var logger = log.New(os.Stderr, "multipath event ", log.LstdFlags|log.Lmicroseconds)

func Printf(event string, format string, args ...any) {
	logger.Printf(event+" "+format, args...)
}
