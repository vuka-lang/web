package web

import (
	"log"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
)

// EnvChange is one variable ApplyEnv changed. Old and New are the values
// before and after; an unset variable is "", as Env reads it.
type EnvChange struct {
	Key, Old, New string
}

var envSources struct {
	m map[string]map[string]string // guarded by envState: source -> key -> value it set
}

// ApplyEnv sets values in the process environment as the named source, below
// the real environment and above the env files, and returns the variables
// whose value changed, sorted by key. A config server's client calls it at
// start-up and again on every new version of its snapshot; the rule is the same.
//
// The source owns the keys it has set. A call replaces the source's previous
// set: a key it set before and no longer provides goes back to the value an
// env file gave it, or is unset when none did. A key the real environment
// holds is never touched, and neither is one that os.Setenv changed since the
// source set it: the source no longer owns it. A key another source owns is
// left to that source. Keys that web reserves (VUKA_…) are ignored.
//
// Each variable is set with os.Setenv, so a concurrent Env, Hot.Get or
// os.Getenv sees the old or the new value of a key, never a mix. Several
// keys change one after the other, not together. Callbacks registered with
// OnEnvChange run after the values are set, in the calling goroutine.
func ApplyEnv(source string, values map[string]string) []EnvChange {
	envState.Lock()
	if envSources.m == nil {
		envSources.m = map[string]map[string]string{}
	}
	prev := envSources.m[source]
	next := map[string]string{}
	var changes []EnvChange
	set := func(k, v string, had bool) {
		old := os.Getenv(k)
		if had {
			os.Setenv(k, v)
		} else {
			os.Unsetenv(k)
		}
		if old != v {
			changes = append(changes, EnvChange{k, old, v})
		}
	}
	for k, v := range values {
		if reservedEnv(k) {
			continue
		}
		cur, present := os.LookupEnv(k)
		if pv, owned := prev[k]; owned {
			if !present || cur != pv {
				continue
			}
		} else if present && (heldByOther(source, k, cur) || !envFileValue(k, cur)) {
			continue
		}
		next[k] = v
		set(k, v, true)
	}
	for k, pv := range prev {
		if _, kept := values[k]; kept {
			continue
		}
		if cur, present := os.LookupEnv(k); !present || cur != pv {
			continue
		}
		if fv, ok := envFileSet(k); ok {
			set(k, fv, true)
		} else {
			set(k, "", false)
		}
	}
	if len(next) == 0 {
		delete(envSources.m, source)
	} else {
		envSources.m[source] = next
	}
	envState.Unlock()
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	fireEnvChanges(changes)
	return changes
}

func envFileSet(key string) (string, bool) {
	for _, l := range envState.loads {
		if v, ok := l.set[key]; ok {
			return v, true
		}
	}
	return "", false
}

func envFileValue(key, cur string) bool {
	v, ok := envFileSet(key)
	return ok && v == cur
}

func heldByOther(source, key, cur string) bool {
	for name, m := range envSources.m {
		if name != source {
			if v, ok := m[key]; ok && v == cur {
				return true
			}
		}
	}
	return false
}

var envWatch struct {
	sync.Mutex
	seq     uint64
	entries []envWatcher
}

type envWatcher struct {
	id  uint64
	key string
	fn  func(old, new string)
}

// OnEnvChange runs fn after ApplyEnv changes the variable key, with its old
// and new values. For a value read once at start and kept (a limit applied to
// a pool, a client built from it); code that reads the variable where it uses
// it needs nothing, and Hot gives a handle that is always current.
//
// Callbacks run in the goroutine that called ApplyEnv, outside web's locks,
// one after another in registration order, once the whole apply is done, so
// a callback may read other keys of the same apply, register or unregister
// callbacks, or call ApplyEnv. A panic in a callback is recovered and logged
// with the key and the panic's type, never the value; the others still run.
// The returned function unregisters fn; a callback unregistered while an
// apply is under way may still be called by it.
func OnEnvChange(key string, fn func(old, new string)) (unregister func()) {
	envWatch.Lock()
	defer envWatch.Unlock()
	envWatch.seq++
	id := envWatch.seq
	envWatch.entries = append(envWatch.entries, envWatcher{id, key, fn})
	return func() {
		envWatch.Lock()
		defer envWatch.Unlock()
		envWatch.entries = slices.DeleteFunc(envWatch.entries, func(w envWatcher) bool { return w.id == id })
	}
}

func fireEnvChanges(changes []EnvChange) {
	if len(changes) == 0 {
		return
	}
	envWatch.Lock()
	var run []func()
	for _, w := range envWatch.entries {
		for _, c := range changes {
			if c.Key == w.key {
				w, c := w, c
				run = append(run, func() { callEnvWatcher(w, c) })
			}
		}
	}
	envWatch.Unlock()
	for _, f := range run {
		f()
	}
}

func callEnvWatcher(w envWatcher, c EnvChange) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("web: OnEnvChange(%s) panicked (%T)", w.key, r)
		}
	}()
	w.fn(c.Old, c.New)
}

// HotValue is a variable read as a T that stays current while the app runs.
type HotValue[T any] struct {
	key string
	def []T
	cur atomic.Pointer[hotValue[T]]
}

type hotValue[T any] struct {
	raw string
	v   T
}

// Hot returns a handle on the environment variable key whose Get is always
// current: limit := web.Hot[int]("RATE_LIMIT", 100). Conversion
// and defaults are Env's; the value is parsed again only when the variable's
// text has changed since the last Get, so a hot path pays for one os.Getenv and
// a comparison. A value that doesn't convert is reported, as Env reports it,
// once per change of the text.
func Hot[T any](key string, def ...T) *HotValue[T] {
	return &HotValue[T]{key: key, def: def}
}

// Get returns the variable's current value, or the default when it is unset,
// empty or doesn't convert. It is safe for concurrent use. A slice or pointer
// it returns is shared by every caller until the variable changes: read it,
// don't modify it.
func (h *HotValue[T]) Get() T {
	raw := os.Getenv(h.key)
	if c := h.cur.Load(); c != nil && c.raw == raw {
		return c.v
	}
	c := &hotValue[T]{raw: raw, v: envOr(h.key, raw, h.def)}
	h.cur.Store(c)
	return c.v
}
