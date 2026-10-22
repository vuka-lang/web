// Package notices is the board's notices, shared by every page and the API.
package notices

import (
	"slices"
	"sync"
)

type Notice struct {
	ID   int    `json:"id"`
	By   string `json:"by"`
	Text string `json:"text"`
}

type Form struct {
	Text string `form:"text" json:"text"`
}

// Changed is broadcast to every board when the notices change.
type Changed struct{}

// Notices are the board's: a service, registered in main.
type Notices struct {
	mu   sync.Mutex
	list []Notice
	next int
}

func New() *Notices { return &Notices{} }

func (n *Notices) Add(by, text string) Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.next++
	x := Notice{ID: n.next, By: by, Text: text}
	n.list = append(n.list, x)
	return x
}

func (n *Notices) All() []Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.list)
}

func (n *Notices) Clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = nil
}
