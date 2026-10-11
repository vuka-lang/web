package web_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/web"
	"github.com/vuka-lang/web/crontest"
)

// runs records a cron's runs by the clock's time.
type runs struct {
	mu    sync.Mutex
	at    []string
	clock *crontest.Clock
	ch    chan struct{}
}

func (r *runs) run(context.Context) {
	r.mu.Lock()
	r.at = append(r.at, r.clock.Now().Format("15:04:05"))
	r.mu.Unlock()
	r.ch <- struct{}{}
}

func (r *runs) list() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.at, " ")
}

// waitTimers waits for the scheduler to wait on n timers.
func waitTimers(t *testing.T, c *crontest.Clock, n int) {
	t.Helper()
	if !c.WaitTimers(n) {
		t.Fatalf("the scheduler isn't waiting on %d timers (now %s, next %s)", n, c.Now(), c.Next())
	}
}

func receive(t *testing.T, ch chan struct{}, c *crontest.Clock) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no run (now %s, next timer %s)", c.Now(), c.Next())
	}
}

func start(t *testing.T, a *web.App) {
	t.Helper()
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
}

func TestCronSchedule(t *testing.T) {
	clock := crontest.NewClock(time.Date(2026, 1, 5, 6, 58, 30, 0, time.UTC)) // a Monday
	r := &runs{clock: clock, ch: make(chan struct{}, 10)}
	a := web.New(web.Only("none"), web.CronClock(clock))
	a.Cron("report", "0 7 * * 1-5", r.run)
	a.Cron("tick", "@every 30s", func() {})
	start(t, a)
	waitTimers(t, clock, 2)
	if n := clock.Next(); !n.Equal(time.Date(2026, 1, 5, 6, 59, 0, 0, time.UTC)) {
		t.Errorf("the first timer: %s", n)
	}
	clock.Set(time.Date(2026, 1, 5, 6, 59, 59, 0, time.UTC))
	select {
	case <-r.ch:
		t.Fatal("ran early")
	case <-time.After(20 * time.Millisecond):
	}
	// Each weekday at 07:00, the weekend skipped.
	for _, day := range []int{5, 6, 7, 8, 9, 12} {
		waitTimers(t, clock, 2)
		clock.Set(time.Date(2026, 1, day, 7, 0, 0, 0, time.UTC))
		receive(t, r.ch, clock)
		if err := crontest.Wait(a); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.list(); got != "07:00:00 07:00:00 07:00:00 07:00:00 07:00:00 07:00:00" {
		t.Errorf("runs: %s", got)
	}
	waitTimers(t, clock, 2)
	if n := clock.Next(); !n.Equal(time.Date(2026, 1, 12, 7, 0, 30, 0, time.UTC)) {
		t.Errorf("next: %s", n) // tick's, at the next half minute
	}
}

func TestCronNoOverlapAndStop(t *testing.T) {
	logs := web.CaptureSlog(t)

	clock := crontest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	started, release := make(chan struct{}, 10), make(chan struct{})
	var ctxErr error
	var mu sync.Mutex
	a := web.New(web.Only("none"), web.CronClock(clock))
	a.Cron("slow", "@every 1m", func(ctx context.Context) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			mu.Lock()
			ctxErr = ctx.Err()
			mu.Unlock()
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitTimers(t, clock, 1)
	clock.Advance(time.Minute)
	receive(t, started, clock)
	waitTimers(t, clock, 1)
	clock.Advance(time.Minute) // still running: skipped
	waitTimers(t, clock, 1)
	select {
	case <-started:
		t.Fatal("two runs at once")
	case <-time.After(30 * time.Millisecond):
	}
	if !strings.Contains(logs.String(), `msg="cron skipped: the previous run is still running" cron=slow`) {
		t.Errorf("logs: %s", logs.String())
	}
	if err := a.RunCron(context.Background(), "slow"); !errors.Is(err, web.ErrCronRunning) {
		t.Errorf("RunCron while running: %v", err)
	}

	// Stop waits for the run within its context, then cancels it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := a.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "crons still running, cancelled: slow") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stop: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		e := ctxErr
		mu.Unlock()
		if e != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's context wasn't cancelled")
		}
		time.Sleep(time.Millisecond)
	}
	if !clock.Next().IsZero() {
		t.Error("a timer after Stop")
	}
}

func TestCronStopWaits(t *testing.T) {
	clock := crontest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	started := make(chan struct{})
	var done bool
	a := web.New(web.Only("none"), web.CronClock(clock))
	a.Cron("short", "@every 1m", func(ctx context.Context) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		done = ctx.Err() == nil
	})
	a.Start(context.Background())
	waitTimers(t, clock, 1)
	clock.Advance(time.Minute)
	receive(t, started, clock)
	if err := a.Stop(context.Background()); err != nil || !done {
		t.Errorf("Stop: %v, finished %v", err, done)
	}
}

// refuse is a lock another replica always wins.
type refuse struct{ asked []string }

func (l *refuse) Claim(_ context.Context, name string, tick time.Time) (bool, error) {
	l.asked = append(l.asked, name+" "+tick.Format("15:04"))
	return false, nil
}

func TestCronLock(t *testing.T) {
	clock := crontest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	lock := &refuse{}
	ran := make(chan struct{}, 1)
	a := web.New(web.Only("none"), web.CronClock(clock), web.Provide(func() web.CronLock { return lock }))
	a.Cron("once", "@every 1m", func() { ran <- struct{}{} })
	start(t, a)
	waitTimers(t, clock, 1)
	clock.Advance(time.Minute)
	waitTimers(t, clock, 1)
	select {
	case <-ran:
		t.Fatal("ran without the lock")
	case <-time.After(30 * time.Millisecond):
	}
	if err := crontest.Run(a, "once"); err != nil { // by hand: the lock isn't asked
		t.Fatal(err)
	}
	receive(t, ran, clock)

	m := web.NewMemoryCronLock()
	tick := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	ok1, _ := m.Claim(context.Background(), "x", tick)
	ok2, _ := m.Claim(context.Background(), "x", tick)
	ok3, _ := m.Claim(context.Background(), "y", tick)
	if !ok1 || ok2 || !ok3 {
		t.Errorf("memory lock: %v %v %v", ok1, ok2, ok3)
	}
}

func TestNoCrons(t *testing.T) {
	clock := crontest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := web.New(web.Only("none"), web.CronClock(clock), web.NoCrons())
	a.Cron("x", "@every 1m", func() {})
	start(t, a)
	time.Sleep(10 * time.Millisecond)
	if !clock.Next().IsZero() {
		t.Error("NoCrons scheduled")
	}
	if err := crontest.Run(a, "x"); err != nil {
		t.Error(err)
	}
}
