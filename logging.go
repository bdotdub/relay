package main

import (
	"fmt"
	"log"
	"sync/atomic"
)

var verboseLogging atomic.Bool

func setVerboseLogging(enabled bool) {
	verboseLogging.Store(enabled)
}

func verbosef(format string, args ...any) {
	if !verboseLogging.Load() {
		return
	}
	log.Printf("[verbose] "+format, args...)
}

func kvSummary(values ...any) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", values[index], values[index+1]))
	}
	return stringsJoin(parts, " ")
}
