package web

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

func testConn() *conn { return &conn{wake: make(chan struct{}, 1)} }

func TestHubShardsSpreadKeys(t *testing.T) {
	h := newHub()
	hits := map[*hubShard]int{}
	for i := range 6400 {
		hits[h.shard(fmt.Sprintf("room:%d", i))]++
	}
	if len(hits) != hubShards {
		t.Fatalf("keys reached %d of %d shards", len(hits), hubShards)
	}
	for _, n := range hits {
		if n < 40 || n > 200 {
			t.Fatalf("a shard holds %d of 6400 keys (mean 100)", n)
		}
	}
	if h.shard("room:1") != h.shard("room:1") {
		t.Fatal("a key moved between shards")
	}
}

func TestHubAdmitCapsHoldUnderConcurrency(t *testing.T) {
	h := newHub()
	h.maxConns, h.maxPerIP, h.maxPerTok = 500, 0, 0
	var ok, refused atomic.Int64
	var mu sync.Mutex
	var releases []func()
	var wg sync.WaitGroup
	for i := range 2000 {
		wg.Go(func() {
			st, rel := h.admit(fmt.Sprintf("10.0.%d.%d", i/250, i%250), fmt.Sprint("tok", i))
			switch st {
			case 0:
				ok.Add(1)
				mu.Lock()
				releases = append(releases, rel)
				mu.Unlock()
			case http.StatusServiceUnavailable:
				refused.Add(1)
			default:
				t.Errorf("status %d", st)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 500 || refused.Load() != 1500 || h.open.Load() != 500 {
		t.Fatalf("admitted %d, refused %d, open %d; want 500, 1500, 500", ok.Load(), refused.Load(), h.open.Load())
	}
	for _, rel := range releases {
		rel()
	}
	if h.open.Load() != 0 {
		t.Fatalf("open %d after every release", h.open.Load())
	}
}

func TestHubAdmitPerAddressAndPerToken(t *testing.T) {
	h := newHub()
	h.maxConns, h.maxPerIP, h.maxPerTok = 0, 3, 2
	var rels []func()
	for i := range 3 {
		st, rel := h.admit("1.2.3.4", fmt.Sprint("t", i))
		if st != 0 {
			t.Fatalf("socket %d refused: %d", i, st)
		}
		rels = append(rels, rel)
	}
	if st, _ := h.admit("1.2.3.4", "other"); st != http.StatusTooManyRequests {
		t.Fatalf("a fourth socket of one address: %d", st)
	}
	if h.open.Load() != 3 {
		t.Fatalf("a refused socket stayed counted: %d", h.open.Load())
	}
	rels[0]()
	st, r1 := h.admit("5.6.7.8", "page")
	st2, r2 := h.admit("9.9.9.9", "page")
	if st != 0 || st2 != 0 {
		t.Fatalf("two sockets of a page: %d %d", st, st2)
	}
	if st, _ := h.admit("8.8.8.8", "page"); st != http.StatusTooManyRequests {
		t.Fatalf("a third socket of a page from another address: %d", st)
	}
	r1()
	r2()
	rels[1]()
	rels[2]()
	for i := range h.shards {
		sh := &h.shards[i]
		if len(sh.perIP) != 0 || len(sh.perToken) != 0 {
			t.Fatalf("shard %d keeps counters after every release: %v %v", i, sh.perIP, sh.perToken)
		}
	}
	if h.open.Load() != 0 {
		t.Fatalf("open %d", h.open.Load())
	}
}

func TestHubDeliverReachesOnlyItsTopic(t *testing.T) {
	h := newHub()
	cs := make([]*conn, 300)
	for i := range cs {
		cs[i] = testConn()
		if !h.add(cs[i]) {
			t.Fatal("add refused")
		}
		h.setTopics(cs[i], []string{"all", fmt.Sprint("own:", i)})
	}
	h.deliver("all", 1)
	h.deliver("own:7", 2)
	for i, c := range cs {
		want := 1
		if i == 7 {
			want = 2
		}
		if len(c.inbox) != want {
			t.Fatalf("conn %d has %d deliveries, want %d", i, len(c.inbox), want)
		}
	}
	h.deliver("", 3)
	for i, c := range cs {
		if got := c.inbox[len(c.inbox)-1].msg; got != 3 {
			t.Fatalf("conn %d: broadcast to every topic not last: %v", i, got)
		}
	}
	h.setTopics(cs[0], []string{"own:0"})
	h.deliver("all", 4)
	if len(cs[0].inbox) != 2 || len(cs[1].inbox) != 3 {
		t.Fatalf("after leaving a topic: %d, %d deliveries", len(cs[0].inbox), len(cs[1].inbox))
	}
	for _, c := range cs {
		h.remove(c)
	}
	for i := range h.shards {
		if sh := &h.shards[i]; len(sh.conns) != 0 || len(sh.topics) != 0 {
			t.Fatalf("shard %d not empty after remove", i)
		}
	}
}

func TestHubConcurrentSubscribeAndBroadcast(t *testing.T) {
	h := newHub()
	var wg sync.WaitGroup
	for i := range 64 {
		c := testConn()
		h.add(c)
		wg.Go(func() {
			for j := range 200 {
				h.setTopics(c, []string{fmt.Sprint("t", j%7), "shared"})
			}
			h.remove(c)
		})
		wg.Go(func() {
			for j := range 200 {
				h.deliver(fmt.Sprint("t", (i+j)%7), j)
				h.deliver("shared", j)
			}
		})
	}
	wg.Wait()
}

func TestHubCloseAllRefusesNewConns(t *testing.T) {
	h := newHub()
	h.closeAll()
	if h.add(testConn()) {
		t.Fatal("a connection joined a closed hub")
	}
}
