// Package feed is what the database-free scenario's pages share with the
// server's broadcast endpoint: the topic, the message and how a page shows it.
package feed

import "strconv"

// Topic is the topic the feed pages subscribe to unless ?topic= names another.
const Topic = "feed"

// Tick is one broadcast: its sequence number and the server's clock, in unix
// nanoseconds, when it was sent.
type Tick struct {
	Seq int
	At  int64
}

// Tag is what a feed page renders of the last tick it took. A client finds
// the sequence number and the send time in the frame that carries it, and so
// measures the time from Broadcast to its arrival.
func (t Tick) Tag() string {
	return "~S" + strconv.Itoa(t.Seq) + ":" + strconv.FormatInt(t.At, 10) + "~"
}

// Rows is the number of lines a page renders: ?rows=, 20 by default, 500 at
// most.
func Rows(n int) int {
	switch {
	case n <= 0:
		return 20
	case n > 500:
		return 500
	}
	return n
}

// Lines numbers a page's rows.
func Lines(n int) []int {
	out := make([]int, Rows(n))
	for i := range out {
		out[i] = i
	}
	return out
}
