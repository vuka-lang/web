package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vuka-lang/web/v2"
	"github.com/vuka-lang/web/v2/examples/loadtest/feed"
	"github.com/vuka-lang/web/v2/examples/loadtest/store"
)

var (
	liveOpen, liveTotal atomic.Int64
	broadcasts          atomic.Int64
)

// countLive counts the live sockets the app holds: a connection's request is
// in flight for as long as the socket is open.
func countLive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_vuka/live" {
			liveOpen.Add(1)
			liveTotal.Add(1)
			defer liveOpen.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

func serveDebug(addr string, app *web.App) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/stats", stats)
	mux.HandleFunc("/debug/metrics", stats)
	mux.HandleFunc("/debug/seq", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"seq": broadcasts.Load()})
	})
	mux.HandleFunc("/debug/broadcast", func(w http.ResponseWriter, r *http.Request) {
		topic := r.URL.Query().Get("topic")
		if topic == "" {
			topic = feed.Topic
		}
		seq := int(broadcasts.Add(1))
		at := time.Now()
		err := app.Broadcast(r.Context(), topic, feed.Tick{Seq: seq, At: at.UnixNano()})
		out := map[string]any{"seq": seq, "at": at.UnixNano(), "dispatch_us": time.Since(at).Microseconds()}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(w, out)
	})
	log.Println(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// rssBytes is the process's resident set.
func rssBytes() int64 {
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 1 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n * int64(os.Getpagesize())
		}
	}
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n * 1024
}

var (
	markMu   sync.Mutex
	markGC   *metrics.Float64Histogram
	gcMetric = "/sched/pauses/total/gc:seconds"
)

// stats is the numbers each sample of the client's run records. ?gc=1 runs a
// collection and returns the memory it frees first, so the sample is of live
// data; ?mark=1 adds gc_pause_window_ms, the pauses since the last mark.
func stats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("gc") == "1" {
		debug.FreeOSMemory()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	out := map[string]any{
		"rss_bytes":       rssBytes(),
		"live_conns":      liveOpen.Load(),
		"live_accepted":   liveTotal.Load(),
		"broadcasts":      broadcasts.Load(),
		"goroutines":      runtime.NumGoroutine(),
		"gomaxprocs":      runtime.GOMAXPROCS(0),
		"heap_alloc":      m.HeapAlloc,
		"heap_inuse":      m.HeapInuse,
		"heap_sys":        m.HeapSys,
		"heap_idle":       m.HeapIdle,
		"heap_released":   m.HeapReleased,
		"heap_objects":    m.HeapObjects,
		"stack_inuse":     m.StackInuse,
		"sys":             m.Sys,
		"next_gc":         m.NextGC,
		"num_gc":          m.NumGC,
		"pause_total_ms":  float64(m.PauseTotalNs) / 1e6,
		"gc_cpu_fraction": m.GCCPUFraction,
		"db_ms":           store.DBTime.Snapshot(),
		"check_ms":        store.CheckTime.Snapshot(),
	}
	for name, key := range map[string]string{
		"/sched/latencies:seconds":          "sched_latency_ms",
		"/sched/pauses/total/gc:seconds":    "gc_pause_ms",
		"/sync/mutex/wait/total:seconds":    "",
		"/cpu/classes/gc/total:cpu-seconds": "",
		"/cpu/classes/total:cpu-seconds":    "",
	} {
		s := []metrics.Sample{{Name: name}}
		metrics.Read(s)
		switch s[0].Value.Kind() {
		case metrics.KindFloat64Histogram:
			if key != "" {
				out[key] = quantiles(s[0].Value.Float64Histogram(), nil)
			}
		case metrics.KindFloat64:
			out[strings.NewReplacer("/", "_", ":", "_").Replace(strings.TrimPrefix(name, "/"))] = s[0].Value.Float64()
		}
	}
	if r.URL.Query().Get("mark") == "1" {
		s := []metrics.Sample{{Name: gcMetric}}
		metrics.Read(s)
		h := s[0].Value.Float64Histogram()
		markMu.Lock()
		out["gc_pause_window_ms"] = quantiles(h, markGC)
		markGC = h
		markMu.Unlock()
	}
	writeJSON(w, out)
}

// quantiles are the histogram's quantiles in ms (upper bounds of buckets),
// over what it counted since since, when given.
func quantiles(h, since *metrics.Float64Histogram) map[string]float64 {
	counts := make([]uint64, len(h.Counts))
	var total uint64
	for i, c := range h.Counts {
		if since != nil && i < len(since.Counts) {
			c -= min(c, since.Counts[i])
		}
		counts[i] = c
		total += c
	}
	out := map[string]float64{"count": float64(total)}
	for _, q := range []struct {
		name string
		at   float64
	}{{"p50", .5}, {"p99", .99}, {"p999", .999}, {"p9999", .9999}, {"max", 1}} {
		var seen uint64
		for i, c := range counts {
			seen += c
			if float64(seen) >= q.at*float64(total) && total > 0 {
				out[q.name] = h.Buckets[i+1] * 1000
				break
			}
		}
	}
	return out
}
