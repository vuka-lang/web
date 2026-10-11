package web

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vuka-lang/ui"
)

// The toolbar's panels, drawn on the server for one record: the script
// puts them in its drawer as they come.

//go:embed toolbar.html
var toolbarHTML string

var panelTemplates = template.Must(template.New("toolbar").Funcs(template.FuncMap{"sql": highlightSQL, "bytes": bytesText, "sub": func(a, b int) int { return a - b },
	"markOld": func(a, b string) template.HTML { return marked(a, b, 0) }, "markNew": func(a, b string) template.HTML { return marked(a, b, 1) }}).Parse(toolbarHTML))

type panelView struct {
	Name, Count, Tone string
	Body              template.HTML
}

// summary is what the handle shows of a record.
func summary(r tbRecord) map[string]any {
	return map[string]any{"id": r.ID, "method": r.Method, "uri": r.URI, "status": r.Status, "ms": ms(r.Dur),
		"queries": len(r.Queries), "live": len(r.Live), "now": r.Now, "done": r.Done, "earlier": r.Earlier, "tone": worst(tones(r)...)}
}

func tones(r tbRecord) []string {
	t := []string{statusTone(r.Status), sqlOf(r.Queries).Tone, logsTone(r.Logs)}
	for _, m := range r.Live {
		if m.snapshot().Err != "" {
			t = append(t, "err")
		}
	}
	return t
}

func worst(ts ...string) string {
	switch {
	case slices.Contains(ts, "err"):
		return "err"
	case slices.Contains(ts, "warn"):
		return "warn"
	}
	return ""
}

func statusTone(s int) string {
	switch {
	case s >= 500:
		return "err"
	case s >= 400:
		return "warn"
	}
	return ""
}

func renderPanels(w io.Writer, r tbRecord) error {
	var ps []panelView
	add := func(name, count, tone, tmpl string, data any) {
		var b bytes.Buffer
		if err := panelTemplates.ExecuteTemplate(&b, tmpl, data); err != nil {
			b.Reset()
			fmt.Fprintf(&b, "<p class=errtext>%s</p>", template.HTMLEscapeString(err.Error()))
		}
		ps = append(ps, panelView{name, count, tone, template.HTML(b.String())})
	}
	add("Request", r.Method, statusTone(r.Status), "request", requestOf(r))
	s := sqlOf(r.Queries)
	add("SQL", count(len(r.Queries)), s.Tone, "sql", s)
	add("Timeline", ms(r.Dur), "", "timeline", timelineOf(r))
	add("Logs", count(len(r.Logs)), logsTone(r.Logs), "logs", logsOf(r.Logs))
	if len(r.Live) > 0 {
		lv := liveOf(r.Live)
		tone := ""
		if slices.ContainsFunc(lv, func(m liveMsg) bool { return m.Err != "" }) {
			tone = "err"
		}
		add("Live", count(len(r.Live)), tone, "live", lv)
	}
	ps = append(ps, notePanels(r.Notes)...)
	return panelTemplates.ExecuteTemplate(w, "panels", map[string]any{"Panels": ps, "Dropped": r.Dropped})
}

func count(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

// ms formats a duration for people: 0.42 ms, 12.3 ms, 1.24 s.
func ms(d time.Duration) string {
	f := float64(d) / float64(time.Millisecond)
	switch {
	case f >= 1000:
		return fmt.Sprintf("%.2f s", f/1000)
	case f >= 10:
		return fmt.Sprintf("%.1f ms", f)
	}
	return fmt.Sprintf("%.2f ms", f)
}

type requestView struct {
	tbRecord
	Ms, Func, Pos string
	Header, Sent  [][2]string
	Sections      []factSection
}

// factSection is NoteRequest's rows under one heading.
type factSection struct {
	Name string
	Rows []RequestNote
}

func requestOf(r tbRecord) requestView {
	v := requestView{tbRecord: r, Ms: ms(r.Dur), Header: headerRows(r.Req), Sent: headerRows(r.Resp)}
	v.Func, v.Pos, _ = strings.Cut(r.Handler, " at ")
	for _, f := range r.Facts {
		i := slices.IndexFunc(v.Sections, func(s factSection) bool { return s.Name == f.Section })
		if i < 0 {
			v.Sections = append(v.Sections, factSection{Name: f.Section})
			i = len(v.Sections) - 1
		}
		v.Sections[i].Rows = append(v.Sections[i].Rows, f)
	}
	return v
}

func headerRows(h http.Header) [][2]string {
	var rows [][2]string
	for k, vs := range h {
		rows = append(rows, [2]string{k, strings.Join(vs, ", ")})
	}
	slices.SortFunc(rows, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return rows
}

type sqlView struct {
	Rows                    []sqlRow
	Count, Repeated, Failed int
	Total, Tone             string
	NPlusOne                []nPlusOne
}

type sqlRow struct {
	N, Times           int
	SQL, Args, Err, DB string
	Rows               string
	Ms, Tone           string
	Bar                template.CSS
}

type nPlusOne struct {
	SQL   string
	Times int
}

// sqlOf lays the statements out: the same statement run more than once is
// repeated (amber); run three times or more with different arguments, an
// N+1 (each row of an earlier result read on its own).
func sqlOf(qs []Statement) sqlView {
	v := sqlView{Count: len(qs)}
	times, args := map[string]int{}, map[string]map[string]bool{}
	var total, longest time.Duration
	for _, q := range qs {
		times[q.SQL]++
		if args[q.SQL] == nil {
			args[q.SQL] = map[string]bool{}
		}
		args[q.SQL][fmt.Sprint(q.Args)] = true
		total += q.Duration
		longest = max(longest, q.Duration)
	}
	v.Total = ms(total)
	seen := map[string]bool{}
	for i, q := range qs {
		row := sqlRow{N: i + 1, Times: times[q.SQL], SQL: q.SQL, DB: q.DB, Ms: ms(q.Duration), Rows: ""}
		if len(q.Args) > 0 {
			row.Args = fmt.Sprint(q.Args)
		}
		if q.Rows >= 0 {
			row.Rows = fmt.Sprint(q.Rows)
		}
		if longest > 0 {
			row.Bar = template.CSS(fmt.Sprintf("width:%.1f%%", 100*float64(q.Duration)/float64(longest)))
		}
		switch {
		case q.Err != nil:
			row.Err, row.Tone = q.Err.Error(), "err"
			if q.Refused {
				row.Err = "not sent: " + row.Err
			}
			v.Failed++
		case row.Times > 1:
			row.Tone = "warn"
			v.Repeated++
		}
		if row.Times >= 3 && len(args[q.SQL]) > 1 && !seen[q.SQL] {
			v.NPlusOne = append(v.NPlusOne, nPlusOne{q.SQL, row.Times})
		}
		seen[q.SQL] = true
		v.Rows = append(v.Rows, row)
	}
	switch {
	case v.Failed > 0 || len(v.NPlusOne) > 0:
		v.Tone = "err"
	case v.Repeated > 0:
		v.Tone = "warn"
	}
	return v
}

var sqlKeywords = regexp.MustCompile(`(?i)\b(SELECT|FROM|WHERE|AND|OR|NOT|IN|IS|NULL|AS|ON|JOIN|LEFT|RIGHT|INNER|OUTER|INSERT|INTO|VALUES|UPDATE|SET|DELETE|ORDER|GROUP|BY|HAVING|LIMIT|OFFSET|RETURNING|DISTINCT|COUNT|CREATE|TABLE|INDEX|DROP|ALTER|EXISTS|LIKE|BETWEEN|CASE|WHEN|THEN|ELSE|END|UNION|ALL|ASC|DESC|WITH|BEGIN|COMMIT|ROLLBACK)\b`)

func highlightSQL(s string) template.HTML {
	return template.HTML(sqlKeywords.ReplaceAllString(template.HTMLEscapeString(s), `<b>$1</b>`))
}

type timelineRow struct {
	Name, Kind, Ms string
	Bar, Indent    template.CSS
}

// timelineOf draws the spans, and the statements as spans of their own,
// against the request's time.
func timelineOf(r tbRecord) []timelineRow {
	type item struct {
		tbSpan
		order int
	}
	var items []item
	for i, s := range r.Spans {
		items = append(items, item{s, i})
	}
	for i, q := range r.Queries {
		at := q.Start.Sub(r.Start)
		if q.Start.IsZero() {
			continue
		}
		depth := 0
		for _, s := range r.Spans {
			if s.At <= at && (s.Open || at+q.Duration <= s.At+s.Dur) {
				depth = max(depth, s.Depth+1)
			}
		}
		name := q.SQL
		if len(name) > 80 {
			name = name[:80] + "…"
		}
		items = append(items, item{tbSpan{Name: name, Kind: "sql", At: at, Dur: q.Duration, Depth: depth}, len(r.Spans) + i})
	}
	slices.SortStableFunc(items, func(a, b item) int {
		if a.At != b.At {
			return int(a.At - b.At)
		}
		return a.order - b.order
	})
	total := r.Dur
	for _, it := range items {
		total = max(total, it.At+it.Dur)
	}
	var rows []timelineRow
	for _, it := range items {
		d := it.Dur
		if it.Open {
			d = total - it.At
		}
		left, width := 0.0, 100.0
		if total > 0 {
			left, width = 100*float64(it.At)/float64(total), max(0.5, 100*float64(d)/float64(total))
		}
		rows = append(rows, timelineRow{Name: it.Name, Kind: it.Kind, Ms: ms(d),
			Bar: template.CSS(fmt.Sprintf("left:%.2f%%;width:%.2f%%", left, min(width, 100-left))), Indent: template.CSS(fmt.Sprintf("padding-left:%dpx", 4+12*it.Depth))})
	}
	return rows
}

type logRow struct {
	At, Level, Msg, Attrs, Tone string
}

func logsTone(ls []tbLog) string {
	t := ""
	for _, l := range ls {
		switch {
		case l.Level >= slog.LevelError:
			return "err"
		case l.Level >= slog.LevelWarn:
			t = "warn"
		}
	}
	return t
}

func logsOf(ls []tbLog) []logRow {
	var rows []logRow
	for _, l := range ls {
		rows = append(rows, logRow{At: "+" + ms(l.At), Level: l.Level.String(), Msg: l.Msg, Attrs: l.Attrs, Tone: logsTone([]tbLog{l})})
	}
	return rows
}

type liveMsg struct {
	Type, Subject, Clock, Ms, Bytes, Err, Tone string
	Ref, Q, Seq, Frames                        int
	Dropped                                    bool
	SQL                                        sqlView
	Timeline                                   []timelineRow
	Logs                                       []logRow
}

// liveOf lays a page's socket messages out, the latest first.
func liveOf(msgs []*tbEntry) []liveMsg {
	var out []liveMsg
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i].snapshot()
		s := sqlOf(m.Queries)
		v := liveMsg{Type: m.Method, Subject: m.URI, Clock: m.Start.Format("15:04:05.000"), Ms: ms(m.Dur), Bytes: bytesText(m.Bytes),
			Err: m.Err, Ref: m.Ref, Q: len(m.Queries), Seq: m.Seq, Frames: len(m.Frames), Dropped: m.FramesDropped, SQL: s, Timeline: timelineOf(m), Logs: logsOf(m.Logs)}
		switch {
		case m.Err != "": // red only for a message that failed; what it ran warns
			v.Tone = "err"
		case worst(s.Tone, logsTone(m.Logs)) != "":
			v.Tone = "warn"
		}
		if !m.Done {
			v.Ms = "…"
		}
		out = append(out, v)
	}
	return out
}

func bytesText(n int) string {
	if n >= 10_000 {
		return fmt.Sprintf("%.1f kB", float64(n)/1000)
	}
	return fmt.Sprintf("%d B", n)
}

// notePanels draws the panels the request noted values for, and those
// AddPanel registered: by their render functions, else as a list.
func notePanels(notes []tbNote) []panelView {
	panelsMu.Lock()
	registered := slices.Clone(panels)
	panelsMu.Unlock()
	byPanel := map[string][]any{}
	var names []string
	for _, p := range registered {
		names = append(names, p.name)
	}
	for _, n := range notes {
		if !slices.Contains(names, n.Panel) {
			names = append(names, n.Panel)
		}
		byPanel[n.Panel] = append(byPanel[n.Panel], n.Value)
	}
	var out []panelView
	for _, name := range names {
		vals := byPanel[name]
		var b bytes.Buffer
		i := slices.IndexFunc(registered, func(p customPanel) bool { return p.name == name })
		if i >= 0 {
			if err := renderPanel(&b, registered[i].render, vals); err != nil {
				b.Reset()
				fmt.Fprintf(&b, "<p class=errtext>%s</p>", template.HTMLEscapeString(err.Error()))
			}
		} else {
			var texts []string
			for _, v := range vals {
				texts = append(texts, fmt.Sprintf("%+v", v))
			}
			panelTemplates.ExecuteTemplate(&b, "notes", texts)
		}
		out = append(out, panelView{Name: name, Count: count(len(vals)), Body: template.HTML(b.String())})
	}
	return out
}

// renderPanel draws a panel of AddPanel's; one that panics shows why.
func renderPanel(w io.Writer, render func([]any) ui.Node, vals []any) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the panel panicked: %v", p)
		}
	}()
	if n := render(vals); n != nil {
		return n.Render(context.Background(), w)
	}
	return nil
}
