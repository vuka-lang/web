package web

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func hotSandbox(t *testing.T, env string) {
	t.Helper()
	envSandbox(t, map[string]string{".env": env})
	reset := func() {
		envState.Lock()
		envSources.m = nil
		envState.Unlock()
		envWatch.Lock()
		envWatch.entries = nil
		envWatch.Unlock()
	}
	reset()
	t.Cleanup(reset)
	if err := LoadEnv(); err != nil {
		t.Fatal(err)
	}
}

func keys(cs []EnvChange) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Key)
	}
	return out
}

func TestApplyEnvPrecedence(t *testing.T) {
	hotSandbox(t, "HOT_FILE=file\nHOT_BOTH=file\nHOT_DROP=file\n")
	t.Setenv("HOT_REAL", "real")
	t.Setenv("HOT_BOTH_REAL", "real")

	ch := ApplyEnv("cfg", map[string]string{"HOT_REAL": "srv", "HOT_FILE": "srv", "HOT_NEW": "srv", "HOT_DROP": "srv"})
	if got := os.Getenv("HOT_REAL"); got != "real" {
		t.Errorf("real environment overridden: %q", got)
	}
	for _, k := range []string{"HOT_FILE", "HOT_NEW", "HOT_DROP"} {
		if os.Getenv(k) != "srv" {
			t.Errorf("%s = %q, want srv", k, os.Getenv(k))
		}
	}
	want := []EnvChange{{"HOT_DROP", "file", "srv"}, {"HOT_FILE", "file", "srv"}, {"HOT_NEW", "", "srv"}}
	if !reflect.DeepEqual(ch, want) {
		t.Errorf("changes = %v, want %v", ch, want)
	}

	ch = ApplyEnv("cfg", map[string]string{"HOT_FILE": "srv", "HOT_NEW": "two"})
	if os.Getenv("HOT_DROP") != "file" {
		t.Errorf("dropped key not restored to the file's value: %q", os.Getenv("HOT_DROP"))
	}
	if want := []EnvChange{{"HOT_DROP", "srv", "file"}, {"HOT_NEW", "srv", "two"}}; !reflect.DeepEqual(ch, want) {
		t.Errorf("changes = %v, want %v", ch, want)
	}

	ch = ApplyEnv("cfg", map[string]string{"HOT_FILE": "srv"})
	if _, ok := os.LookupEnv("HOT_NEW"); ok {
		t.Error("dropped key no file defines is still set")
	}
	if len(ch) != 1 || ch[0].Key != "HOT_NEW" {
		t.Errorf("changes = %v", ch)
	}
	if ch = ApplyEnv("cfg", map[string]string{"HOT_FILE": "srv"}); len(ch) != 0 {
		t.Errorf("repeat apply changed %v", ch)
	}

	ApplyEnv("cfg", nil)
	if os.Getenv("HOT_FILE") != "file" || os.Getenv("HOT_REAL") != "real" {
		t.Error("an empty apply did not hand every key back")
	}
	if len(envSources.m) != 0 {
		t.Errorf("source still recorded: %v", envSources.m)
	}
}

func TestApplyEnvSetenvWins(t *testing.T) {
	hotSandbox(t, "HOT_A=file\n")
	ApplyEnv("cfg", map[string]string{"HOT_A": "srv", "HOT_B": "srv"})
	os.Setenv("HOT_A", "mine")
	os.Setenv("HOT_B", "mine")

	ch := ApplyEnv("cfg", map[string]string{"HOT_A": "srv2", "HOT_B": "srv2"})
	if len(ch) != 0 || os.Getenv("HOT_A") != "mine" || os.Getenv("HOT_B") != "mine" {
		t.Errorf("os.Setenv value overwritten: %v %q", ch, os.Getenv("HOT_A"))
	}
	ApplyEnv("cfg", nil)
	if os.Getenv("HOT_A") != "mine" {
		t.Error("a dropped key changed by os.Setenv was reset")
	}
}

func TestApplyEnvSources(t *testing.T) {
	hotSandbox(t, "")
	ApplyEnv("a", map[string]string{"HOT_X": "a"})
	ch := ApplyEnv("b", map[string]string{"HOT_X": "b", "HOT_Y": "b"})
	if os.Getenv("HOT_X") != "a" || !reflect.DeepEqual(keys(ch), []string{"HOT_Y"}) {
		t.Errorf("a key another source owns was taken: %v %q", ch, os.Getenv("HOT_X"))
	}
}

func TestApplyEnvReservedAndEmpty(t *testing.T) {
	hotSandbox(t, "")
	ch := ApplyEnv("cfg", map[string]string{"VUKA_ENV": "development", "HOT_E": ""})
	if os.Getenv("VUKA_ENV") == "development" || len(ch) != 0 {
		t.Errorf("reserved key applied: %v", ch)
	}
	if v, ok := os.LookupEnv("HOT_E"); !ok || v != "" {
		t.Error("an empty value is not set")
	}
}

func TestEnvFromFileAfterApply(t *testing.T) {
	hotSandbox(t, "HOT_F=file\n")
	ApplyEnv("cfg", map[string]string{"HOT_F": "srv"})
	if EnvFromFile("HOT_F") {
		t.Error("an applied value reads as from a file")
	}
	ApplyEnv("cfg", nil)
	if !EnvFromFile("HOT_F") {
		t.Error("a restored value does not read as from a file")
	}
}

func TestOnEnvChange(t *testing.T) {
	hotSandbox(t, "")
	var got []string
	rec := func(name string) func(o, n string) {
		return func(o, n string) { got = append(got, fmt.Sprintf("%s:%q>%q", name, o, n)) }
	}
	off1 := OnEnvChange("HOT_K", rec("one"))
	OnEnvChange("HOT_K", rec("two"))
	OnEnvChange("HOT_OTHER", rec("other"))

	ApplyEnv("cfg", map[string]string{"HOT_K": "v1"})
	ApplyEnv("cfg", map[string]string{"HOT_K": "v1"})
	ApplyEnv("cfg", map[string]string{"HOT_K": "v2"})
	off1()
	ApplyEnv("cfg", nil)
	want := []string{`one:"">"v1"`, `two:"">"v1"`, `one:"v1">"v2"`, `two:"v1">"v2"`, `two:"v2">""`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestOnEnvChangeReentrant(t *testing.T) {
	hotSandbox(t, "")
	var inner atomic.Int32
	OnEnvChange("HOT_R", func(o, n string) {
		off := OnEnvChange("HOT_R2", func(o, n string) { inner.Add(1) })
		ApplyEnv("cfg2", map[string]string{"HOT_R2": "x"})
		off()
		ApplyEnv("cfg2", map[string]string{"HOT_R2": "y"})
	})
	ApplyEnv("cfg", map[string]string{"HOT_R": "1"})
	if inner.Load() != 1 {
		t.Errorf("inner callback ran %d times", inner.Load())
	}
}

func TestOnEnvChangePanic(t *testing.T) {
	hotSandbox(t, "")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	var ran bool
	OnEnvChange("HOT_P", func(o, n string) { panic("secret-" + n) })
	OnEnvChange("HOT_P", func(o, n string) { ran = true })
	ApplyEnv("cfg", map[string]string{"HOT_P": "s3cr3t"})
	if !ran {
		t.Error("callback after a panicking one did not run")
	}
	out := buf.String()
	if !strings.Contains(out, "HOT_P") || strings.Contains(out, "s3cr3t") {
		t.Errorf("log = %q", out)
	}
}

type countedText struct{ s string }

var parses atomic.Int32

func (c *countedText) UnmarshalText(b []byte) error {
	parses.Add(1)
	c.s = string(b)
	return nil
}

func TestHot(t *testing.T) {
	hotSandbox(t, "")
	n := Hot[int]("HOT_N", 100)
	if n.Get() != 100 {
		t.Errorf("default = %d", n.Get())
	}
	ApplyEnv("cfg", map[string]string{"HOT_N": "7"})
	if n.Get() != 7 {
		t.Errorf("after apply = %d", n.Get())
	}
	ApplyEnv("cfg", nil)
	if n.Get() != 100 {
		t.Errorf("after drop = %d", n.Get())
	}

	h := Hot[countedText]("HOT_T")
	ApplyEnv("cfg", map[string]string{"HOT_T": "a"})
	parses.Store(0)
	for i := 0; i < 50; i++ {
		if h.Get().s != "a" {
			t.Fatal("wrong value")
		}
	}
	if parses.Load() != 1 {
		t.Errorf("parsed %d times for one value", parses.Load())
	}
	ApplyEnv("cfg", map[string]string{"HOT_T": "b"})
	if h.Get().s != "b" || h.Get().s != "b" || parses.Load() != 2 {
		t.Errorf("parses = %d", parses.Load())
	}
}

func TestHotConversionError(t *testing.T) {
	hotSandbox(t, "")
	takeEnvErrors()
	n := Hot("HOT_BAD", 5)
	ApplyEnv("cfg", map[string]string{"HOT_BAD": "s3cr3t"})
	if n.Get() != 5 || n.Get() != 5 {
		t.Errorf("bad value = %d, want the default", n.Get())
	}
	errs := takeEnvErrors()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "HOT_BAD") || strings.Contains(errs[0].Error(), "s3cr3t") {
		t.Errorf("errors = %v", errs)
	}
	ApplyEnv("cfg", map[string]string{"HOT_BAD": "9"})
	if n.Get() != 9 || len(takeEnvErrors()) != 0 {
		t.Error("a fixed value still fails")
	}
}

func TestApplyEnvConcurrent(t *testing.T) {
	hotSandbox(t, "HOT_C=file\n")
	h := Hot[int]("HOT_C", -1)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if v := Env("HOT_C", -1); v != -1 && (v < 0 || v > 100) {
					t.Errorf("Env read %d", v)
				}
				if v := h.Get(); v < -1 || v > 100 {
					t.Errorf("Hot read %d", v)
				}
			}
		}()
	}
	var calls atomic.Int32
	OnEnvChange("HOT_C", func(o, n string) { calls.Add(1) })
	for i := 0; i < 2000; i++ {
		if i%7 == 0 {
			ApplyEnv("cfg", nil)
		} else {
			ApplyEnv("cfg", map[string]string{"HOT_C": fmt.Sprint(i % 100)})
		}
	}
	close(stop)
	wg.Wait()
	if calls.Load() == 0 {
		t.Error("no callbacks")
	}
}
