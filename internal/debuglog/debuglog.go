package debuglog

import (
	"log"
	"os"
	"strings"
)

var (
	enabled         = debugEnabled(os.Getenv("MULTIPATH_DEBUG"))
	logger          = log.New(os.Stderr, "multipath debug ", log.LstdFlags|log.Lmicroseconds)
	componentFilter = parseComponentFilter(os.Getenv("MULTIPATH_DEBUG_COMPONENTS"))
)

func Enabled() bool {
	return enabled
}

func Printf(component string, format string, args ...any) {
	if !enabled {
		return
	}
	if componentFilter != nil && !componentFilter[component] {
		return
	}
	logger.Printf(component+": "+format, args...)
}

func debugEnabled(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return value != "" && value != "0" && value != "false" && value != "off"
}

func parseComponentFilter(value string) map[string]bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	filter := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			filter[p] = true
		}
	}
	if len(filter) == 0 {
		return nil
	}
	return filter
}
