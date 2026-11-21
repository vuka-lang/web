package web

import (
	"context"
	"net/http"
	"slices"
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
