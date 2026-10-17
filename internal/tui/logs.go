package tui

import (
	"strings"
	"sync"
)

// LogBuffer collects the server's log output while the TUI owns the screen.
// It implements io.Writer, so it can be passed to log.SetOutput.
type LogBuffer struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial string
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 500
	}
	return &LogBuffer{max: max}
}

func (l *LogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := strings.Split(l.partial+string(p), "\n")
	l.partial = parts[len(parts)-1]
	for _, ln := range parts[:len(parts)-1] {
		l.lines = append(l.lines, strings.TrimRight(ln, "\r"))
	}
	if len(l.lines) > l.max {
		l.lines = append([]string(nil), l.lines[len(l.lines)-l.max:]...)
	}
	return len(p), nil
}

// Lines returns a copy of the collected lines.
func (l *LogBuffer) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}
