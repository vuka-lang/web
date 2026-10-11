package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/vuka"
	"github.com/vuka-lang/web/cron"
)

// Crons: functions run on a schedule, declared with @web.Cron or added with
// App.Cron. They take a context.Context and dependencies, like a handler,
// and return nothing or an error. Start schedules them after building their
// dependencies; Stop stops scheduling, waits for the runs in progress within
// its context, then cancels theirs. A run never overlaps the previous one,
// a panic is recovered, and failures are logged through slog.

type cronSpec struct {
	name, spec, where string
	fn                reflect.Value
	recv              reflect.Type
	params            []vuka.Param
	err               error // the spec's, from the declarer
}

// Cron runs the function on a schedule (see package cron for the syntax):
//
//	@web.Cron("@every 30s")
//	@web.Cron("0 7 * * *")
//	@web.Cron("CRON_TZ=Africa/Dar_es_Salaam 0 7 * * 1-5")
//	func cleanUp(ctx context.Context, store *PetStore) error { … }
//
// Its parameters are a context.Context and provided types; on a method, the
// receiver is provided too. The cron's name is the function's ("cleanUp";
// "jobs.cleanUp" outside package main; "PetStore.Refresh" for a method). An
// invalid spec fails Build, naming the function and where it is declared.
func Cron(spec string) func(*vuka.Decl) {
	return func(d *vuka.Decl) {
		s := &cronSpec{name: strings.TrimPrefix(d.Name, "main."), spec: spec, where: fmt.Sprintf("%s at %s:%d", d.Name, d.File, d.Line),
			fn: reflect.ValueOf(d.Func), recv: d.Recv, params: d.Params}
		if _, err := cron.Parse(spec); err != nil {
			s.err = fmt.Errorf("web: @web.Cron(%q) on %s: %v", spec, s.where, err)
		}
		record(func(r *registry) { r.crons = append(r.crons, declCron{d.Pkg, s}) })
	}
}

type declCron struct {
	pkg  string
	spec *cronSpec
}

// Cron runs fn on a schedule, as @web.Cron does; name names it for RunCron,
// the logs and the toolbar. fn takes a context.Context and provided types.
func (a *App) Cron(name, spec string, fn any) *App {
	a.mutable("Cron")
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		a.c.errs = append(a.c.errs, fmt.Errorf("web: cron %s: %T is not a function", name, fn))
		return a
	}
	s := &cronSpec{name: name, spec: spec, where: funcName(v), fn: v}
	for i := 0; i < v.Type().NumIn(); i++ {
		s.params = append(s.params, vuka.Param{Type: v.Type().In(i)})
	}
	if _, err := cron.Parse(spec); err != nil {
		s.err = fmt.Errorf("web: cron %s (%s): %v", name, s.where, err)
	}
	a.cronSpecs = append(a.cronSpecs, s)
	return a
}

// CronClock runs the app's crons by clock instead of the system's: a
// virtual one in tests (crontest.Clock).
func CronClock(c cron.Clock) Option { return OptionFunc(func(a *App) { a.cronClock = c }) }

// NoCrons keeps Start from scheduling the crons, for a replica that only
// serves requests; RunCron still runs them.
func NoCrons() Option { return OptionFunc(func(a *App) { a.noCrons = true }) }

// CronLock decides which process runs a scheduled tick. Each process of an
// app schedules its crons by itself, so with several replicas every one runs
// every tick; provide a CronLock (backed by a database or Redis) and only the
// replica that claims a tick runs it. Every replica asks for the same name
// and tick — @every ticks too are aligned to a fixed origin — so a lock is
// a unique key: an INSERT of (name, tick) into a table with a unique index,
// or Redis's SET name:tick NX with an expiry. Without one, the app uses
// NewMemoryCronLock, which claims every tick. RunCron doesn't ask.
type CronLock interface {
	Claim(ctx context.Context, name string, tick time.Time) (bool, error)
}

// NewMemoryCronLock is the lock of a single process: it claims each tick
// of a cron once.
func NewMemoryCronLock() CronLock { return &memoryLock{last: map[string]time.Time{}} }

type memoryLock struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *memoryLock) Claim(_ context.Context, name string, tick time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !tick.After(l.last[name]) {
		return false, nil
	}
	l.last[name] = tick
	return true, nil
}

// ErrCronRunning is RunCron's error when the cron's previous run hasn't
// finished: runs never overlap.
var ErrCronRunning = errors.New("the previous run is still running")

var cronLockType = typeOf[CronLock]()

type scheduler struct {
	app    *App
	jobs   []*cronJob
	clock  cron.Clock
	lock   CronLock
	ctx    context.Context // the runs'; cancelled when Stop gives up waiting
	cancel context.CancelFunc
	halt   context.CancelFunc // stops the scheduling loops
	loops  sync.WaitGroup
	runs   sync.WaitGroup
	mu     sync.Mutex
	closed bool
	active atomic.Int64 // scheduled and toolbar runs started and not done
}

type cronJob struct {
	name, spec, where string
	sched             cron.Schedule
	call              func(context.Context) error
	running           atomic.Bool

	mu   sync.Mutex
	next time.Time
	last cronRun
}

type cronRun struct {
	ID     string // the toolbar's record
	At     time.Time
	Dur    time.Duration
	Err    string
	Manual bool
}

// compileCrons checks the crons and makes the app's scheduler.
func (a *App) compileCrons() []error {
	var specs []*cronSpec
	for _, c := range a.reg.crons {
		if a.take(c.pkg) {
			specs = append(specs, c.spec)
		}
	}
	specs = append(specs, a.cronSpecs...)
	if len(specs) == 0 {
		return nil
	}
	var errs []error
	s := &scheduler{app: a, clock: a.cronClock}
	if s.clock == nil {
		s.clock = cron.RealClock
	}
	seen := map[string]string{}
	for _, c := range specs {
		if c.err != nil {
			errs = append(errs, c.err)
			continue
		}
		if prev, dup := seen[c.name]; dup {
			errs = append(errs, fmt.Errorf("web: cron %s is declared twice: by %s and by %s", c.name, prev, c.where))
			continue
		}
		seen[c.name] = c.where
		j, err := a.compileCron(c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.jobs = append(s.jobs, j)
	}
	a.sched = s
	return errs
}

func (a *App) compileCron(c *cronSpec) (*cronJob, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("web: cron %s (%s): %s", c.name, c.where, fmt.Sprintf(format, args...))
	}
	t := c.fn.Type()
	if t.NumOut() > 1 || t.NumOut() == 1 && t.Out(0) != errType {
		return nil, fail("returns %s; a cron returns nothing or an error", t)
	}
	var deps []reflect.Type // nil: the context
	deref := false          // a value receiver of a provided pointer
	if c.recv != nil {
		r := c.recv
		if !a.c.has(r) && r.Kind() != reflect.Pointer && a.c.has(reflect.PointerTo(r)) {
			r, deref = reflect.PointerTo(r), true
		}
		if !a.c.has(r) {
			return nil, fail("the receiver %s is not a @web.Service or a provided type", c.recv)
		}
		deps = append(deps, r)
	}
	for i, p := range c.params {
		if p.Type == contextType {
			deps = append(deps, nil)
			continue
		}
		if !a.c.has(p.Type) {
			label := cmp.Or(p.Name, fmt.Sprint("#", i+1))
			return nil, fail("parameter %s %s: a cron takes a context.Context and provided types; no provider for %s", label, p.Type, p.Type)
		}
		deps = append(deps, p.Type)
	}
	for _, d := range deps {
		if d == nil {
			continue
		}
		if err := a.c.check(d, "cron "+c.name+" ("+c.where+")"); err != nil {
			return nil, err
		}
		a.needs = append(a.needs, d)
	}
	sched, _ := cron.Parse(c.spec)
	j := &cronJob{name: c.name, spec: c.spec, where: c.where, sched: sched}
	j.call = func(ctx context.Context) error {
		args := make([]reflect.Value, len(deps))
		for i, d := range deps {
			if d == nil {
				args[i] = reflect.ValueOf(&ctx).Elem()
				continue
			}
			v, err := a.c.get(d)
			if err != nil {
				return err
			}
			if i == 0 && deref {
				v = v.Elem()
			}
			args[i] = v
		}
		if out := c.fn.Call(args); len(out) == 1 && !out[0].IsNil() {
			return out[0].Interface().(error)
		}
		return nil
	}
	return j, nil
}

// RunCron runs the cron name now, in the caller's goroutine with ctx, and
// returns its error (a panic's too). It doesn't wait for a run in progress:
// it returns ErrCronRunning. For tests, and commands running a cron by
// hand; the schedule's lock isn't asked.
func (a *App) RunCron(ctx context.Context, name string) error {
	if err := a.Build(); err != nil {
		return err
	}
	j := a.sched.job(name)
	if j == nil {
		return fmt.Errorf("web: no cron named %q%s", name, a.sched.names())
	}
	_, err := a.sched.run(ctx, j, time.Time{}, true, nil)
	return err
}

func (s *scheduler) job(name string) *cronJob {
	if s == nil {
		return nil
	}
	if i := slices.IndexFunc(s.jobs, func(j *cronJob) bool { return j.name == name }); i >= 0 {
		return s.jobs[i]
	}
	return nil
}

func (s *scheduler) names() string {
	if s == nil || len(s.jobs) == 0 {
		return "; the app has none"
	}
	var ns []string
	for _, j := range s.jobs {
		ns = append(ns, j.name)
	}
	return "; the app has " + strings.Join(ns, ", ")
}

// start schedules every cron: a loop each, waiting on the clock.
func (s *scheduler) start(context.Context) error {
	a := s.app
	if s.lock == nil {
		s.lock = NewMemoryCronLock()
		if a.c.has(cronLockType) {
			v, err := a.c.get(cronLockType)
			if err != nil {
				return err
			}
			s.lock = v.Interface().(CronLock)
		}
	}
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	loops, halt := context.WithCancel(s.ctx)
	s.halt = halt
	for _, j := range s.jobs {
		s.loops.Add(1)
		go s.loop(loops, j)
		if a.dev {
			log.Printf("web: cron %s (%s) next at %s", j.name, j.spec, j.sched.Next(s.clock.Now()).Format(time.DateTime+" MST"))
		}
	}
	return nil
}

// loop waits for each tick of j and starts its run. A tick missed (the
// machine slept, the clock jumped) isn't run late: the next one is.
func (s *scheduler) loop(ctx context.Context, j *cronJob) {
	defer s.loops.Done()
	now := s.clock.Now()
	for {
		next := j.sched.Next(now)
		if next.IsZero() {
			return
		}
		j.mu.Lock()
		j.next = next
		j.mu.Unlock()
		t := s.clock.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		s.tick(j, next)
		if now = s.clock.Now(); now.Before(next) {
			now = next
		}
	}
}

// tick starts j's run for the tick at, unless the previous run is still
// going or another replica claimed the tick.
func (s *scheduler) tick(j *cronJob, at time.Time) {
	if j.running.Load() {
		slog.Warn("cron skipped: the previous run is still running", "cron", j.name, "tick", at)
		return
	}
	if !s.enter() {
		return
	}
	go func() {
		defer s.leave()
		ok, err := s.lock.Claim(s.ctx, j.name, at)
		if err != nil {
			slog.Error("cron skipped: the lock failed", "cron", j.name, "tick", at, "err", err)
			return
		}
		if ok {
			s.run(s.ctx, j, at, false, nil)
		}
	}()
}

// enter counts a run in, unless the scheduler has stopped.
func (s *scheduler) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.runs.Add(1)
	s.active.Add(1)
	return true
}

// leave counts a run entered out.
func (s *scheduler) leave() {
	s.active.Add(-1)
	s.runs.Done()
}

// WaitCrons waits until no run the schedule (or the toolbar) started is in
// progress, or ctx is done: for tests moving a virtual clock, before they
// move it again — a tick finding the previous run still going is skipped.
func (a *App) WaitCrons(ctx context.Context) error {
	if a.sched == nil {
		return nil
	}
	for a.sched.active.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

// run runs j once: recorded by the toolbar (onEntry gets its record's
// id), its panic recovered, its failure logged.
func (s *scheduler) run(ctx context.Context, j *cronJob, at time.Time, manual bool, onEntry func(string)) (id string, err error) {
	if !j.running.CompareAndSwap(false, true) {
		if !manual {
			slog.Warn("cron skipped: the previous run is still running", "cron", j.name, "tick", at)
		}
		return "", fmt.Errorf("web: cron %s: %w", j.name, ErrCronRunning)
	}
	defer j.running.Store(false)
	start := time.Now()
	for _, f := range s.app.cronCtx {
		ctx = f(ctx)
	}
	ctx, finish := s.app.cronEntry(ctx, j)
	if finish != nil {
		id = entryOf(ctx).ID
	}
	if onEntry != nil {
		onEntry(id)
	}
	err = j.safeCall(ctx)
	dur := time.Since(start)
	if err != nil {
		slog.ErrorContext(ctx, "cron failed", "cron", j.name, "err", err, "took", dur)
	} else {
		slog.DebugContext(ctx, "cron ran", "cron", j.name, "took", dur)
	}
	if finish != nil {
		finish(err)
	}
	j.mu.Lock()
	j.last = cronRun{ID: id, At: start, Dur: dur, Manual: manual}
	if err != nil {
		j.last.Err = err.Error()
	}
	j.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("web: cron %s: %w", j.name, err)
	}
	return id, err
}

func (j *cronJob) safeCall(ctx context.Context) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			slog.ErrorContext(ctx, "cron panicked", "cron", j.name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	return j.call(ctx)
}

// stop ends the scheduling loops, waits for the runs in progress until ctx
// is done, then cancels their context.
func (s *scheduler) stop(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.halt()
	s.loops.Wait()
	done := make(chan struct{})
	go func() { s.runs.Wait(); close(done) }()
	defer s.cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		var busy []string
		for _, j := range s.jobs {
			if j.running.Load() {
				busy = append(busy, j.name)
			}
		}
		return fmt.Errorf("web: crons still running, cancelled: %s: %w", strings.Join(busy, ", "), ctx.Err())
	}
}
