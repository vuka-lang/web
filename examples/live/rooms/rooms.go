// Package rooms is the chat's: what it broadcasts, and each room's history.
package rooms

import (
	"slices"
	"sync"
)

// Said and Joined are what the chat broadcasts; each reaches its own Update.
type Said struct{ Who, Text string }
type Joined struct{ Who string }

type Form struct {
	Text string `form:"text"`
}

// Log keeps each room's history, so a page joining late sees it: a
// service, provided in main.
type Log struct {
	mu    sync.Mutex
	rooms map[string][]string
}

func NewLog() *Log { return &Log{} }

func (l *Log) Add(room, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rooms == nil {
		l.rooms = map[string][]string{}
	}
	l.rooms[room] = append(l.rooms[room], line)
}

func (l *Log) History(room string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.rooms[room])
}
