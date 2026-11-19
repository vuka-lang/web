package web

import (
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

// RestartStatus is the exit status of a program whose Run was ended by
// Restart, so a supervisor can tell a restart it asked for from a failure.
const RestartStatus = 75

var restartEnv struct {
	sync.RWMutex
	keys map[string]bool
}

// RestartOnlyEnv records environment variables the program reads only while
// it starts, so a change to one doesn't reach it until it restarts. The
// compiler writes the call for the keys it finds; a program rarely makes it.
func RestartOnlyEnv(keys ...string) {
	restartEnv.Lock()
	defer restartEnv.Unlock()
	if restartEnv.keys == nil {
		restartEnv.keys = map[string]bool{}
	}
	for _, k := range keys {
		restartEnv.keys[k] = true
	}
}

// NeedsRestart reports whether key was recorded by RestartOnlyEnv: its value
// is read only while the program starts. A key the compiler couldn't classify
// reports false.
func NeedsRestart(key string) bool {
	restartEnv.RLock()
	defer restartEnv.RUnlock()
	return restartEnv.keys[key]
}

var stoppers struct {
	sync.Mutex
	seq   uint64
	stops map[uint64]func()
}

var exit = os.Exit

// Restart asks every stoppers App to stop as on SIGTERM: Run drains its
// connections, stops the services and then exits the process with
// RestartStatus, for a supervisor to start it again. It reports whether any
// App was stoppers to be asked; the exit happens in Run, once it has stopped.
func Restart() bool {
	stoppers.Lock()
	stops := slices.Collect(func(yield func(func()) bool) {
		for _, f := range stoppers.stops {
			if !yield(f) {
				return
			}
		}
	})
	stoppers.Unlock()
	for _, f := range stops {
		f()
	}
	return len(stops) > 0
}

// onRestart registers stop to be called by Restart until the returned
// function is called; stop reports through restarted that it was Restart.
func onRestart(stop func()) (restarted *atomic.Bool, unregister func()) {
	restarted = new(atomic.Bool)
	stoppers.Lock()
	defer stoppers.Unlock()
	if stoppers.stops == nil {
		stoppers.stops = map[uint64]func(){}
	}
	stoppers.seq++
	id := stoppers.seq
	stoppers.stops[id] = func() { restarted.Store(true); stop() }
	return restarted, func() {
		stoppers.Lock()
		defer stoppers.Unlock()
		delete(stoppers.stops, id)
	}
}
