// Command client is the live-page load test's client: it behaves as the
// browser's runtime does, over plain WebSockets. Each user loads the page for
// its token and the refresh button's handler id, joins, and then clicks once
// per period, timing each reply by its ref. See ../README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	tokenRe  = regexp.MustCompile(`data-vk-token="([^"]+)"`)
	targetRe = regexp.MustCompile(`<button[^>]*id="refresh"[^>]*>`)
	onClick  = regexp.MustCompile(`data-vk-on-click="([^"]+)"`)
)

type options struct {
	url             string
	sockets, owners int
	rate            int
	every           time.Duration
	warmup, measure time.Duration
	compress        bool
	statsURL        string
	out             string
	mark            string
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "http://127.0.0.1:18090", "the app")
	flag.IntVar(&o.sockets, "sockets", 1000, "users, one socket each")
	flag.IntVar(&o.owners, "owners", 1000, "the owners they view, in turn")
	flag.IntVar(&o.rate, "rate", 200, "users joining per second")
	flag.DurationVar(&o.every, "every", 5*time.Second, "one event per user per this")
	flag.DurationVar(&o.warmup, "warmup", 30*time.Second, "time after the last join before measuring")
	flag.DurationVar(&o.measure, "measure", 2*time.Minute, "how long to measure")
	flag.BoolVar(&o.compress, "compress", true, "offer permessage-deflate, as browsers do")
	flag.StringVar(&o.statsURL, "stats", "http://127.0.0.1:18091/debug/stats", "the server's stats, sampled during the run")
	flag.StringVar(&o.out, "out", "", "write the result as JSON here")
	flag.StringVar(&o.mark, "mark", "", "create this file when measuring starts")
	flag.Parse()
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

// recorder holds the measuring window's latencies, per user, so that taking
// one takes no lock.
type recorder struct {
	on  atomic.Bool
	n   atomic.Int64
	per [][]time.Duration
	buc []atomic.Int64 // per second of the window: events slower than slowAt
}

const slowAt = 100 * time.Millisecond

func run(o options) error {
	tr := &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64, DisableCompression: true}
	httpc := &http.Client{Transport: tr, Timeout: time.Minute}
	rec := &recorder{per: make([][]time.Duration, o.sockets), buc: make([]atomic.Int64, int(o.measure/time.Second)+2)}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	var joined, failed, replyErrs, closes atomic.Int64
	var wg sync.WaitGroup
	joinStart := time.Now()
	gate := time.NewTicker(time.Second / time.Duration(max(1, o.rate)))
	defer gate.Stop()
	var start atomic.Int64 // the window's start, unix ns
	for i := range o.sockets {
		<-gate.C
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := user(ctx, httpc, o, i, rec, &start, &joined, &replyErrs)
			if ctx.Err() == nil {
				if joined.Load() >= 0 && err != nil {
					closes.Add(1)
					failed.Add(1)
					if failed.Load() <= 5 {
						log.Printf("user %d: %v", i, err)
					}
				}
			}
		}()
	}
	for joined.Load()+failed.Load() < int64(o.sockets) {
		time.Sleep(100 * time.Millisecond)
	}
	joinTook := time.Since(joinStart)
	log.Printf("%d joined, %d failed in %s; warming up %s", joined.Load(), failed.Load(), joinTook.Round(time.Millisecond), o.warmup)
	time.Sleep(o.warmup)

	if o.mark != "" {
		os.WriteFile(o.mark, nil, 0o644)
	}
	lag := &lagProbe{}
	go lag.run(ctx)
	before := sample(httpc, o.statsURL)
	start.Store(time.Now().UnixNano())
	rec.on.Store(true)
	var samples []map[string]any
	t0 := time.Now()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for time.Since(t0) < o.measure {
		<-tick.C
		samples = append(samples, sample(httpc, o.statsURL))
		log.Printf("%s in: %d events so far", time.Since(t0).Round(time.Second), rec.count())
	}
	rec.on.Store(false)
	after := sample(httpc, o.statsURL)
	took := time.Since(t0)

	all := slices.Concat(rec.per...)
	slices.Sort(all)
	res := map[string]any{
		"sockets": o.sockets, "joined": joined.Load(), "failed": failed.Load(), "reply_errors": replyErrs.Load(),
		"join_seconds": joinTook.Seconds(), "measure_seconds": took.Seconds(),
		"events": len(all), "events_per_second": float64(len(all)) / took.Seconds(),
		"latency_ms":      latency(all),
		"client":          map[string]any{"lag_ms": lag.result(), "gomaxprocs": runtime.GOMAXPROCS(0)},
		"slow_per_second": rec.slowSeconds(),
		"server_before":   before, "server_after": after, "server_samples": samples,
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if o.out != "" {
		if err := os.WriteFile(o.out, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("sockets=%d joined=%d failed=%d events/s=%.0f latency_ms=%v reply_errors=%d client_lag_ms=%v\n",
		o.sockets, joined.Load(), failed.Load(), float64(len(all))/took.Seconds(), res["latency_ms"], replyErrs.Load(), lag.result())
	stop()
	return nil
}

func (r *recorder) count() int64 { return r.n.Load() }

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

func latency(all []time.Duration) map[string]float64 {
	out := map[string]float64{}
	if len(all) == 0 {
		return out
	}
	for _, q := range []struct {
		name string
		at   float64
	}{{"p50", .5}, {"p90", .9}, {"p99", .99}, {"p999", .999}} {
		out[q.name] = float64(all[int(q.at*float64(len(all)-1))].Microseconds()) / 1000
	}
	out["max"] = float64(all[len(all)-1].Microseconds()) / 1000
	return out
}

func sample(c *http.Client, u string) map[string]any {
	if u == "" {
		return nil
	}
	resp, err := c.Get(u)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// user is one browser tab: GET, join, then a click per period, its first at a
// random point of the period so the users spread out.
func user(ctx context.Context, c *http.Client, o options, i int, rec *recorder, start *atomic.Int64, joined, replyErrs *atomic.Int64) error {
	base, err := url.Parse(o.url)
	if err != nil {
		return err
	}
	page := *base
	page.Path = "/"
	page.RawQuery = "u=" + strconv.Itoa(1+i%o.owners)
	token, target, cookie, err := load(ctx, c, page.String())
	if err != nil {
		return err
	}
	ws := *base
	ws.Scheme = strings.Replace(base.Scheme, "http", "ws", 1)
	ws.Path = "/_vuka/live"
	ws.RawQuery = "t=" + url.QueryEscape(token)
	dialCtx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()
	dopts := &websocket.DialOptions{HTTPClient: c, HTTPHeader: http.Header{}}
	if cookie != "" {
		dopts.HTTPHeader.Set("Cookie", cookie)
	}
	if o.compress {
		dopts.CompressionMode = websocket.CompressionNoContextTakeover
	}
	conn, _, err := websocket.Dial(dialCtx, ws.String(), dopts)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 24)

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"join","ref":1,"v":2}`)); err != nil {
		return err
	}
	var rep reply
	if err := readReply(ctx, conn, &rep); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	if rep.Type != "render" {
		return fmt.Errorf("join answered %q: %s", rep.Type, rep.Error)
	}
	joined.Add(1)

	// One goroutine reads (and so answers the server's pings), this one writes.
	var pending sync.Map // ref → time sent
	go func() {
		for {
			var m reply
			if err := readReply(ctx, conn, &m); err != nil {
				return
			}
			got := time.Now()
			if m.Ref == 0 {
				continue
			}
			v, ok := pending.LoadAndDelete(m.Ref)
			if !ok {
				continue
			}
			if m.Error != "" || m.Type == "error" {
				replyErrs.Add(1)
				continue
			}
			if rec.on.Load() {
				d := got.Sub(v.(time.Time))
				rec.per[i] = append(rec.per[i], d)
				rec.n.Add(1)
				if d >= slowAt {
					if s := int((got.UnixNano() - start.Load()) / 1e9); s >= 0 && s < len(rec.buc) {
						rec.buc[s].Add(1)
					}
				}
			}
		}
	}()

	first := time.Duration(rand.Int64N(int64(o.every)))
	select {
	case <-time.After(first):
	case <-ctx.Done():
		return nil
	}
	tick := time.NewTicker(o.every)
	defer tick.Stop()
	ref := 1
	var msg bytes.Buffer
	for {
		ref++
		msg.Reset()
		fmt.Fprintf(&msg, `{"type":"event","target":%q,"event":"click","ref":%d}`, target, ref)
		pending.Store(ref, time.Now())
		if err := conn.Write(ctx, websocket.MessageText, msg.Bytes()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("event: %w", err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil
		}
	}
}

type reply struct {
	Type  string `json:"type"`
	Ref   int    `json:"ref"`
	Error string `json:"error"`
}

func readReply(ctx context.Context, c *websocket.Conn, into *reply) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// load GETs the page for its join token and the refresh button's handler.
func load(ctx context.Context, c *http.Client, u string) (token, target, cookie string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("GET: %s", resp.Status)
	}
	m := tokenRe.FindSubmatch(body)
	if m == nil {
		return "", "", "", errors.New("the page has no data-vk-token")
	}
	b := targetRe.Find(body)
	if b == nil {
		return "", "", "", errors.New(`the page has no <button id="refresh">`)
	}
	h := onClick.FindSubmatch(b)
	if h == nil {
		return "", "", "", errors.New("the refresh button has no handler")
	}
	for _, ck := range resp.Cookies() {
		if cookie != "" {
			cookie += "; "
		}
		cookie += ck.Name + "=" + ck.Value
	}
	return string(m[1]), string(h[1]), cookie, nil
}

// lagProbe measures this process's own scheduling delay: a late timer here is
// a slow reply that isn't the server's.
type lagProbe struct {
	mu   sync.Mutex
	lags []time.Duration
}

func (l *lagProbe) run(ctx context.Context) {
	const period = 10 * time.Millisecond
	for ctx.Err() == nil {
		at := time.Now()
		time.Sleep(period)
		d := max(0, time.Since(at)-period)
		l.mu.Lock()
		l.lags = append(l.lags, d)
		l.mu.Unlock()
	}
}

func (l *lagProbe) result() map[string]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := slices.Clone(l.lags)
	slices.Sort(s)
	return latency(s)
}
