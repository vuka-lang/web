package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The toolbar's crons: each run is a record of its own, like a request's
// (its statements, spans, logs and notes), kept apart from the requests'
// ring so a cron running every second doesn't push them out. Every record
// gets a Crons panel listing the app's crons, each with a button running
// it now.

const (
	toolbarCrons    = livePrefix + "toolbar/crons"
	toolbarCronRun  = livePrefix + "toolbar/crons/run"
	toolbarCronKeep = 50 // cron runs kept
)

func (tb *toolbar) addCron(e *tbEntry) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.crons) >= toolbarCronKeep {
		delete(tb.byID, tb.crons[0].ID)
		tb.crons = slices.Delete(tb.crons, 0, 1)
	}
	tb.crons = append(tb.crons, e)
	tb.byID[e.ID] = e
}

// cronEntry starts the record of a run of j; finish ends it. Without a
// toolbar, ctx as is and a nil finish.
func (a *App) cronEntry(ctx context.Context, j *cronJob) (context.Context, func(error)) {
	if a.tb == nil {
		return ctx, nil
	}
	e := &tbEntry{tbRecord: tbRecord{ID: newToolbarID(), Method: "CRON", URI: j.name, Route: "cron " + j.spec, Handler: j.where, Start: time.Now()}}
	a.tb.addCron(e)
	s := &tbSink{tb: a.tb}
	s.cur.Store(e)
	unbind := bindGoroutine(s)
	return context.WithValue(ctx, tbKey{}, s), func(err error) {
		unbind()
		e.mu.Lock()
		e.Status, e.Dur, e.Done = http.StatusOK, time.Since(e.Start), true
		if err != nil {
			e.Status, e.Err = http.StatusInternalServerError, err.Error()
		}
		e.mu.Unlock()
	}
}

// routeCrons serves the list of cron runs and runs a cron now.
func (a *App) routeCrons() {
	tb := a.tb
	a.router.Handle(http.MethodGet, toolbarCrons, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		tb.mu.Lock()
		runs := slices.Clone(tb.crons)
		tb.mu.Unlock()
		out := []map[string]any{}
		for _, e := range slices.Backward(runs) {
			rec := e.snapshot()
			state := "…"
			if rec.Done {
				state = "ok"
				if rec.Err != "" {
					state = "failed"
				}
			}
			out = append(out, map[string]any{"id": rec.ID, "label": fmt.Sprintf("cron %s · %s · %s", rec.URI, rec.Start.Format("15:04:05"), state)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": out})
	}))
	a.router.Handle(http.MethodPost, toolbarCronRun, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The toolbar's own request: a form on another site can't set the
		// header.
		if r.Header.Get(ToolbarHeader) == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "run a cron from the toolbar"})
			return
		}
		s, name := a.sched, r.URL.Query().Get("name")
		j := s.job(name)
		if j == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("no cron named %q", name)})
			return
		}
		if !s.enter() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the app is stopping"})
			return
		}
		started, failed := make(chan string, 1), make(chan error, 1)
		go func() {
			defer s.leave()
			if _, err := s.run(s.runContext(), j, time.Time{}, true, func(id string) { started <- id }); errors.Is(err, ErrCronRunning) {
				failed <- err
			}
		}()
		select {
		case id := <-started:
			writeJSON(w, http.StatusOK, map[string]any{"id": id})
		case <-failed:
			writeJSON(w, http.StatusConflict, map[string]any{"error": name + " is still running"})
		}
	}))
}

// runContext is the context of a run started from the toolbar: the
// scheduler's once started, so Stop waits for it.
func (s *scheduler) runContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

type cronRow struct {
	Name, Spec, Func, Pos, Next, Last, LastID, Err, Tone string
	Running                                              bool
}

// cronPanel is the Crons panel: the app's crons, their next and last runs.
func (a *App) cronPanel() []panelView {
	if a.sched == nil {
		return nil
	}
	now := a.sched.clock.Now()
	var rows []cronRow
	tone := ""
	for _, j := range a.sched.jobs {
		j.mu.Lock()
		next, last := j.next, j.last
		j.mu.Unlock()
		if next.IsZero() || !next.After(now) {
			next = j.sched.Next(now)
		}
		row := cronRow{Name: j.name, Spec: j.spec, Running: j.running.Load(), LastID: last.ID, Err: last.Err}
		row.Func, row.Pos, _ = strings.Cut(j.where, " at ")
		if !next.IsZero() {
			row.Next = next.Format("15:04:05") + " (in " + next.Sub(now).Round(time.Second).String() + ")"
			if !a.sched.running() {
				row.Next = "not scheduled"
			}
		}
		if !last.At.IsZero() {
			how := "ok"
			if last.Err != "" {
				how, row.Tone, tone = "failed", "err", "err"
			}
			if last.Manual {
				how += ", run by hand"
			}
			row.Last = fmt.Sprintf("%s · %s · %s", last.At.Format("15:04:05"), ms(last.Dur), how)
		}
		rows = append(rows, row)
	}
	var b bytes.Buffer
	if err := panelTemplates.ExecuteTemplate(&b, "crons", rows); err != nil {
		b.Reset()
		fmt.Fprintf(&b, "<p class=errtext>%s</p>", template.HTMLEscapeString(err.Error()))
	}
	return []panelView{{Name: "Crons", Count: count(len(rows)), Tone: tone, Body: template.HTML(b.String())}}
}

// running reports whether Start scheduled the crons and Stop hasn't
// stopped them.
func (s *scheduler) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx != nil && !s.closed
}
