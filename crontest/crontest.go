// Package crontest tests an app's crons: Run runs one by name with its
// dependencies, and Clock is a virtual clock to test schedules with — time
// stands still until the test moves it.
//
//	clock := crontest.NewClock(time.Date(2026, 1, 5, 6, 59, 0, 0, time.UTC))
//	app := web.New(web.CronClock(clock), web.Provide(NewStore))
//	app.Start(ctx)
//	defer app.Stop(ctx)
//	clock.WaitTimers(1)          // the scheduler waits for its tick
//	clock.Advance(time.Minute)   // 07:00: the cron runs
//	crontest.Wait(app)           // and is done
package crontest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/cron"
)

// Run runs the app's cron name now, its dependencies built by the app, and
// returns its error (a panic's too).
func Run(app *web.App, name string) error { return app.RunCron(context.Background(), name) }

// Wait waits until no cron the schedule started is still running: call it
// after a run's work is seen and before moving the clock again, as a tick
// finding the previous run still going is skipped.
func Wait(app *web.App) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return app.WaitCrons(ctx)
}

// Clock is a virtual cron.Clock: Now is what the test set, and timers fire
// when Advance or Set moves time past them.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
	moved  chan struct{} // closed when a timer is added; then made again
}

// NewClock is a virtual clock showing t.
func NewClock(t time.Time) *Clock { return &Clock{now: t, moved: make(chan struct{})} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer is a timer firing once Now reaches d from now.
func (c *Clock) NewTimer(d time.Duration) cron.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{c: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t
	}
	c.timers = append(c.timers, t)
	close(c.moved)
	c.moved = make(chan struct{})
	return t
}

// Advance moves the clock d forward, firing the timers due in order.
func (c *Clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// Set moves the clock to t, firing the timers due in order. A scheduler
// makes its next timer only after the previous one fired: to see every
// tick, move the clock one tick at a time, waiting for the timers between
// (WaitTimers).
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	slices.SortStableFunc(c.timers, func(a, b *timer) int { return a.at.Compare(b.at) })
	keep := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(t) {
			keep = append(keep, tm)
			continue
		}
		tm.ch <- tm.at
	}
	c.timers = keep
}

// WaitTimers waits until n timers are waiting — the scheduler is waiting
// for its crons' next ticks — for at most 5 seconds, reporting whether they
// are.
func (c *Clock) WaitTimers(n int) bool {
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		have, moved := len(c.timers), c.moved
		c.mu.Unlock()
		if have >= n {
			return true
		}
		select {
		case <-moved:
		case <-deadline:
			return false
		}
	}
}

// Next is when the earliest waiting timer fires; the zero time without one.
func (c *Clock) Next() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Time
	for _, t := range c.timers {
		if next.IsZero() || t.at.Before(next) {
			next = t.at
		}
	}
	return next
}

type timer struct {
	c  *Clock
	at time.Time
	ch chan time.Time
}

func (t *timer) C() <-chan time.Time { return t.ch }

func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	i := slices.Index(t.c.timers, t)
	if i < 0 {
		return false
	}
	t.c.timers = slices.Delete(t.c.timers, i, i+1)
	return true
}
