package debuglog

import (
	"log"
	"os"
	"strings"
)

var (
	enabled = debugEnabled(os.Getenv("MULTIPATH_DEBUG"))
	logger  = log.New(os.Stderr, "multipath debug ", log.LstdFlags|log.Lmicroseconds)
)

func Enabled() bool {
	return enabled
}

func Printf(component string, format string, args ...any) {
	if !enabled {
		return
	}
	logger.Printf(component+": "+format, args...)
}

func debugEnabled(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return value != "" && value != "0" && value != "false" && value != "off"
}
