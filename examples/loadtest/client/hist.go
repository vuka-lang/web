package main

import (
	"math/bits"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// hist counts durations in buckets of an eighth of a power of two: quantiles
// within about 12%, with no lock and nothing kept per sample.
type hist struct {
	b [64 * 8]atomic.Int64
	n atomic.Int64
}

func bucketOf(ns int64) int {
	ns = max(ns, 8)
	e := bits.Len64(uint64(ns)) - 1
	return e*8 + int(ns>>(e-3))&7
}

func bucketUpper(b int) int64 {
	e, f := b/8, b%8
	return int64(9+f) << (e - 3)
}

func (h *hist) observe(d time.Duration) {
	h.b[bucketOf(int64(d))].Add(1)
	h.n.Add(1)
}

func (h *hist) result() map[string]float64 {
	out := map[string]float64{"count": float64(h.n.Load())}
	total := h.n.Load()
	if total == 0 {
		return out
	}
	for _, q := range quantileNames {
		var seen int64
		for i := range h.b {
			seen += h.b[i].Load()
			if float64(seen) >= q.at*float64(total) {
				out[q.name] = float64(bucketUpper(i)) / 1e6
				break
			}
		}
	}
	return out
}

var quantileNames = []struct {
	name string
	at   float64
}{{"p50", .5}, {"p90", .9}, {"p99", .99}, {"p999", .999}, {"max", 1}}

// latency is the exact quantiles of sorted durations, in ms.
func latency(all []time.Duration) map[string]float64 {
	out := map[string]float64{}
	if len(all) == 0 {
		return out
	}
	for _, q := range quantileNames {
		out[q.name] = float64(all[int(q.at*float64(len(all)-1))].Microseconds()) / 1000
	}
	return out
}

// quantilesOf is latency of values in ms.
func quantilesOf(ms []float64) map[string]float64 {
	out := map[string]float64{}
	if len(ms) == 0 {
		return out
	}
	s := slices.Clone(ms)
	slices.Sort(s)
	for _, q := range quantileNames {
		out[q.name] = s[int(q.at*float64(len(s)-1))]
	}
	return out
}

// lagProbe measures this process's own scheduling delay: a late timer here is
// a slow reply that isn't the server's.
type lagProbe struct {
	mu   sync.Mutex
	lags []time.Duration
	all  []time.Duration
}

func (l *lagProbe) run(done <-chan struct{}) {
	const period = 10 * time.Millisecond
	for {
		select {
		case <-done:
			return
		default:
		}
		at := time.Now()
		time.Sleep(period)
		d := max(0, time.Since(at)-period)
		l.mu.Lock()
		l.lags = append(l.lags, d)
		l.mu.Unlock()
	}
}

// take is the quantiles of the lags since the last take.
func (l *lagProbe) take() map[string]float64 {
	l.mu.Lock()
	s := l.lags
	l.lags = nil
	l.all = append(l.all, s...)
	l.mu.Unlock()
	slices.Sort(s)
	return latency(s)
}

// total is the quantiles of every lag taken, and those not yet.
func (l *lagProbe) total() map[string]float64 {
	l.mu.Lock()
	s := slices.Concat(l.all, l.lags)
	l.mu.Unlock()
	slices.Sort(s)
	return latency(s)
}

// recorder holds a window's event latencies, per user, so that taking one
// takes no lock.
type recorder struct {
	start time.Time
	n     atomic.Int64
	per   [][]time.Duration
	buc   []atomic.Int64 // per second of the window: events slower than slowAt
}

const slowAt = 100 * time.Millisecond

func newRecorder(users int, window time.Duration) *recorder {
	return &recorder{start: time.Now(), per: make([][]time.Duration, users), buc: make([]atomic.Int64, int(window/time.Second)+2)}
}

func (r *recorder) add(i int, d time.Duration) {
	r.per[i] = append(r.per[i], d)
	r.n.Add(1)
	if d >= slowAt {
		if s := int(time.Since(r.start) / time.Second); s >= 0 && s < len(r.buc) {
			r.buc[s].Add(1)
		}
	}
}

// slowSeconds is the seconds of the window with slow replies: when they came
// tells a periodic stall from a steady queue.
func (r *recorder) slowSeconds() map[string]int64 {
	out := map[string]int64{}
	for i := range r.buc {
		if n := r.buc[i].Load(); n > 0 {
			out[strconv.Itoa(i)] = n
		}
	}
	return out
}

func (r *recorder) summary(took time.Duration) map[string]any {
	all := slices.Concat(r.per...)
	slices.Sort(all)
	return map[string]any{
		"events": len(all), "events_per_second": float64(len(all)) / took.Seconds(),
		"latency_ms": latency(all), "slow_per_second": r.slowSeconds(),
	}
}

// bcastTable is the arrivals of the broadcasts of one phase, each indexed by
// its sequence number from base: how many pages got it and how long the first
// and the last took, in ns.
type bcastTable struct {
	base  int
	count []atomic.Int32
	first []atomic.Int64
	last  []atomic.Int64
}

func newBcastTable(base, k int) *bcastTable {
	t := &bcastTable{base: base, count: make([]atomic.Int32, k), first: make([]atomic.Int64, k), last: make([]atomic.Int64, k)}
	for i := range t.first {
		t.first[i].Store(1 << 62)
	}
	return t
}

func (t *bcastTable) arrive(seq int, lat int64) {
	i := seq - t.base
	if i < 0 || i >= len(t.count) {
		return
	}
	t.count[i].Add(1)
	for {
		old := t.first[i].Load()
		if lat >= old || t.first[i].CompareAndSwap(old, lat) {
			break
		}
	}
	for {
		old := t.last[i].Load()
		if lat <= old || t.last[i].CompareAndSwap(old, lat) {
			break
		}
	}
}
