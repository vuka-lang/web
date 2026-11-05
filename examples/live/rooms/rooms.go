// Package rooms is the chat's: what it broadcasts, and each room's history.
package rooms

import (
	"slices"
	"sync"
)

// Line is a line of a room: what its chat streams, keyed by ID.
type Line struct {
	ID   int64
	Text string
}

// Said and Joined are what the chat broadcasts; each reaches its own Update.
type Said struct{ Line }
type Joined struct{ Line }

type Form struct {
	Text string `form:"text"`
}

// Log keeps each room's history, so a page joining late sees it: a
// service, provided in main.
type Log struct {
	mu    sync.Mutex
	next  int64
	rooms map[string][]Line
}

func NewLog() *Log { return &Log{} }

// Add adds a line to a room's history and returns it, numbered.
func (l *Log) Add(room, text string) Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rooms == nil {
		l.rooms = map[string][]Line{}
	}
	l.next++
	line := Line{ID: l.next, Text: text}
	l.rooms[room] = append(l.rooms[room], line)
	return line
}

// History is a room's last n lines.
func (l *Log) History(room string, n int) []Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := l.rooms[room]
	return slices.Clone(lines[max(0, len(lines)-n):])
}
