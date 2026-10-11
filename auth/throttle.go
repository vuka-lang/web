package auth

import (
	"strings"
	"sync"
	"time"
)

// Throttle limits failed logins: after PerLogin failures for one login, or
// PerIP from one address, within Window, further attempts are refused
// (429) until the oldest failure is Window old. Counts are per process.
type Throttle struct {
	PerLogin int
	PerIP    int
	Window   time.Duration
}

// ThrottleDefaults: 5 failures per login and 50 per address in 15 minutes.
var ThrottleDefaults = Throttle{PerLogin: 5, PerIP: 50, Window: 15 * time.Minute}

type throttler struct {
	cfg   Throttle
	mu    sync.Mutex
	fails map[string][]time.Time // "login:x" / "ip:y" → failure times, oldest first
	ops   int
}

func newThrottler(cfg Throttle) *throttler {
	return &throttler{cfg: cfg, fails: map[string][]time.Time{}}
}

func loginKey(login string) string { return "login:" + strings.ToLower(strings.TrimSpace(login)) }

// wait is how long attempts for login from ip must wait; 0 when they may
// go ahead.
func (t *throttler) wait(login, ip string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.waitFor(loginKey(login), t.cfg.PerLogin, now), t.waitFor("ip:"+ip, t.cfg.PerIP, now))
}

func (t *throttler) waitFor(key string, limit int, now time.Time) time.Duration {
	if limit <= 0 {
		return 0
	}
	fs := t.recent(key, now)
	if len(fs) < limit {
		return 0
	}
	return fs[len(fs)-limit].Add(t.cfg.Window).Sub(now)
}

// recent drops key's failures older than the window.
func (t *throttler) recent(key string, now time.Time) []time.Time {
	fs := t.fails[key]
	i := 0
	for i < len(fs) && now.Sub(fs[i]) >= t.cfg.Window {
		i++
	}
	if fs = fs[i:]; len(fs) == 0 {
		delete(t.fails, key)
	} else {
		t.fails[key] = fs
	}
	return fs
}

// fail counts a failed attempt.
func (t *throttler) fail(login, ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range []string{loginKey(login), "ip:" + ip} {
		fs := t.recent(k, now)
		if len(fs) >= 1000 {
			fs = fs[1:]
		}
		t.fails[k] = append(fs, now)
	}
	if t.ops++; t.ops%1000 == 0 {
		for k := range t.fails {
			t.recent(k, now)
		}
	}
}

// succeed forgets a login's failures once it signs in.
func (t *throttler) succeed(login string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, loginKey(login))
}
