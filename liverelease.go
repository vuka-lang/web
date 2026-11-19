package web

import (
	"context"
	"crypto/tls"
	"net/http"
)

// liveRequest is what a live connection keeps of the request that opened it,
// with ctx as its context: the address, the headers (cookies, credentials,
// forwarding), the peer and the mount, which each run of a route for the
// connection starts from. The body, the parsed forms and the server's state
// for the request are not kept, so the request itself can be collected once
// the upgrade is done.
func liveRequest(r *http.Request, ctx context.Context) *http.Request {
	s := r.Clone(ctx)
	s.Body, s.GetBody, s.ContentLength = http.NoBody, nil, 0
	s.TransferEncoding, s.Trailer, s.Response = nil, nil, nil
	s.Form, s.PostForm, s.MultipartForm = nil, nil, nil
	if t := s.TLS; t != nil {
		s.TLS = &tls.ConnectionState{Version: t.Version, HandshakeComplete: t.HandshakeComplete, CipherSuite: t.CipherSuite, NegotiatedProtocol: t.NegotiatedProtocol, ServerName: t.ServerName}
	}
	return s
}

// discardWriter is the response a page's route has once the connection
// joined it: web.ResponseWriter(ctx) in a live event returns it, and nothing
// written to it goes anywhere.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (*discardWriter) WriteHeader(int)             {}
func (*discardWriter) Write(b []byte) (int, error) { return len(b), nil }

// LiveConns is how many live connections the app holds, joined or not.
func (a *App) LiveConns() int {
	n := 0
	a.hub.each(func(*conn) { n++ })
	return n
}
