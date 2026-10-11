package auth

import (
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Throttle limits failed logins: after PerLogin failures for one login, or
// for the account a login finds, or PerIP from one address, within Window,
// further attempts are refused (429) until the oldest failure is Window
// old. Counts are per process.
type Throttle struct {
	PerLogin int
	PerIP    int
	Window   time.Duration
}

// ThrottleDefaults: 5 failures per login and 50 per address in 15 minutes.
var ThrottleDefaults = Throttle{PerLogin: 5, PerIP: 50, Window: 15 * time.Minute}

// maxThrottleKeys bounds the logins, accounts and addresses counted at
// once: past it, the one whose last failure is oldest is forgotten.
const maxThrottleKeys = 100_000

// maxLoginKey bounds a login's key, in bytes.
const maxLoginKey = 256

type throttler struct {
	cfg   Throttle
	mu    sync.Mutex
	fails map[string][]time.Time // "login:x" / "user:id" / "ip:y" → failure times, oldest first
	ops   int
	max   int
}

func newThrottler(cfg Throttle) *throttler {
	return &throttler{cfg: cfg, fails: map[string][]time.Time{}, max: maxThrottleKeys}
}

// foldLogin is how logins compare: compatibility forms joined (NFKC), case
// folded, accents dropped — as a database collation that is case- and
// accent-insensitive finds the same row for "Admin", "ADMIN" and "ádmin",
// they count as one login.
var foldLogin = sync.Pool{New: func() any {
	return transform.Chain(norm.NFKC, cases.Fold(), norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
}}

func loginKey(login string) string {
	login = strings.TrimSpace(login)
	if len(login) > 4*maxLoginKey {
		login = login[:4*maxLoginKey]
	}
	t := foldLogin.Get().(transform.Transformer)
	folded, _, err := transform.String(t, login)
	foldLogin.Put(t)
	if err != nil {
		folded = strings.ToLower(login)
	}
	if len(folded) > maxLoginKey {
		folded = folded[:maxLoginKey]
		for !utf8.ValidString(folded) {
			folded = folded[:len(folded)-1]
		}
	}
	return "login:" + folded
}

func userKey(uid string) string {
	if len(uid) > maxLoginKey {
		uid = uid[:maxLoginKey]
	}
	return "user:" + uid
}

// wait is how long attempts for login from ip must wait; 0 when they may
// go ahead.
func (t *throttler) wait(login, ip string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.waitFor(loginKey(login), t.cfg.PerLogin, now), t.waitFor("ip:"+ip, t.cfg.PerIP, now))
}

// waitUser is how long attempts at the account uid must wait, whatever
// login found it.
func (t *throttler) waitUser(uid string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waitFor(userKey(uid), t.cfg.PerLogin, now)
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

// fail counts a failed attempt for login from ip, and for the account uid
// when the login found one.
func (t *throttler) fail(login, ip, uid string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := []string{loginKey(login), "ip:" + ip}
	if uid != "" {
		keys = append(keys, userKey(uid))
	}
	for _, k := range keys {
		fs := t.recent(k, now)
		if len(fs) == 0 && len(t.fails) >= t.max {
			t.evict(now)
		}
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

// evict makes room for a key: the expired go, then, if that isn't
// enough, the key whose last failure is oldest.
func (t *throttler) evict(now time.Time) {
	for k := range t.fails {
		t.recent(k, now)
	}
	if len(t.fails) < t.max {
		return
	}
	var oldest string
	var at time.Time
	for k, fs := range t.fails {
		if last := fs[len(fs)-1]; oldest == "" || last.Before(at) {
			oldest, at = k, last
		}
	}
	delete(t.fails, oldest)
}

// succeed forgets a login's and its account's failures once it signs in.
func (t *throttler) succeed(login, uid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, loginKey(login))
	delete(t.fails, userKey(uid))
}
