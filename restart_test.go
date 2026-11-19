package web

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRestartOnlyEnv(t *testing.T) {
	prefix := fmt.Sprintf("RESTART_TEST_%d_", time.Now().UnixNano())
	key, other, live := prefix+"KEY", prefix+"OTHER", prefix+"LIVE"
	if NeedsRestart(key) {
		t.Fatal("reported before it was recorded")
	}
	RestartOnlyEnv(key, other)
	RestartOnlyEnv(key)
	for _, k := range []string{key, other} {
		if !NeedsRestart(k) {
			t.Errorf("NeedsRestart(%s) = false", k)
		}
	}
	if NeedsRestart(live) {
		t.Error("an unrecorded key needs a restart")
	}
}

func TestRestartWithoutRun(t *testing.T) {
	if Restart() {
		t.Fatal("Restart reported an app with none running")
	}
}

func TestRestartStopsRunWithStatus(t *testing.T) {
	status := make(chan int, 1)
	old := exit
	exit = func(code int) { status <- code }
	t.Cleanup(func() { exit = old })

	takeEnvErrors() // those of other tests would fail Start
	var stopped bool
	app := New(NoDotEnv(), OnStop(func(context.Context) error { stopped = true; return nil }))
	app.reg = registry{views: map[string]declView{}, uses: map[string][]Middleware{}} // not what other tests declared
	done := make(chan error, 1)
	go func() { done <- app.Run("127.0.0.1:0") }()

	deadline := time.Now().Add(5 * time.Second)
	for !Restart() {
		select {
		case err := <-done:
			t.Fatalf("Run ended before it was restarted: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no app was running to restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return")
	}
	if code := <-status; code != RestartStatus {
		t.Errorf("exit status %d, want %d", code, RestartStatus)
	}
	if !stopped {
		t.Error("the services weren't stopped before the exit")
	}
	if Restart() {
		t.Error("a finished Run is still registered")
	}
}
