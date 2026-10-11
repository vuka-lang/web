package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vuka-lang/ui"
)

// built is a message checked and rendered, ready to send.
type built struct {
	msg        Message
	from       *netmail.Address
	rcpts      []string // envelope recipients
	html, text string
	raw        []byte
	at         time.Time
}

func (b *built) sent() Sent {
	return Sent{Message: b.msg, From: b.from.String(), Recipients: b.rcpts, HTML: b.html, Text: b.text, Raw: b.raw, At: b.at}
}

// InvalidError is a message refused before sending: what is wrong with it.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "mail: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// clean refuses a header value with a line break: what a header injection
// needs, a "\r\nBcc: …" smuggled into a subject or a name.
func clean(what, v string) error {
	if strings.ContainsAny(v, "\r\n\x00") {
		return invalid("%s has a line break (header injection refused): %q", what, v)
	}
	return nil
}

func parseAddress(what, s string) (*netmail.Address, error) {
	if err := clean(what, s); err != nil {
		return nil, err
	}
	a, err := netmail.ParseAddress(s)
	if err != nil {
		return nil, invalid("%s: %q isn't an email address", what, s)
	}
	return a, nil
}

func parseList(what string, list []string) ([]*netmail.Address, error) {
	var out []*netmail.Address
	for _, s := range list {
		a, err := parseAddress(what, s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Headers the Message's fields write; Headers can't set them.
var ownHeaders = []string{"From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Mime-Version", "Content-Type", "Content-Transfer-Encoding", "Content-Disposition"}

// build checks msg and renders it into a MIME message: the body a single
// part, multipart/alternative for text and HTML, inside multipart/mixed
// with attachments.
func (m *Mailer) build(ctx context.Context, msg Message) (*built, error) {
	b := &built{msg: msg, at: time.Now()}
	switch {
	case msg.From != "":
		a, err := parseAddress("From", msg.From)
		if err != nil {
			return nil, err
		}
		b.from = a
	case m.from != nil:
		a := *m.from
		b.from = &a
	default:
		return nil, invalid("no sender: set the message's From, or MAIL_FROM")
	}
	to, err := parseList("To", msg.To)
	if err != nil {
		return nil, err
	}
	cc, err := parseList("Cc", msg.Cc)
	if err != nil {
		return nil, err
	}
	bcc, err := parseList("Bcc", msg.Bcc)
	if err != nil {
		return nil, err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return nil, invalid("%q has no recipient", msg.Subject)
	}
	for _, a := range slices.Concat(to, cc, bcc) {
		if !slices.Contains(b.rcpts, a.Address) {
			b.rcpts = append(b.rcpts, a.Address)
		}
	}
	var replyTo *netmail.Address
	if msg.ReplyTo != "" {
		if replyTo, err = parseAddress("ReplyTo", msg.ReplyTo); err != nil {
			return nil, err
		}
	}
	if err := clean("Subject", msg.Subject); err != nil {
		return nil, err
	}
	var extra []string
	for k, v := range msg.Headers {
		if !validHeaderName(k) {
			return nil, invalid("header name %q isn't a valid name", k)
		}
		if slices.Contains(ownHeaders, textproto.CanonicalMIMEHeaderKey(k)) {
			return nil, invalid("header %s is the message's own: set it with Message's fields", k)
		}
		if err := clean("header "+k, v); err != nil {
			return nil, err
		}
		extra = append(extra, k)
	}
	slices.Sort(extra)
	for i, a := range msg.Attachments {
		if a.Filename == "" {
			return nil, invalid("attachment %d has no Filename", i+1)
		}
		if err := clean("attachment "+a.Filename, a.Filename+a.ContentType); err != nil {
			return nil, err
		}
		if a.ContentType != "" {
			if _, _, err := mime.ParseMediaType(a.ContentType); err != nil {
				return nil, invalid("attachment %s: content type %q: %v", a.Filename, a.ContentType, err)
			}
		}
	}
	if msg.HTML != nil {
		s, err := ui.String(ctx, msg.HTML)
		if err != nil {
			return nil, fmt.Errorf("mail: rendering %q: %w", msg.Subject, err)
		}
		b.html = s
	}
	b.text = msg.Text
	if b.text == "" && b.html != "" {
		b.text = htmlToText(b.html)
	}

	var w bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&w, "%s: %s\r\n", k, v) }
	header("From", b.from.String())
	if len(to) > 0 {
		header("To", addressList(to))
	}
	if len(cc) > 0 {
		header("Cc", addressList(cc))
	}
	if replyTo != nil {
		header("Reply-To", replyTo.String())
	}
	header("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	if !hasHeader(msg.Headers, "Date") {
		header("Date", b.at.Format(time.RFC1123Z))
	}
	if !hasHeader(msg.Headers, "Message-Id") {
		header("Message-ID", messageID(b.from.Address))
	}
	header("MIME-Version", "1.0")
	for _, k := range extra {
		header(textproto.CanonicalMIMEHeaderKey(k), mime.QEncoding.Encode("utf-8", msg.Headers[k]))
	}

	body := func(w io.Writer) (string, error) { return writeBody(w, b.text, b.html) }
	if len(msg.Attachments) == 0 {
		var part bytes.Buffer
		ct, err := body(&part)
		if err != nil {
			return nil, err
		}
		w.WriteString(ct)
		w.Write(part.Bytes())
	} else {
		mw := newMultipart(&w)
		header("Content-Type", `multipart/mixed; boundary="`+mw.Boundary()+`"`)
		w.WriteString("\r\n")
		var part bytes.Buffer
		ct, err := body(&part)
		if err != nil {
			return nil, err
		}
		// ct is the body's headers and their blank line; part its content.
		h := textproto.MIMEHeader{}
		for _, line := range strings.Split(strings.TrimSuffix(ct, "\r\n\r\n"), "\r\n") {
			k, v, _ := strings.Cut(line, ": ")
			h.Set(k, v)
		}
		pw, _ := mw.CreatePart(h)
		pw.Write(part.Bytes())
		for _, a := range msg.Attachments {
			ctype := a.ContentType
			if ctype == "" {
				if ctype = mime.TypeByExtension(path.Ext(a.Filename)); ctype == "" {
					ctype = "application/octet-stream"
				}
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", ctype)
			h.Set("Content-Transfer-Encoding", "base64")
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
			pw, _ := mw.CreatePart(h)
			writeBase64(pw, a.Data)
		}
		mw.Close()
	}
	b.raw = w.Bytes()
	return b, nil
}

// writeBody writes the body's content to w and returns its headers (and
// the blank line after them): one part, or multipart/alternative.
func writeBody(w io.Writer, text, html string) (string, error) {
	const qp = "Content-Transfer-Encoding: quoted-printable\r\n"
	switch {
	case html == "":
		writeQP(w, text)
		return "Content-Type: text/plain; charset=utf-8\r\n" + qp + "\r\n", nil
	case text == "":
		writeQP(w, html)
		return "Content-Type: text/html; charset=utf-8\r\n" + qp + "\r\n", nil
	}
	mw := newMultipart(w)
	for _, p := range []struct{ ct, s string }{{"text/plain; charset=utf-8", text}, {"text/html; charset=utf-8", html}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.ct)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, err := mw.CreatePart(h)
		if err != nil {
			return "", err
		}
		writeQP(pw, p.s)
	}
	mw.Close()
	return `Content-Type: multipart/alternative; boundary="` + mw.Boundary() + "\"\r\n\r\n", nil
}

// newMultipart is a multipart writer with a boundary short enough to keep
// its Content-Type header within 78 characters.
func newMultipart(w io.Writer) *multipart.Writer {
	mw := multipart.NewWriter(w)
	b := make([]byte, 12)
	rand.Read(b)
	mw.SetBoundary("vk" + hex.EncodeToString(b))
	return mw
}

func writeQP(w io.Writer, s string) {
	q := quotedprintable.NewWriter(w)
	q.Write([]byte(strings.ReplaceAll(s, "\r\n", "\n")))
	q.Close()
}

func writeBase64(w io.Writer, data []byte) {
	s := base64.StdEncoding.EncodeToString(data)
	for len(s) > 76 {
		io.WriteString(w, s[:76]+"\r\n")
		s = s[76:]
	}
	io.WriteString(w, s+"\r\n")
}

func addressList(as []*netmail.Address) string {
	var out []string
	for _, a := range as {
		out = append(out, a.String())
	}
	return strings.Join(out, ", ")
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if textproto.CanonicalMIMEHeaderKey(k) == name {
			return true
		}
	}
	return false
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 127 || c == ':' {
			return false
		}
	}
	return true
}

func messageID(from string) string {
	b := make([]byte, 16)
	rand.Read(b)
	_, domain, _ := strings.Cut(from, "@")
	if domain == "" {
		domain = "localhost"
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">"
}
