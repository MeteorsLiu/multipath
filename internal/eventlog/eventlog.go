package eventlog

import (
	"log"
	"os"
)

var logger = log.New(os.Stderr, "multipath event ", log.LstdFlags|log.Lmicroseconds)

func Printf(event string, format string, args ...any) {
	if !shouldPrint(event) {
		return
	}
	logger.Printf(event+" "+format, args...)
}

func shouldPrint(event string) bool {
	switch event {
	case "selector", "ping", "reconnect":
		return true
	default:
		return false
	}
}
