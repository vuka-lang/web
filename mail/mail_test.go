package mail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"strings"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newLog(t *testing.T) *Mailer {
	t.Helper()
	m, err := New(Config{From: "Pets <hello@pets.example>", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// part is a MIME part read back: its content type and decoded body.
type part struct {
	ctype, disposition, body string
	parts                    []part
}

func parse(t *testing.T, raw []byte) (*netmail.Message, part) {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	return msg, readPart(t, msg.Header.Get("Content-Type"), "", msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
}

func readPart(t *testing.T, ctype, disp, cte string, body io.Reader) part {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatal(err)
	}
	p := part{ctype: mt, disposition: disp}
	if strings.HasPrefix(mt, "multipart/") {
		r := multipart.NewReader(body, params["boundary"])
		for {
			sub, err := r.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			p.parts = append(p.parts, readPart(t, sub.Header.Get("Content-Type"), sub.Header.Get("Content-Disposition"), sub.Header.Get("Content-Transfer-Encoding"), sub))
		}
		return p
	}
	var b []byte
	switch cte {
	case "quoted-printable":
		b, err = io.ReadAll(qpReader(body))
	case "base64":
		b, err = io.ReadAll(b64Reader(body))
	default:
		b, err = io.ReadAll(body)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.body = string(b)
	return p
}

func TestBuild(t *testing.T) {
	m := newLog(t)
	ctx := context.Background()

	// Text only: one part.
	b, err := m.build(ctx, Message{To: []string{"ada@example.com"}, Subject: "Hi", Text: "Hello,\nAda"})
	if err != nil {
		t.Fatal(err)
	}
	h, p := parse(t, b.raw)
	if h.Header.Get("From") != `"Pets" <hello@pets.example>` || h.Header.Get("To") != "<ada@example.com>" || h.Header.Get("Subject") != "Hi" ||
		h.Header.Get("Mime-Version") != "1.0" || !strings.HasSuffix(h.Header.Get("Message-Id"), "@pets.example>") || h.Header.Get("Date") == "" {
		t.Errorf("headers: %v", h.Header)
	}
	if p.ctype != "text/plain" || p.body != "Hello,\r\nAda" {
		t.Errorf("body: %+v", p)
	}

	// HTML from a node: the text part made from it, multipart/alternative;
	// Bcc a recipient but no header; names and the subject encoded.
	html := ui.El("div", nil, ui.El("h1", nil, ui.Text("Welcome, Ada & co")),
		ui.El("p", nil, ui.Text("Your pet is "), ui.El("a", []ui.Attr{{Name: "href", Value: "https://pets.example/pets/1"}}, ui.Text("Rex")), ui.Text(".")),
		ui.El("ul", nil, ui.El("li", nil, ui.Text("one")), ui.El("li", nil, ui.Text("two"))))
	b, err = m.build(ctx, Message{From: "Zoë <zoe@pets.example>", To: []string{"Ada Lovelace <ada@example.com>", "bob@example.com"}, Cc: []string{"carol@example.com"},
		Bcc: []string{"audit@pets.example", "bob@example.com"}, ReplyTo: "help@pets.example", Subject: "Karibu — welcome", HTML: html,
		Headers: map[string]string{"List-Unsubscribe": "<https://pets.example/u>", "x-campaign": "spring"}})
	if err != nil {
		t.Fatal(err)
	}
	h, p = parse(t, b.raw)
	dec := new(mime.WordDecoder)
	from, _ := dec.DecodeHeader(h.Header.Get("From"))
	subject, _ := dec.DecodeHeader(h.Header.Get("Subject"))
	if from != `Zoë <zoe@pets.example>` || !strings.HasPrefix(h.Header.Get("From"), "=?utf-8?q?Zo=C3=AB?=") || subject != "Karibu — welcome" || !strings.HasPrefix(h.Header.Get("Subject"), "=?utf-8?q?") {
		t.Errorf("encoded headers: %q %q (%q)", from, subject, h.Header.Get("Subject"))
	}
	if h.Header.Get("To") != `"Ada Lovelace" <ada@example.com>, <bob@example.com>` || h.Header.Get("Cc") != "<carol@example.com>" ||
		h.Header.Get("Bcc") != "" || h.Header.Get("Reply-To") != "<help@pets.example>" ||
		h.Header.Get("List-Unsubscribe") != "<https://pets.example/u>" || h.Header.Get("X-Campaign") != "spring" {
		t.Errorf("headers: %v", h.Header)
	}
	if got := strings.Join(b.rcpts, " "); got != "ada@example.com bob@example.com carol@example.com audit@pets.example" {
		t.Errorf("recipients: %s", got)
	}
	if p.ctype != "multipart/alternative" || len(p.parts) != 2 || p.parts[0].ctype != "text/plain" || p.parts[1].ctype != "text/html" {
		t.Fatalf("structure: %+v", p)
	}
	if want := "Welcome, Ada & co\r\n\r\nYour pet is Rex (https://pets.example/pets/1).\r\n\r\n- one\r\n- two"; p.parts[0].body != want {
		t.Errorf("text:\n%q\nwant\n%q", p.parts[0].body, want)
	}
	if !strings.Contains(p.parts[1].body, `<h1>Welcome, Ada &amp; co</h1>`) {
		t.Errorf("html: %s", p.parts[1].body)
	}

	// Attachments: multipart/mixed around the body.
	big := bytes.Repeat([]byte("0123456789"), 30)
	b, err = m.build(ctx, Message{To: []string{"ada@example.com"}, Subject: "Report", Text: "attached", HTML: ui.Safe("<p>attached</p>"),
		Attachments: []Attachment{{Filename: "report.csv", Data: []byte("a,b\n1,2\n")}, {Filename: "données.bin", Data: big}, {Filename: "x", ContentType: "image/png", Data: []byte{0x89, 'P'}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, p = parse(t, b.raw)
	if p.ctype != "multipart/mixed" || len(p.parts) != 4 || p.parts[0].ctype != "multipart/alternative" || len(p.parts[0].parts) != 2 {
		t.Fatalf("structure: %+v", p)
	}
	csv, bin, png := p.parts[1], p.parts[2], p.parts[3]
	if csv.ctype != "text/csv" || csv.body != "a,b\n1,2\n" || csv.disposition != `attachment; filename=report.csv` {
		t.Errorf("csv: %+v", csv)
	}
	if bin.ctype != "application/octet-stream" || bin.body != string(big) || bin.disposition != `attachment; filename*=utf-8''donn%C3%A9es.bin` {
		t.Errorf("bin: %+v", bin)
	}
	if png.ctype != "image/png" || png.body != "\x89P" {
		t.Errorf("png: %+v", png)
	}
	for _, line := range strings.Split(string(b.raw), "\r\n") {
		if len(line) > 78 {
			t.Errorf("a line of %d bytes: %s", len(line), line)
		}
	}

	// HTML only, given as text.
	b, _ = m.build(ctx, Message{To: []string{"ada@example.com"}, HTML: ui.Safe("<p>Hi <b>there</b></p><script>alert(1)</script>")})
	if b.text != "Hi there" {
		t.Errorf("text from html: %q", b.text)
	}
}

func TestRefused(t *testing.T) {
	m := newLog(t)
	noFrom, _ := New(Config{Logger: quiet()})
	to := []string{"ada@example.com"}
	for name, c := range map[string]struct {
		m    *Mailer
		msg  Message
		want string
	}{
		"subject":     {m, Message{To: to, Subject: "Hi\r\nBcc: victim@example.com"}, "Subject has a line break (header injection refused)"},
		"subject lf":  {m, Message{To: to, Subject: "Hi\nX: y"}, "header injection refused"},
		"to":          {m, Message{To: []string{"ada@example.com\r\nBcc: victim@example.com"}}, "To has a line break"},
		"name":        {m, Message{To: []string{"Ada\n <ada@example.com>"}}, "To has a line break"},
		"from":        {m, Message{From: "x@example.com\r\nBcc: y@example.com", To: to}, "From has a line break"},
		"reply-to":    {m, Message{To: to, ReplyTo: "a@example.com\rB: c"}, "ReplyTo has a line break"},
		"header":      {m, Message{To: to, Headers: map[string]string{"X-Note": "a\r\nBcc: victim@example.com"}}, "header X-Note has a line break"},
		"header name": {m, Message{To: to, Headers: map[string]string{"X-A: b\r\nBcc": "c"}}, "isn't a valid name"},
		"own header":  {m, Message{To: to, Headers: map[string]string{"bcc": "victim@example.com"}}, "header bcc is the message's own"},
		"ctype":       {m, Message{To: to, Headers: map[string]string{"Content-Type": "text/html"}}, "is the message's own"},
		"filename":    {m, Message{To: to, Attachments: []Attachment{{Filename: "a\r\n.txt"}}}, "attachment a\r\n.txt has a line break"},
		"no filename": {m, Message{To: to, Attachments: []Attachment{{Data: []byte("x")}}}, "attachment 1 has no Filename"},
		"ctype bad":   {m, Message{To: to, Attachments: []Attachment{{Filename: "a", ContentType: "not a type"}}}, `content type "not a type"`},
		"address":     {m, Message{To: []string{"not an address"}}, `To: "not an address" isn't an email address`},
		"cc":          {m, Message{To: to, Cc: []string{"@"}}, `Cc: "@"`},
		"nobody":      {m, Message{Subject: "Hi"}, `"Hi" has no recipient`},
		"no sender":   {noFrom, Message{To: to}, "no sender: set the message's From, or MAIL_FROM"},
	} {
		err := c.m.Send(context.Background(), c.msg)
		var inv *InvalidError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", name, err, c.want)
		}
	}
	if len(m.Sent()) != 0 {
		t.Error("a refused message was kept")
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Driver: "sendgrid"}, `unknown driver "sendgrid"`},
		{Config{Driver: "smtp"}, "needs a host (MAIL_HOST)"},
		{Config{Driver: "smtp", Host: "h", Encryption: "maybe"}, `unknown encryption "maybe"`},
		{Config{Driver: "smtp", Host: "h", Port: 70000}, "port 70000"},
		{Config{From: "nobody"}, `MAIL_FROM: "nobody" isn't an email address`},
		{Config{From: "a@example.com", FromName: "A\r\nBcc: b@example.com"}, "MAIL_FROM_NAME has a line break"},
	} {
		if _, err := New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v; want %q", c.cfg, err, c.want)
		}
	}
	m, err := New(Config{From: "app@example.com", FromName: "App"})
	if err != nil || m.from.String() != `"App" <app@example.com>` || m.Driver() != "log" {
		t.Errorf("%v %v", m.from, err)
	}
	for enc, addr := range map[string]string{"": "h:587", "starttls": "h:587", "tls": "h:465", "SSL": "h:465", "none": "h:25"} {
		tr, err := newSMTP(Config{Host: "h", Encryption: enc})
		if err != nil || tr.addr != addr {
			t.Errorf("%q: %v %v", enc, tr, err)
		}
	}
}

func TestLogDriver(t *testing.T) {
	var out bytes.Buffer
	m, _ := New(Config{From: "app@example.com", Logger: slog.New(slog.NewTextHandler(&out, nil))})
	err := m.Send(context.Background(), Message{To: []string{"ada@example.com"}, Bcc: []string{"b@example.com"}, Subject: "Hi", HTML: ui.Safe("<p>Hello</p>")})
	if err != nil {
		t.Fatal(err)
	}
	s := m.Sent()
	if len(s) != 1 || s[0].Subject != "Hi" || s[0].HTML != "<p>Hello</p>" || s[0].Text != "Hello" || s[0].From != "<app@example.com>" ||
		strings.Join(s[0].Recipients, ",") != "ada@example.com,b@example.com" || !bytes.Contains(s[0].Raw, []byte("Subject: Hi\r\n")) {
		t.Errorf("sent: %+v", s)
	}
	if !strings.Contains(out.String(), `msg="mail not sent (log driver)" from=<app@example.com> to="ada@example.com, b@example.com" subject=Hi attachments=0 text=Hello`) {
		t.Errorf("log: %s", out.String())
	}
	m.Reset()
	if len(m.Sent()) != 0 {
		t.Error("Reset")
	}
}

func TestHTMLToText(t *testing.T) {
	for in, want := range map[string]string{
		`<html><head><title>T</title><style>p{}</style></head><body><p>One</p><p>Two<br>Three</p></body></html>`: "One\n\nTwo\nThree",
		`<p>A   lot
		of    space</p>`: "A lot of space",
		`<a href="https://x.example/a?b=1&amp;c=2">here</a>`:              "here (https://x.example/a?b=1&c=2)",
		`<a href="https://x.example/">https://x.example/</a>`:             "https://x.example/",
		`<a href="/relative">link</a> <a href='mailto:a@b'>mail</a>`:      "link mail",
		`<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>`: "a b\n\nc",
		`Tom &amp; Jerry &lt;3 &#x2603;<!-- hidden -->`:                   "Tom & Jerry <3 ☃",
		`<p data-x="a>b">quoted</p>`:                                      "quoted",
	} {
		if got := htmlToText(in); got != want {
			t.Errorf("%s:\n got %q\nwant %q", in, got, want)
		}
	}
}
