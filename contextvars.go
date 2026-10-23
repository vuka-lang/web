package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The context processors web provides, which an app lists in
// ContextProcessors like its own.

// RequestInfo is the request a page renders for, as templates read it.
type RequestInfo struct {
	Path    string            // "/users/42"
	Query   url.Values        // the URL's query parameters
	Params  map[string]string // the route's path parameters by name: {"id": "42"}
	Name    string            // the route's handler: "users.Show"
	Pattern string            // the route: "GET /users/{id}"
}

// RequestVars is RequestContext's result: the variable request.
type RequestVars struct {
	Request RequestInfo
}

// RequestContext is a context processor giving templates the variable
// request: the path, query, path parameters and route of the request the
// page renders for. Its input is the URL: on a live page that moves to
// another, the components showing it render again, a layout that stays
// among them.
func RequestContext(ctx context.Context) (RequestVars, error) {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return RequestVars{}, errors.New("web: RequestContext: no request here")
	}
	recordRequest(ctx)
	r := rq.r
	info := RequestInfo{Path: r.URL.Path, Query: r.URL.Query(), Params: map[string]string{}}
	if s := rq.route; s != nil {
		info.Name = shortName(s.where)
		info.Pattern = s.method + " " + s.path
		for _, n := range pathParams(s.path) {
			info.Params[n] = r.PathValue(n)
		}
	}
	return RequestVars{Request: info}, nil
}

// recordRequest records the request's URL as an input of the context
// processor ctx runs (RecordInput).
func recordRequest(ctx context.Context) {
	if runOf(ctx) == nil {
		return
	}
	RecordInput(ctx, Input{Key: "request", Now: requestURL})
}

func requestURL(ctx context.Context) string {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return ""
	}
	u := rq.r.URL.RequestURI()
	if s := rq.route; s != nil {
		u = s.method + " " + s.path + " " + u
	}
	return u
}

// Message is a flash message: one Flash added, shown by the next page that
// renders the variable messages.
type Message struct {
	Level string `json:"level"` // "info", "success", "warning", "error": the app's own words
	Text  string `json:"text"`
}

// MessagesVars is MessagesContext's result: the variable messages.
type MessagesVars struct {
	Messages []Message
}

// Flash adds a message for the visitor, which the next page reading the
// variable messages (MessagesContext) shows once: a form's handler flashes
// "Saved" and redirects, and the page it redirects to shows it. Over HTTP
// it travels in a signed cookie, so it must be called before the response
// is written; from a live event it shows in the page's next render. It
// needs a request's or a live page's context of an app listing
// MessagesContext.
func Flash(ctx context.Context, level, text string) error {
	st := storeOf(ctx)
	if st == nil || st.rq == nil {
		return errors.New("web: Flash: ctx isn't a request's or a live page's of an app with web.ContextProcessors")
	}
	m := Message{Level: level, Text: text}
	if h := st.hist; h != nil {
		h.mu.Lock()
		h.msgs = append(h.msgs, m)
		h.msgVer++
		h.shown = false
		h.mu.Unlock()
		return nil
	}
	st.mu.Lock()
	st.msgs = append(st.msgs, m)
	msgs := append(incomingMessages(st.rq), st.msgs...)
	st.mu.Unlock()
	st.rq.app.setMessages(st.rq.w, msgs)
	return nil
}

// MessagesContext is a context processor giving templates the variable
// messages: what Flash added for the visitor, each shown once — a page
// rendering them clears them.
func MessagesContext(ctx context.Context) (MessagesVars, error) {
	st := storeOf(ctx)
	if st == nil || st.rq == nil {
		return MessagesVars{}, nil
	}
	var out []Message
	if h := st.hist; h != nil {
		RecordInput(ctx, Input{Key: "messages", Now: messagesVersion})
		h.mu.Lock()
		out = append(out, h.msgs...)
		h.shown = true
		h.mu.Unlock()
	}
	st.mu.Lock()
	out = append(out, st.msgs...)
	st.msgs = nil
	st.mu.Unlock()
	if in := incomingMessages(st.rq); len(in) > 0 {
		out = append(in, out...)
		if st.hist == nil {
			st.rq.app.setMessages(st.rq.w, nil)
		}
	}
	if st.hist == nil {
		st.mu.Lock()
		st.shown = append(st.shown, out...)
		st.mu.Unlock()
	}
	return MessagesVars{Messages: out}, nil
}

// shownMessages are the messages a page's render of ctx's request showed.
func shownMessages(ctx context.Context) []Message {
	st := storeOf(ctx)
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.shown
}

// joined gives a live connection the messages its page's HTTP render
// showed, which its join renders again.
func (h *varHistory) joined(msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	h.mu.Lock()
	h.msgs = append(msgs, h.msgs...)
	h.msgVer++
	h.mu.Unlock()
}

// msgStash keeps the messages a live page's HTTP render showed, under its
// join token, until its socket joins.
type msgStash struct {
	mu sync.Mutex
	m  map[string]stashed
}

type stashed struct {
	msgs []Message
	at   time.Time
}

const stashFor = 2 * time.Minute

func (s *msgStash) put(token string, msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.m == nil {
		s.m = map[string]stashed{}
	}
	for k, v := range s.m {
		if now.Sub(v.at) > stashFor {
			delete(s.m, k)
		}
	}
	s.m[token] = stashed{msgs, now}
}

func (s *msgStash) take(token string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[token]
	if !ok || time.Since(v.at) > stashFor {
		return nil
	}
	delete(s.m, token)
	return v.msgs
}

func messagesVersion(ctx context.Context) string {
	if st := storeOf(ctx); st != nil && st.hist != nil {
		st.hist.mu.Lock()
		defer st.hist.mu.Unlock()
		return strconv.FormatUint(st.hist.msgVer, 10)
	}
	return ""
}

// newEvent starts a live event: the messages the page showed go.
func (h *varHistory) newEvent() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.shown && len(h.msgs) > 0 {
		h.msgs, h.shown = nil, false
		h.msgVer++
	}
	h.mu.Unlock()
}

const messagesCookie = "vk_messages"

// incomingMessages are the messages the request's cookie carries, when its
// signature holds.
func incomingMessages(rq *request) []Message {
	c, err := rq.r.Cookie(messagesCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	b, err1 := base64.RawURLEncoding.DecodeString(body)
	s, err2 := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(s, rq.app.mac("vk-messages", b)) {
		return nil
	}
	var msgs []Message
	if json.Unmarshal(b, &msgs) != nil {
		return nil
	}
	return msgs
}

// setMessages sets the messages cookie to msgs, or expires it for none.
func (a *App) setMessages(w http.ResponseWriter, msgs []Message) {
	if w == nil {
		return
	}
	c := &http.Cookie{Name: messagesCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if len(msgs) == 0 {
		c.MaxAge = -1
	} else {
		b, _ := json.Marshal(msgs)
		c.Value = base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(a.mac("vk-messages", b))
	}
	h := w.Header()
	// one Set-Cookie for it: a later Flash replaces an earlier one's
	kept := h.Values("Set-Cookie")[:0:0]
	for _, v := range h.Values("Set-Cookie") {
		if !strings.HasPrefix(v, messagesCookie+"=") {
			kept = append(kept, v)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
	http.SetCookie(w, c)
}

// mac is the app's signature of msg for purpose, with its live key.
func (a *App) mac(purpose string, msg []byte) []byte {
	m := hmac.New(sha256.New, a.liveKey)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write(msg)
	return m.Sum(nil)
}

// CSRFVars is CSRFContext's result: the variable csrfToken.
type CSRFVars struct {
	CSRFToken string
}

// CSRFContext is a context processor giving templates the variable
// csrfToken: a token bound to the visitor's browser (a random cookie it
// sets the first time) and sign-in (LiveIdentity), for a form whose
// handler checks it with ValidCSRFToken — one posted from another origin
// the app trusts, say. web's own protection (CSRF) checks the request's
// origin and needs no token.
func CSRFContext(ctx context.Context) (CSRFVars, error) {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil {
		return CSRFVars{}, errors.New("web: CSRFContext: no request here")
	}
	return CSRFVars{CSRFToken: rq.app.csrfToken(rq, true)}, nil
}

const csrfCookie = "vk_csrf"

// csrfToken is the request's visitor's token; set makes the browser's
// cookie when it has none and the response can still carry one.
func (a *App) csrfToken(rq *request, set bool) string {
	seed := ""
	if c, err := rq.r.Cookie(csrfCookie); err == nil && len(c.Value) >= 22 {
		seed = c.Value
	} else if set && rq.w != nil && rq.r.Header.Get("X-Vuka-Live") == "" {
		var b [18]byte
		rand.Read(b[:])
		seed = base64.RawURLEncoding.EncodeToString(b[:])
		http.SetCookie(rq.w, &http.Cookie{Name: csrfCookie, Value: seed, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: rq.r.TLS != nil})
		rq.r.AddCookie(&http.Cookie{Name: csrfCookie, Value: seed}) // later reads of this request agree
	}
	return base64.RawURLEncoding.EncodeToString(a.mac("vk-csrf", []byte(seed+"\x00"+a.liveWho(rq.ctx))))
}

// ValidCSRFToken reports whether token is the csrfToken CSRFContext gives
// the visitor of ctx's request.
func ValidCSRFToken(ctx context.Context, token string) bool {
	rq, _ := ctx.Value(ctxKey{}).(*request)
	if rq == nil || token == "" {
		return false
	}
	if c, err := rq.r.Cookie(csrfCookie); err != nil || len(c.Value) < 22 {
		return false
	}
	return hmac.Equal([]byte(token), []byte(rq.app.csrfToken(rq, false)))
}

// RefreshVars makes the processors giving the variables names run again on
// their next read, as when an input of theirs changed: an event that
// changed what a processor loads (the user's own record) calls it, and the
// components showing the variables render again if their values changed.
func RefreshVars(ctx context.Context, names ...string) {
	st := storeOf(ctx)
	if st == nil {
		return
	}
	for _, n := range names {
		f := st.set.byName[n]
		if f == nil {
			continue
		}
		e := st.entry(f.proc)
		e.mu.Lock()
		e.stale = true
		e.mu.Unlock()
	}
}
