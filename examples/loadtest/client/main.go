// Command client is the live-page load test's client: it behaves as the
// browser's runtime does, over plain WebSockets, and measures what the
// server's live runtime does at scale. It ramps connections up, then runs
// the phases it is given: interactive events, a broadcast to every subscriber
// of a topic (timed to the last), and a storm on one topic while other pages
// send events. Some users are slow: they stop reading. See ../README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	url             string
	ports           int
	scenario        string
	sockets, owners int
	rate            int
	every           time.Duration
	warmup, measure time.Duration
	compress        bool
	debug           string
	out, mark       string

	interactiveFrac, slowFrac float64
	slowMode                  string
	slowDelay, slowDrain      time.Duration
	rows                      int
	topic                     string

	step      int
	settle    time.Duration
	settleGC  bool
	phases    string
	bcasts    int
	bcastGap  time.Duration
	bcastWait time.Duration
	stormRate float64
	stormFor  time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "http://127.0.0.1:18090", "the app; with -ports, the first of its addresses")
	flag.IntVar(&o.ports, "ports", 1, "the app's consecutive ports from -url's, users dialling them in turn: the ephemeral ports of one address are about 16,000")
	flag.StringVar(&o.scenario, "scenario", "pg", "pg (the Postgres dashboard) or mem (/counter and /feed, no database)")
	flag.IntVar(&o.sockets, "sockets", 1000, "users, one socket each")
	flag.IntVar(&o.owners, "owners", 1000, "pg: the owners they view, in turn")
	flag.IntVar(&o.rate, "rate", 200, "users joining per second")
	flag.DurationVar(&o.every, "every", 5*time.Second, "one event per interactive user per this")
	flag.DurationVar(&o.warmup, "warmup", 30*time.Second, "time after the last join before measuring")
	flag.DurationVar(&o.measure, "measure", 2*time.Minute, "how long the events phase measures")
	flag.BoolVar(&o.compress, "compress", true, "offer permessage-deflate, as browsers do")
	flag.StringVar(&o.debug, "debug", "http://127.0.0.1:18099", "the server's debug address: its metrics are sampled and its broadcasts triggered there")
	flag.StringVar(&o.out, "out", "", "write the result as JSON here")
	flag.StringVar(&o.mark, "mark", "", "create this file when measuring starts")

	flag.Float64Var(&o.interactiveFrac, "interactive", -1, "the fraction of users sending events (default 1 for pg, 0.1 for mem)")
	flag.Float64Var(&o.slowFrac, "slow", -1, "the fraction of users that don't keep up with what they receive (default 0 for pg, 0.02 for mem)")
	flag.StringVar(&o.slowMode, "slow-mode", "delay", "delay (read one frame per -slow-delay) or stop (read nothing until the run drains them: the server drops them at its ping timeout)")
	flag.DurationVar(&o.slowDelay, "slow-delay", 15*time.Second, "delay mode: the wait between frames; the server gives up on a write blocked for 10s, so a longer one ends in a close once the buffers are full")
	flag.DurationVar(&o.slowDrain, "slow-drain", 5*time.Second, "slow-mode stop: how long a drained user waits for a frame before it counts as still open")
	flag.IntVar(&o.rows, "rows", 20, "mem: the rows of each page")
	flag.StringVar(&o.topic, "topic", "feed", "mem: the topic the feed pages subscribe to and the phases broadcast on")

	flag.IntVar(&o.step, "step", 1000, "ramp: users joined per step")
	flag.DurationVar(&o.settle, "settle", 10*time.Second, "ramp: wait after each step before sampling")
	flag.BoolVar(&o.settleGC, "settle-gc", true, "ramp: also sample after the server collects and returns memory")
	flag.StringVar(&o.phases, "phases", "", "after the ramp, comma-separated: events, broadcast, storm (default events for pg, all for mem)")
	flag.IntVar(&o.bcasts, "broadcasts", 20, "broadcast phase: how many, one after another")
	flag.DurationVar(&o.bcastGap, "broadcast-gap", 500*time.Millisecond, "broadcast phase: the wait after each is complete")
	flag.DurationVar(&o.bcastWait, "broadcast-wait", 15*time.Second, "broadcast phase: how long one waits for its subscribers")
	flag.Float64Var(&o.stormRate, "storm-rate", 10, "storm phase: broadcasts per second")
	flag.DurationVar(&o.stormFor, "storm-for", 20*time.Second, "storm phase: how long")
	flag.Parse()
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

type tally [3]*bucket

// bucket is the closes (or failures) of one class: how many by status, and
// when in the run, in seconds.
type bucket struct {
	by map[string]int
	at []float64
}

type runner struct {
	o       options
	ctx     context.Context
	targets []*url.URL
	httpc   *http.Client
	slowc   *http.Client
	dbg     *http.Client
	classes []class
	wg      sync.WaitGroup

	joined, failed, ended, replyErrs atomic.Int64
	alive                            [3]atomic.Int64
	eventsOn                         atomic.Bool
	rec                              atomic.Pointer[recorder]
	sub                              atomic.Pointer[hist]
	bt                               atomic.Pointer[bcastTable]
	subFrames, subBytes              atomic.Int64
	lastSeq                          []atomic.Int32
	resume                           chan struct{}
	slowOpen, slowFrames             atomic.Int64
	slowResolved                     atomic.Int64

	t0       time.Time
	mu       sync.Mutex
	closes   tally
	failures tally
	timeline map[string]float64
}

// mark names the moment a phase begins or ends, in seconds since the start.
func (r *runner) mark(name string) {
	r.mu.Lock()
	r.timeline[name] = time.Since(r.t0).Seconds()
	r.mu.Unlock()
}

func (r *runner) note(t *tally, cl class, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t[cl] == nil {
		t[cl] = &bucket{by: map[string]int{}}
	}
	t[cl].by[reason(err)]++
	t[cl].at = append(t[cl].at, time.Since(r.t0).Seconds())
}

func (t *tally) result() map[string]any {
	out := map[string]any{}
	for cl, b := range t {
		if b == nil {
			b = &bucket{by: map[string]int{}}
		}
		var n int
		for _, c := range b.by {
			n += c
		}
		out[classNames[cl]] = map[string]any{"total": n, "by_status": b.by, "at_seconds": quantilesOf(b.at)}
	}
	return out
}

func (r *runner) count(cl class) int {
	n := 0
	for _, c := range r.classes {
		if c == cl {
			n++
		}
	}
	return n
}

func (r *runner) total() int64 {
	var n int64
	for i := range r.alive {
		n += r.alive[i].Load()
	}
	return n
}

func (r *runner) get(path string) map[string]any { return fetch(r.dbg, r.o.debug+path) }
func (r *runner) post(path string) map[string]any {
	resp, err := r.dbg.Post(r.o.debug+path, "text/plain", nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	m := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// assign deals the classes out in a fixed random order, so every step of the
// ramp has the same mix.
func assign(n int, inter, slowFrac float64) []class {
	nI := min(n, int(math.Round(float64(n)*inter)))
	nS := min(n-nI, int(math.Round(float64(n)*slowFrac)))
	cs := make([]class, n)
	for i := range cs {
		switch {
		case i < nI:
			cs[i] = interactive
		case i < nI+nS:
			cs[i] = slow
		}
	}
	rand.New(rand.NewPCG(1, 2)).Shuffle(n, func(i, j int) { cs[i], cs[j] = cs[j], cs[i] })
	return cs
}

func (r *runner) has(phase string) bool {
	return slices.Contains(strings.Split(r.o.phases, ","), phase)
}

func run(o options) error {
	mem := o.scenario == "mem"
	if o.scenario != "pg" && !mem {
		return fmt.Errorf("unknown scenario %q", o.scenario)
	}
	if o.interactiveFrac < 0 {
		o.interactiveFrac = map[bool]float64{true: 0.1, false: 1}[mem]
	}
	if o.slowFrac < 0 {
		o.slowFrac = map[bool]float64{true: 0.02, false: 0}[mem]
	}
	if o.phases == "" {
		o.phases = map[bool]string{true: "events,broadcast,storm", false: "events"}[mem]
	}
	o.step = max(1, o.step)
	if err := raiseNofile(o.sockets); err != nil {
		return err
	}
	base, err := url.Parse(o.url)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	r := &runner{
		o: o, ctx: ctx,
		httpc:   newHTTPClient(0),
		slowc:   newHTTPClient(4096),
		dbg:     &http.Client{Timeout: 2 * time.Minute},
		classes: assign(o.sockets, o.interactiveFrac, o.slowFrac),
		lastSeq: make([]atomic.Int32, o.sockets),
		resume:  make(chan struct{}),
		t0:      time.Now(), timeline: map[string]float64{},
	}
	host := base.Hostname()
	port, _ := strconv.Atoi(base.Port())
	for i := range max(1, o.ports) {
		u := *base
		u.Host = host + ":" + strconv.Itoa(port+i)
		r.targets = append(r.targets, &u)
	}

	res := map[string]any{"machine": machine(), "sockets": o.sockets, "classes": map[string]int{}}
	for cl := range classNames {
		res["classes"].(map[string]int)[classNames[cl]] = r.count(class(cl))
	}
	cfg := map[string]string{}
	flag.VisitAll(func(f *flag.Flag) { cfg[f.Name] = f.Value.String() })
	res["config"] = cfg

	lag := &lagProbe{}
	lagDone := make(chan struct{})
	defer close(lagDone)
	go lag.run(lagDone)

	r.mark("ramp_start")
	if err := r.ramp(res); err != nil {
		return err
	}
	res["ramp"].(map[string]any)["client_lag_ms"] = lag.take()
	r.mark("ramp_end")

	r.eventsOn.Store(true)
	log.Printf("warming up %s", o.warmup)
	time.Sleep(o.warmup)
	if o.mark != "" {
		os.WriteFile(o.mark, nil, 0o644)
	}
	if r.has("events") && r.count(interactive) > 0 {
		maps.Copy(res, r.eventsPhase(lag))
	}
	if r.has("broadcast") && mem && r.count(normal) > 0 {
		res["broadcast"] = r.broadcastPhase(lag)
	}
	if r.has("storm") && mem && r.count(normal) > 0 {
		res["storm"] = r.stormPhase(lag)
	}
	res["slow"] = r.drainSlow()

	res["joined"], res["failed"], res["reply_errors"] = r.joined.Load(), r.failed.Load(), r.replyErrs.Load()
	res["timeline"] = r.timelineResult()
	res["closes"], res["failures"] = r.closeResult(), r.failureResult()
	res["client"] = map[string]any{"lag_ms": lag.total(), "self": r.self(), "gomaxprocs": runtime.GOMAXPROCS(0)}
	res["server_end"] = r.get("/debug/metrics?mark=1")
	b, _ := json.MarshalIndent(res, "", "  ")
	if o.out != "" {
		if err := os.WriteFile(o.out, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(r.summaryLine(res))
	stop()
	return nil
}

func (r *runner) timelineResult() map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.timeline)
}

func (r *runner) closeResult() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes.result()
}

func (r *runner) failureResult() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures.result()
}

// self is this process's own memory: a client that's the bottleneck shows here.
func (r *runner) self() map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]any{
		"rss_bytes": rssBytes(os.Getpid()), "heap_inuse": m.HeapInuse, "stack_inuse": m.StackInuse,
		"goroutines": runtime.NumGoroutine(), "num_gc": m.NumGC,
	}
}

// ramp joins the users a step at a time, sampling the server after each
// settles: the memory a connection costs is the slope.
func (r *runner) ramp(res map[string]any) error {
	var steps []map[string]any
	var ns, rss, rssRaw, heap, goro, cliRSS []float64
	record := func(raw, settled map[string]any) {
		n := float64(r.total())
		steps = append(steps, map[string]any{
			"connections": int64(n), "joined": r.joined.Load(), "failed": r.failed.Load(),
			"server_raw": raw, "server_settled": settled, "client": r.self(),
		})
		if settled == nil {
			settled = raw
		}
		ns = append(ns, n)
		rss = append(rss, num(settled, "rss_bytes"))
		rssRaw = append(rssRaw, num(raw, "rss_bytes"))
		heap = append(heap, num(settled, "heap_inuse")+num(settled, "stack_inuse"))
		goro = append(goro, num(settled, "goroutines"))
		cliRSS = append(cliRSS, float64(rssBytes(os.Getpid())))
	}
	sample := func() {
		raw := r.get("/debug/metrics?mark=1")
		var settled map[string]any
		if r.o.settleGC {
			settled = r.get("/debug/metrics?gc=1")
		}
		record(raw, settled)
	}
	sample()
	start := time.Now()
	for launched := 0; launched < r.o.sockets; {
		to := min(launched+r.o.step, r.o.sockets)
		took := r.launch(launched, to)
		launched = to
		log.Printf("%d launched, %d joined, %d failed (step took %s); settling %s", to, r.joined.Load(), r.failed.Load(), took.Round(time.Millisecond), r.o.settle)
		time.Sleep(r.o.settle)
		sample()
		steps[len(steps)-1]["step_seconds"] = took.Seconds()
	}
	last := len(ns) - 1
	per := func(y []float64) map[string]float64 {
		out := map[string]float64{"slope": slope(ns, y)}
		if last > 0 && ns[last] > ns[0] {
			out["last_minus_first"] = (y[last] - y[0]) / (ns[last] - ns[0])
		}
		return out
	}
	res["ramp"] = map[string]any{
		"seconds": time.Since(start).Seconds(),
		"steps":   steps,
		"bytes_per_connection": map[string]any{
			"server_rss_settled":    per(rss),
			"server_rss_raw":        per(rssRaw),
			"server_heap_and_stack": per(heap),
			"server_goroutines":     per(goro),
			"client_rss":            per(cliRSS),
			"note":                  "slope: least squares over the steps, the empty server included; goroutines is a count per connection, not bytes",
		},
	}
	return nil
}

// launch starts users from..to-1, o.rate a second, and waits until each has
// joined or failed.
func (r *runner) launch(from, to int) time.Duration {
	start := time.Now()
	gate := time.NewTicker(time.Second / time.Duration(max(1, r.o.rate)))
	defer gate.Stop()
	for i := from; i < to; i++ {
		<-gate.C
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.user(i)
		}()
	}
	for r.joined.Load()+r.failed.Load() < int64(to) && r.ctx.Err() == nil {
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start)
}

// sampling samples the server every period until d has passed.
func (r *runner) sampling(d, every time.Duration) []map[string]any {
	var out []map[string]any
	t0 := time.Now()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for time.Since(t0) < d {
		select {
		case <-tick.C:
			out = append(out, r.get("/debug/metrics?mark=1"))
		case <-r.ctx.Done():
			return out
		}
	}
	return out
}

func (r *runner) eventsPhase(lag *lagProbe) map[string]any {
	rec := newRecorder(r.o.sockets, r.o.measure)
	r.mark("events_start")
	defer r.mark("events_end")
	r.eventsOn.Store(true)
	lag.take()
	before := r.get("/debug/metrics?mark=1")
	r.rec.Store(rec)
	t0 := time.Now()
	samples := r.sampling(r.o.measure, 10*time.Second)
	r.rec.Store(nil)
	took := time.Since(t0)
	time.Sleep(200 * time.Millisecond)
	out := rec.summary(took)
	out["measure_seconds"] = took.Seconds()
	out["client_lag_ms"] = lag.take()
	out["server_before"], out["server_after"], out["server_samples"] = before, r.get("/debug/metrics?mark=1"), samples
	log.Printf("events: %v", out["latency_ms"])
	return out
}

func ms(ns float64) float64 { return math.Round(ns/1e4) / 100 }

// broadcastPhase sends o.bcasts broadcasts one at a time, each waiting for
// every subscriber that's still connected, and times each to the first and
// to the last of them.
func (r *runner) broadcastPhase(lag *lagProbe) map[string]any {
	r.eventsOn.Store(false)
	r.mark("broadcast_start")
	defer r.mark("broadcast_end")
	time.Sleep(2 * time.Second)
	seq0 := int(num(r.get("/debug/seq"), "seq")) + 1
	tab := newBcastTable(seq0, r.o.bcasts)
	h := &hist{}
	r.sub.Store(h)
	r.bt.Store(tab)
	lag.take()
	before := r.get("/debug/metrics?mark=1")
	var per []map[string]any
	var lasts, firsts []float64
	complete := 0
	for k := range r.o.bcasts {
		expected := int(r.alive[normal].Load())
		resp := r.post("/debug/broadcast?topic=" + url.QueryEscape(r.o.topic))
		deadline := time.Now().Add(r.o.bcastWait)
		for int(tab.count[k].Load()) < expected && time.Now().Before(deadline) && r.ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		got := int(tab.count[k].Load())
		e := map[string]any{"seq": int(num(resp, "seq")), "expected": expected, "delivered": got, "dispatch_ms": num(resp, "dispatch_us") / 1000}
		if got > 0 {
			f, l := float64(tab.first[k].Load()), float64(tab.last[k].Load())
			e["first_ms"], e["last_ms"] = ms(f), ms(l)
			firsts, lasts = append(firsts, ms(f)), append(lasts, ms(l))
		}
		if got >= expected {
			complete++
		}
		per = append(per, e)
		time.Sleep(r.o.bcastGap)
	}
	r.bt.Store(nil)
	return map[string]any{
		"broadcasts": r.o.bcasts, "complete": complete, "subscribers": r.alive[normal].Load(),
		"to_last_ms": quantilesOf(lasts), "to_first_ms": quantilesOf(firsts),
		"per_subscriber_ms": h.result(), "per_broadcast": per,
		"client_lag_ms": lag.take(), "server_before": before, "server_after": r.get("/debug/metrics?mark=1"),
	}
}

// stormPhase broadcasts on the topic at a steady rate while the interactive
// users send events, which are timed apart from the subscribers' frames.
func (r *runner) stormPhase(lag *lagProbe) map[string]any {
	for i := range r.lastSeq {
		r.lastSeq[i].Store(0)
	}
	r.mark("storm_start")
	defer r.mark("storm_end")
	rec := newRecorder(r.o.sockets, r.o.stormFor+10*time.Second)
	h, disp := &hist{}, &hist{}
	r.sub.Store(h)
	r.subFrames.Store(0)
	r.subBytes.Store(0)
	r.eventsOn.Store(true)
	lag.take()
	before := r.get("/debug/metrics?mark=1")
	subs := r.alive[normal].Load()
	r.rec.Store(rec)
	t0 := time.Now()
	var sent, failed int
	var lastSeq int
	var samples []map[string]any
	tick := time.NewTicker(time.Duration(float64(time.Second) / max(r.o.stormRate, 0.001)))
	defer tick.Stop()
	sampleTick := time.NewTicker(5 * time.Second)
	defer sampleTick.Stop()
	for time.Since(t0) < r.o.stormFor && r.ctx.Err() == nil {
		select {
		case <-tick.C:
			resp := r.post("/debug/broadcast?topic=" + url.QueryEscape(r.o.topic))
			if _, bad := resp["error"]; bad || resp["seq"] == nil {
				failed++
				continue
			}
			sent++
			lastSeq = int(num(resp, "seq"))
			disp.observe(time.Duration(num(resp, "dispatch_us") * 1000))
		case <-sampleTick.C:
			samples = append(samples, r.get("/debug/metrics?mark=1"))
		}
	}
	took := time.Since(t0)
	time.Sleep(3 * time.Second)
	r.rec.Store(nil)
	time.Sleep(200 * time.Millisecond)

	var behind []float64
	caught, silent := 0, 0
	for i, c := range r.classes {
		if c != normal {
			continue
		}
		s := int(r.lastSeq[i].Load())
		switch {
		case s == 0:
			silent++
		case s >= lastSeq:
			caught++
		default:
			behind = append(behind, float64(lastSeq-s))
		}
	}
	frames := r.subFrames.Load()
	out := map[string]any{
		"seconds": took.Seconds(), "rate_target": r.o.stormRate, "rate_achieved": float64(sent) / took.Seconds(),
		"broadcasts_sent": sent, "broadcasts_failed": failed, "subscribers": subs,
		"frames_received": frames, "frame_bytes_avg": float64(r.subBytes.Load()) / math.Max(1, float64(frames)), "frames_per_broadcast_per_subscriber": float64(frames) / math.Max(1, float64(sent)*float64(subs)),
		"subscribers_caught_up": caught, "subscribers_no_frame": silent, "subscribers_behind_by_seq": quantilesOf(behind),
		"subscriber_latency_ms": h.result(), "dispatch_ms": disp.result(),
		"interactive": rec.summary(took), "client_lag_ms": lag.take(),
		"server_before": before, "server_after": r.get("/debug/metrics?mark=1"), "server_samples": samples,
	}
	log.Printf("storm: interactive %v, subscribers %v", out["interactive"].(map[string]any)["latency_ms"], out["subscriber_latency_ms"])
	return out
}

// drainSlow lets the slow users read what the server queued for them, and
// finds which it closed.
func (r *runner) drainSlow() map[string]any {
	out := map[string]any{"clients": r.count(slow), "mode": r.o.slowMode}
	if r.count(slow) == 0 {
		return out
	}
	r.mark("drain_start")
	out["server_live_conns_at_drain"] = num(r.get("/debug/metrics"), "live_conns")
	want := r.alive[slow].Load()
	close(r.resume)
	if r.o.scenario == "mem" {
		r.post("/debug/broadcast?topic=" + url.QueryEscape(r.o.topic))
	}
	deadline := time.Now().Add(r.o.slowDrain + 5*time.Second)
	for r.slowResolved.Load() < want && r.alive[slow].Load() > 0 && time.Now().Before(deadline) && r.ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
	}
	r.mu.Lock()
	closed := 0
	if b := r.closes[slow]; b != nil {
		for _, n := range b.by {
			closed += n
		}
	}
	r.mu.Unlock()
	out["still_open"], out["closed"], out["frames_read"] = r.slowOpen.Load()+r.alive[slow].Load(), closed, r.slowFrames.Load()
	return out
}

func (r *runner) summaryLine(res map[string]any) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("sockets=%d joined=%d failed=%d", r.o.sockets, r.joined.Load(), r.failed.Load()))
	if l, ok := res["latency_ms"]; ok {
		parts = append(parts, fmt.Sprintf("events/s=%.0f latency_ms=%v", res["events_per_second"], l))
	}
	if b, ok := res["broadcast"].(map[string]any); ok {
		parts = append(parts, fmt.Sprintf("to_last_ms=%v", b["to_last_ms"]))
	}
	if s, ok := res["storm"].(map[string]any); ok {
		parts = append(parts, fmt.Sprintf("storm_interactive_ms=%v", s["interactive"].(map[string]any)["latency_ms"]))
	}
	parts = append(parts, fmt.Sprintf("reply_errors=%d client_lag_ms=%v", r.replyErrs.Load(), res["client"].(map[string]any)["lag_ms"]))
	return strings.Join(parts, " ")
}
