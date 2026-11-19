package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vuka-lang/ui/v2/live"
)

func holdGate(t *testing.T, permits int) (release func()) {
	broadcastGate.reset(permits)
	var rels []func()
	for range permits {
		rels = append(rels, broadcastGate.acquire(context.Background()))
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, r := range rels {
				r()
			}
		})
	}
	t.Cleanup(func() { release(); broadcastGate.reset(0) })
	return release
}

// Events of pages whose renders don't wait for the gate are answered while
// every permit of it is taken.
func TestLiveInteractiveEventsSkipTheRenderGate(t *testing.T) {
	release := holdGate(t, 2)
	app := liveApp()
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	drained(t, app)
	listener, _ := joined(t, srv, "/chat/go")
	counter, html := joined(t, srv, "/counter")
	app.Broadcast(context.Background(), "room:go", said{"hi"})
	for i := range 20 {
		send(t, counter, map[string]any{"type": "event", "ref": 10 + i, "target": handlerOf(t, html, "click", 0), "event": "click"})
		if m, err := recv(counter, time.Second); err != nil || m.Ref != 10+i || m.Type != live.PatchMsg {
			t.Fatalf("event %d under a storm: %+v, %v", i, m, err)
		}
	}
	release()
	if m := must(t, listener); !strings.Contains(logOf(m), "hi") {
		t.Fatalf("the broadcast after the release: %+v", m)
	}
}

func TestGateBoundsConcurrentHolders(t *testing.T) {
	var g gate
	g.reset(3)
	var mu sync.Mutex
	cur, peak := 0, 0
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			rel := g.acquire(context.Background())
			mu.Lock()
			cur++
			peak = max(peak, cur)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			rel()
		})
	}
	wg.Wait()
	if peak != 3 {
		t.Fatalf("peak holders %d, want 3", peak)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var h gate
	h.reset(1)
	h.acquire(context.Background())
	if h.acquire(ctx) != nil {
		t.Fatal("a canceled wait got a permit")
	}
}
