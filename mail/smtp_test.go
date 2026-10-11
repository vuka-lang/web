package mail

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func qpReader(r io.Reader) io.Reader  { return quotedprintable.NewReader(r) }
func b64Reader(r io.Reader) io.Reader { return base64.NewDecoder(base64.StdEncoding, r) }

// testCert is a certificate for 127.0.0.1, and a client config trusting it.
func testCert(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, &tls.Config{RootCAs: pool}
}

// fakeSMTP is an SMTP server in the test: EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA, QUIT — what net/smtp says.
type fakeSMTP struct {
	addr       string
	port       int
	tls        *tls.Config
	implicit   bool // TLS from the start
	starttls   bool // offers STARTTLS
	user, pass string
	silent     bool // never greets
	failRcpt   int  // answer 451 to the first n RCPTs
	rejectRcpt bool // answer 550 to every RCPT

	mu   sync.Mutex
	got  []received
	rcpt int
}

type received struct {
	from     string
	to       []string
	data     string
	user     string
	tls      bool
	commands []string
}

func (f *fakeSMTP) start(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.addr = ln.Addr().String()
	_, p, _ := net.SplitHostPort(f.addr)
	f.port, _ = strconv.Atoi(p)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.got...)
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	secure := false
	if f.implicit {
		c = tls.Server(c, f.tls)
		secure = true
	}
	if f.silent {
		io.Copy(io.Discard, c)
		return
	}
	tp := textproto.NewConn(c)
	tp.PrintfLine("220 fake ESMTP")
	var r received
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		r.commands = append(r.commands, strings.ToUpper(verb))
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			lines := []string{"fake"}
			if f.starttls && !secure {
				lines = append(lines, "STARTTLS")
			}
			if f.user != "" {
				lines = append(lines, "AUTH PLAIN")
			}
			lines = append(lines, "8BITMIME")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				tp.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			tp.PrintfLine("220 go ahead")
			tc := tls.Server(c, f.tls)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, secure = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			mech, resp, _ := strings.Cut(arg, " ")
			b, _ := base64.StdEncoding.DecodeString(resp)
			parts := strings.Split(string(b), "\x00")
			if mech != "PLAIN" || len(parts) != 3 || parts[1] != f.user || parts[2] != f.pass {
				tp.PrintfLine("535 5.7.8 authentication failed")
				continue
			}
			r.user = parts[1]
			tp.PrintfLine("235 2.7.0 ok")
		case "MAIL":
			r.from = strings.TrimSuffix(strings.TrimPrefix(strings.Fields(arg)[0], "FROM:<"), ">")
			tp.PrintfLine("250 ok")
		case "RCPT":
			f.mu.Lock()
			fail := f.rcpt < f.failRcpt
			f.rcpt++
			f.mu.Unlock()
			switch {
			case f.rejectRcpt:
				tp.PrintfLine("550 5.1.1 no such user")
			case fail:
				tp.PrintfLine("451 4.3.0 try again later")
			default:
				r.to = append(r.to, strings.Trim(strings.TrimPrefix(arg, "TO:"), "<>"))
				tp.PrintfLine("250 ok")
			}
		case "DATA":
			tp.PrintfLine("354 go on")
			b, err := io.ReadAll(tp.DotReader())
			if err != nil {
				return
			}
			r.data, r.tls = string(b), secure
			f.mu.Lock()
			f.got = append(f.got, r)
			f.mu.Unlock()
			tp.PrintfLine("250 queued")
		case "RSET", "NOOP":
			tp.PrintfLine("250 ok")
		case "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 unknown")
		}
	}
}

func (f *fakeSMTP) rcpts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rcpt
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func smtpMailer(t *testing.T, f *fakeSMTP, client *tls.Config, enc string, mut ...func(*Config)) *Mailer {
	t.Helper()
	cfg := Config{Driver: "smtp", Host: "127.0.0.1", Port: f.port, Username: f.user, Password: f.pass, Encryption: enc, TLS: client,
		From: "app@example.com", Timeout: 2 * time.Second, Logger: quiet(), Backoff: time.Millisecond}
	for _, f := range mut {
		f(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var welcome = Message{To: []string{"ada@example.com"}, Bcc: []string{"audit@example.com"}, Subject: "Welcome", Text: "Hi Ada"}

func TestSMTPStartTLS(t *testing.T) {
	server, client := testCert(t)
	f := (&fakeSMTP{tls: server, starttls: true, user: "app", pass: "s3cret"}).start(t)
	m := smtpMailer(t, f, client, "starttls")
	if err := m.Send(context.Background(), welcome); err != nil {
		t.Fatal(err)
	}
	got := f.received()
	if len(got) != 1 {
		t.Fatalf("received %d", len(got))
	}
	r := got[0] // its data read by a dot reader: LF line ends
	if !r.tls || r.user != "app" || r.from != "app@example.com" || strings.Join(r.to, ",") != "ada@example.com,audit@example.com" ||
		!strings.Contains(r.data, "Subject: Welcome\n") || !strings.Contains(r.data, "Hi Ada") || strings.Contains(r.data, "audit@") {
		t.Errorf("received: %+v", r)
	}
	if want := "EHLO STARTTLS EHLO AUTH MAIL RCPT RCPT DATA"; strings.Join(r.commands, " ") != want {
		t.Errorf("commands: %s; want %s", strings.Join(r.commands, " "), want)
	}
	if len(m.Sent()) != 0 {
		t.Error("the smtp driver kept a message")
	}

	// A wrong password, an untrusted certificate, no STARTTLS offered.
	bad := smtpMailer(t, f, client, "starttls", func(c *Config) { c.Password = "nope" })
	if err := bad.Send(context.Background(), welcome); err == nil || !strings.Contains(err.Error(), "AUTH: 535") {
		t.Errorf("a wrong password: %v", err)
	}
	untrusted := smtpMailer(t, f, nil, "starttls")
	if err := untrusted.Send(context.Background(), welcome); err == nil || !strings.Contains(err.Error(), "STARTTLS") || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an untrusted certificate: %v", err)
	}
	plain := (&fakeSMTP{}).start(t)
	if err := smtpMailer(t, plain, client, "").Send(context.Background(), welcome); err == nil || !strings.Contains(err.Error(), "doesn't offer STARTTLS; for a local mail catcher set MAIL_ENCRYPTION=none") {
		t.Errorf("no STARTTLS: %v", err)
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	server, client := testCert(t)
	f := (&fakeSMTP{tls: server, implicit: true, user: "app", pass: "pw"}).start(t)
	if err := smtpMailer(t, f, client, "tls").Send(context.Background(), welcome); err != nil {
		t.Fatal(err)
	}
	if r := f.received(); len(r) != 1 || !r[0].tls || r[0].user != "app" || strings.Join(r[0].commands, " ") != "EHLO AUTH MAIL RCPT RCPT DATA" {
		t.Errorf("received: %+v", r)
	}
}

func TestSMTPNone(t *testing.T) {
	// A local catcher: no TLS; PLAIN only because the host is 127.0.0.1.
	f := (&fakeSMTP{user: "u", pass: "p"}).start(t)
	if err := smtpMailer(t, f, nil, "none").Send(context.Background(), welcome); err != nil {
		t.Fatal(err)
	}
	if r := f.received(); len(r) != 1 || r[0].tls || r[0].user != "u" {
		t.Errorf("received: %+v", r)
	}
	nobody := (&fakeSMTP{}).start(t)
	if err := smtpMailer(t, nobody, nil, "none").Send(context.Background(), welcome); err != nil {
		t.Fatal(err)
	}
}

func TestSMTPTimeout(t *testing.T) {
	f := (&fakeSMTP{silent: true}).start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := smtpMailer(t, f, nil, "none").Send(ctx, welcome)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a silent server: %v after %s", err, time.Since(start))
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("not a timeout: %v", err)
	}
	// Nothing listening: the dial fails at once.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	m := smtpMailer(t, &fakeSMTP{port: port}, nil, "none")
	if err := m.Send(context.Background(), welcome); err == nil || !strings.Contains(err.Error(), "connection refused") || !temporary(err) {
		t.Errorf("refused: %v", err)
	}
}

func TestSendLater(t *testing.T) {
	f := (&fakeSMTP{failRcpt: 2}).start(t)
	m := smtpMailer(t, f, nil, "none")
	if err := m.SendLater(context.Background(), Message{To: []string{"ada@example.com"}, Subject: "Later"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SendLater(context.Background(), Message{Subject: "nobody"}); err == nil {
		t.Error("an invalid message was queued")
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.received(); len(r) != 1 || !strings.Contains(r[0].data, "Subject: Later") {
		t.Errorf("after two 451s: %+v", r)
	}

	// A permanent failure isn't retried; it is logged.
	var logs syncBuffer
	rej := (&fakeSMTP{rejectRcpt: true}).start(t)
	m = smtpMailer(t, rej, nil, "none", func(c *Config) { c.Logger = slogTo(&logs) })
	m.SendLater(context.Background(), welcome)
	m.Flush(context.Background())
	if rej.rcpts() != 1 || !strings.Contains(logs.String(), `msg="mail: SendLater gave up" subject=Welcome`) || !strings.Contains(logs.String(), "attempts=1") {
		t.Errorf("rcpts %d, logs %s", rej.rcpt, logs.String())
	}

	// Stop waits for the queue within its context, then cancels it.
	slow := (&fakeSMTP{failRcpt: 1000}).start(t)
	m = smtpMailer(t, slow, nil, "none", func(c *Config) { c.Backoff = time.Hour; c.Logger = quiet() })
	m.SendLater(context.Background(), welcome)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Stop(ctx); err == nil || !strings.Contains(err.Error(), "1 messages still queued, cancelled") {
		t.Errorf("Stop: %v", err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Error(err)
	}
	if err := m.SendLater(context.Background(), welcome); err == nil {
		t.Error("SendLater after Stop")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// blockingTransport holds every message until released.
type blockingTransport struct{ release chan struct{} }

func (b blockingTransport) Send(ctx context.Context, from string, to []string, msg []byte) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

// SendLater holds at most Config.Queue messages: past it, it refuses.
func TestSendLaterQueueBound(t *testing.T) {
	tr := blockingTransport{release: make(chan struct{})}
	m, err := New(Config{Transport: tr, From: "app@example.com", Queue: 10})
	if err != nil {
		t.Fatal(err)
	}
	msg := Message{To: []string{"ada@example.com"}, Subject: "Later"}
	accepted, refused := 0, 0
	for range 50 {
		switch err := m.SendLater(context.Background(), msg); {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQueueFull):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 10 || refused != 40 {
		t.Errorf("accepted %d, refused %d; want 10 and 40", accepted, refused)
	}
	close(tr.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.SendLater(context.Background(), msg); err != nil {
		t.Errorf("after the queue drained: %v", err)
	}
	m.Flush(ctx)
}
