//go:build !vuka_release

package web

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// What the toolbar serves: its script and stylesheet, the records' panels,
// the crons' and GraphiQL's routes; and the readable live runtime. A release
// build (vuka build, which sets the vuka_release tag) leaves them out
// (toolbar_release.go), and with them html/template's executor.

const toolbarLinked = true

//go:embed toolbar.js
var toolbarScript []byte

//go:embed toolbar.css
var toolbarCSS string

//go:embed toolbar-logo.svg
var toolbarLogo []byte

// toolbarSource is the script served, the stylesheet of its shadow DOM in it
// (the Vuka logo in that as a data URL).
var toolbarSource, toolbarTag = func() ([]byte, string) {
	logo := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(toolbarLogo)
	css, _ := json.Marshal(strings.Replace(toolbarCSS, "__LOGO__", logo, 1))
	src := bytes.Replace(toolbarScript, []byte(`"__CSS__"`), css, 1)
	src = append([]byte(devSocketJS), src...)
	return src, cssTag(src)
}()

// liveJS is the live runtime as written, served in development (live.go).
//
//go:embed live.js
var liveJS []byte

// routeToolbar serves the script, the records and the build's state.
func (a *App) routeToolbar() {
	tb := a.tb
	a.router.Handle(http.MethodGet, toolbarJS, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTagged(w, r, "text/javascript; charset=utf-8", toolbarTag, toolbarSource)
	}))
	a.router.Handle(http.MethodGet, toolbarData, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		e := tb.get(a.router.Param(r, "id"))
		if e == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "the toolbar keeps the latest " + strconv.Itoa(toolbarKeep) + " requests; this one is gone"})
			return
		}
		rec := e.snapshot()
		if q := r.URL.Query(); q.Has("wire") {
			seq, _ := strconv.Atoi(q.Get("wire"))
			i := slices.IndexFunc(rec.Live, func(m *tbEntry) bool { return m.Seq == seq })
			if i < 0 {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "the toolbar keeps the latest " + strconv.Itoa(toolbarLive) + " messages of a page; this one is gone"})
				return
			}
			var b bytes.Buffer
			if err := renderWire(&b, rec.Live[i].snapshot()); err != nil {
				fmt.Fprintf(&b, "<p class=errtext>%s</p>", template.HTMLEscapeString(err.Error()))
			}
			writeJSON(w, http.StatusOK, map[string]any{"html": b.String()})
			return
		}
		out := summary(rec)
		if r.URL.Query().Has("panels") {
			var b bytes.Buffer
			if err := renderPanels(&b, rec, append(a.cronPanel(), a.graphiqlPanel()...)...); err != nil {
				fmt.Fprintf(&b, "<p class=err>%s</p>", err)
			}
			out["html"] = b.String()
		}
		writeJSON(w, http.StatusOK, out)
	}))
	a.router.Handle(http.MethodGet, toolbarBuild, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, readBuildState(tb.dir, a.assets))
	}))
	a.routeCrons()
	a.routeGraphiQL()
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

type gqlFieldRow struct {
	Path, Field, At, Ms string
	Err                 bool
	SQL                 int
	Bar                 template.CSS
}

type gqlBatchRow struct {
	Loader, At, Ms, Err string
	Keys, SQL           int
	Bar                 template.CSS
}

type gqlView struct {
	Fields  []gqlFieldRow
	Batches []gqlBatchRow
	Total   string
	Slowest string
}

// graphqlOf lays a request's field trace out against its time: fields in
// the order they started, their statements counted by origin.
func graphqlOf(r tbRecord) gqlView {
	sqlBy := map[string]int{}
	for _, q := range r.Queries {
		if q.Origin != "" {
			sqlBy[q.Origin]++
		}
		if q.Field != "" && q.Origin == "" {
			sqlBy["\x00"+q.Field]++
		}
	}
	var total time.Duration
	for _, f := range r.GQLFields {
		total = max(total, f.Start.Sub(r.Start)+f.Dur)
	}
	for _, b := range r.GQLBatches {
		total = max(total, b.Start.Sub(r.Start)+b.Dur)
	}
	bar := func(start time.Time, d time.Duration) template.CSS {
		if total <= 0 {
			return ""
		}
		left := 100 * float64(start.Sub(r.Start)) / float64(total)
		return template.CSS(fmt.Sprintf("left:%.2f%%;width:%.2f%%", left, min(max(0.5, 100*float64(d)/float64(total)), 100-left)))
	}
	v := gqlView{Total: ms(total)}
	fields := slices.Clone(r.GQLFields)
	slices.SortStableFunc(fields, func(a, b GraphQLField) int { return a.Start.Compare(b.Start) })
	var slowest GraphQLField
	for _, f := range fields {
		if f.Dur > slowest.Dur {
			slowest = f
		}
		v.Fields = append(v.Fields, gqlFieldRow{Path: f.Path, Field: f.Field, At: ms(f.Start.Sub(r.Start)), Ms: ms(f.Dur), Err: f.Err, SQL: sqlBy[f.Path], Bar: bar(f.Start, f.Dur)})
	}
	if slowest.Path != "" {
		v.Slowest = slowest.Path + " (" + ms(slowest.Dur) + ")"
	}
	for _, b := range r.GQLBatches {
		v.Batches = append(v.Batches, gqlBatchRow{Loader: b.Loader, Keys: b.Keys, At: ms(b.Start.Sub(r.Start)), Ms: ms(b.Dur), Err: b.Err, SQL: sqlBy["\x00loader "+b.Loader], Bar: bar(b.Start, b.Dur)})
	}
	return v
}

// graphiqlPanel is the GraphiQL panel of an app serving web.GraphQL: the
// script fills it with its editor, which stays as records change.
func (a *App) graphiqlPanel() []panelView {
	if a.graphqlPath == "" {
		return nil
	}
	return []panelView{{Name: "GraphiQL", Body: template.HTML(`<div class="gql-slot" data-endpoint="` + template.HTMLEscapeString(a.graphqlPath) +
		`" data-script="` + graphiqlJS + `?v=` + graphiqlTag + `"></div>`)}}
}

//go:embed toolbar_graphiql.js
var graphiqlScript []byte

var graphiqlTag = cssTag(graphiqlScript)

// routeGraphiQL serves the panel's script, and checks its query as it is
// typed: the errors running it would meet before execution, as the app's
// own engine finds them. Neither is recorded as a request.
func (a *App) routeGraphiQL() {
	tb := a.tb
	a.router.Handle(http.MethodGet, graphiqlJS, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTagged(w, r, "text/javascript; charset=utf-8", graphiqlTag, graphiqlScript)
	}))
	a.router.Handle(http.MethodPost, graphiqlCheck, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// The toolbar's own request: a form on another site can't set the
		// header.
		if r.Header.Get(ToolbarHeader) == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "check a query from the toolbar"})
			return
		}
		tb.mu.Lock()
		check := tb.gqlCheck
		tb.mu.Unlock()
		if check == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "the app serves no GraphQL"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(check(body))
	}))
}

type wireView struct {
	Seq     int
	Client  *frameView
	Server  []frameView
	Changes int // diff rows in all
}

type frameView struct {
	tbFrame
	Label, Size, Split, Detail string
	Bars                       []shareBar
	JSON                       template.HTML
}

type shareBar struct {
	Name, Text string
	Style      template.CSS
}

func renderWire(w io.Writer, m tbRecord) error {
	v := wireView{Seq: m.Seq}
	for _, f := range m.Frames {
		fv := frameView{tbFrame: f, Size: bytesText(f.Size), JSON: highlightJSON(f.Body, f.Truncated)}
		if f.Dir == "client" {
			fv.Label, fv.Detail = clientLabel(f.Body)
			v.Client = &fv
			continue
		}
		if r := f.Report; r != nil {
			fv.Label = r.Type
			if r.V == 2 || len(r.Insts) > 0 && r.Insts[0].Kind != "html" {
				fv.Label += " · v2"
			}
			fv.Split = splitText(r)
			for _, s := range r.Split {
				if s.Bytes > 0 && r.Size > 0 {
					fv.Bars = append(fv.Bars, shareBar{s.Name, fmt.Sprintf("%s %s", s.Name, bytesText(s.Bytes)),
						template.CSS(fmt.Sprintf("width:%.2f%%", 100*float64(s.Bytes)/float64(r.Size)))})
				}
			}
			v.Changes += len(r.Rows)
		}
		v.Server = append(v.Server, fv)
	}
	return panelTemplates.ExecuteTemplate(w, "wire", v)
}

// clientLabel is a client frame's type and what it says: "c1:1 click",
// "/pets?page=2 (patch)", "form name, password".
func clientLabel(body string) (string, string) {
	var m struct {
		Type, Target, Event, Key, URL string
		V                             int
		Patch                         bool
		Form                          map[string]json.RawMessage
	}
	json.Unmarshal([]byte(body), &m)
	var parts []string
	switch m.Type {
	case "join":
		if m.V > 0 {
			parts = append(parts, "protocol v"+strconv.Itoa(m.V))
		}
	case "navigate":
		parts = append(parts, m.URL)
		if m.Patch {
			parts = append(parts, "(patch)")
		}
	default:
		parts = append(parts, m.Target, m.Event)
		if m.Key != "" {
			parts = append(parts, "key "+m.Key)
		}
		if len(m.Form) > 0 {
			names := slices.Sorted(maps.Keys(m.Form))
			parts = append(parts, "form "+strings.Join(names, ", "))
		}
	}
	return cmp.Or(m.Type, "message"), strings.TrimSpace(strings.Join(parts, " "))
}

// splitText is a frame's bytes by what they carry: "974 B: statics 610 B
// (new fingerprints 2), dynamics 280 B, framing 84 B".
func splitText(r *wireReport) string {
	var parts []string
	for _, s := range r.Split {
		p := s.Name + " " + bytesText(s.Bytes)
		if s.Note != "" {
			p += " (" + s.Note + ")"
		}
		parts = append(parts, p)
	}
	return bytesText(r.Size) + ": " + strings.Join(parts, ", ")
}

// highlightJSON pretty-prints a frame and colours its keys, strings,
// numbers and literals; a truncated frame is shown as it is.
func highlightJSON(body string, truncated bool) template.HTML {
	src := body
	if !truncated {
		var b bytes.Buffer
		if json.Indent(&b, []byte(body), "", "  ") == nil {
			src = b.String()
		}
	}
	var out strings.Builder
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(src))
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			class := "js"
			if k < len(src) && src[k] == ':' {
				class = "jk"
			}
			out.WriteString(`<span class="` + class + `">` + template.HTMLEscapeString(src[i:j]) + `</span>`)
			i = j
		case c == '-' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(src) && strings.IndexByte("0123456789.eE+-", src[j]) >= 0 {
				j++
			}
			out.WriteString(`<span class="jn">` + src[i:j] + `</span>`)
			i = j
		case strings.HasPrefix(src[i:], "true") || strings.HasPrefix(src[i:], "false") || strings.HasPrefix(src[i:], "null"):
			n := 4
			if c == 'f' {
				n = 5
			}
			out.WriteString(`<span class="jb">` + src[i:i+n] + `</span>`)
			i += n
		default:
			j := i + 1
			for j < len(src) && strings.IndexByte(`"-0123456789tfn`, src[j]) < 0 {
				j++
			}
			out.WriteString(template.HTMLEscapeString(src[i:j]))
			i = j
		}
	}
	return template.HTML(out.String())
}

// marked is one side of a changed value, quoted, the part that differs from
// the other side marked.
func marked(a, b string, side int) template.HTML {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	for pre > 0 && !utf8.RuneStart(a[pre]) && pre < len(a) {
		pre--
	}
	for suf > 0 && len(a)-suf < len(a) && !utf8.RuneStart(a[len(a)-suf]) {
		suf--
	}
	s := a
	if side == 1 {
		s = b
	}
	esc := template.HTMLEscapeString
	mid := s[pre : len(s)-suf]
	out := `"` + esc(s[:pre])
	if mid != "" {
		out += "<mark>" + esc(mid) + "</mark>"
	}
	return template.HTML(out + esc(s[len(s)-suf:]) + `"`)
}

// connView is the Connection panel of a record, and the state the handle
// shows.
type connView struct {
	State, Since, Text, Tone string
	Conns                    int
	Rows                     []connRow
}

type connRow struct {
	Clock, Kind, Label, Text, Detail, Tone string
	Conn, Code                             int
}

var connLabels = map[string]string{"open": "opened", "reconnect": "reconnected", "join": "joined", "rejoin": "rejoined",
	"close": "closed", "drop": "dropped", "refused": "refused", "leave": "moved", "arrive": "arrived"}

// connOf lays a page's connection events out, the latest first, and finds
// the page's state from them: connected while a connection that joined (or
// arrived) hasn't ended; connecting while one opened hasn't joined; else
// from how the last one ended — recovering (closed to load the page again),
// idle, moved (navigated to another page) or disconnected.
func connOf(evs []tbConn) connView {
	var v connView
	if len(evs) == 0 {
		return v
	}
	live := map[int]tbConn{} // each connection's last event, while it lasts
	seen := map[int]bool{}
	var last tbConn
	for _, e := range evs {
		seen[e.Conn] = true
		switch e.Kind {
		case "open", "reconnect", "join", "rejoin", "arrive":
			live[e.Conn] = e
		default:
			delete(live, e.Conn)
			last = e
		}
	}
	v.Conns = len(seen)
	var connected, connecting *tbConn
	for _, e := range live {
		switch e.Kind {
		case "join", "rejoin", "arrive":
			if connected == nil || e.At.After(connected.At) {
				connected = &e
			}
		default:
			if connecting == nil || e.At.After(connecting.At) {
				connecting = &e
			}
		}
	}
	switch {
	case connected != nil:
		v.State, v.Since = "connected", connected.At.Format("15:04:05.000")
	case connecting != nil:
		v.State, v.Since = "connecting", connecting.At.Format("15:04:05.000")
	default:
		v.Since = last.At.Format("15:04:05.000")
		switch {
		case last.Kind == "leave":
			v.State = "moved"
		case last.Code == int(closeReload):
			v.State, v.Tone = "recovering", "warn"
		case last.Code == int(closeIdle):
			v.State = "idle"
		default:
			v.State, v.Tone = "disconnected", "warn"
		}
	}
	v.Text = v.State + " since " + v.Since
	if v.State != "connected" && v.State != "connecting" {
		v.Text += " — " + last.Text
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		r := connRow{Clock: e.At.Format("15:04:05.000"), Kind: e.Kind, Label: connLabels[e.Kind], Text: e.Text, Detail: e.Detail, Conn: e.Conn, Code: e.Code}
		switch e.Kind {
		case "drop", "refused":
			r.Tone = "err"
		case "close":
			if e.Code != int(closeIdle) {
				r.Tone = "warn"
			}
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// graphiqlPage is what a browser opening the endpoint gets in development:
// a page whose toolbar opens on its GraphiQL panel.
func (a *App) graphiqlPage(w http.ResponseWriter, r *http.Request, path string) {
	tag := strings.Replace(a.tb.tag(r), "<script ", "<script data-vk-graphiql ", 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>GraphQL · %[1]s</title>%[2]s</head>`+
		`<body style="margin:2rem;font:14px/1.5 system-ui,sans-serif"><p>The development toolbar's GraphiQL panel runs queries against <code>%[1]s</code>; each run is a call of its list, with the request's field trace.</p></body></html>`,
		template.HTMLEscapeString(path), tag)
}
