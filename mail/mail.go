// Package mail sends email from a web app: a Mailer service apps inject,
// configured from the environment (or .env), with a log driver for
// development and tests and an SMTP driver for production.
//
//	app := web.New(mail.Provide())   // MAIL_DRIVER, MAIL_HOST, … from the environment
//
//	@web.Post("/signup")
//	func Signup(ctx context.Context, in NewUser, m *mail.Mailer) error {
//		return m.Send(ctx, mail.Message{
//			To:      []string{in.Email},
//			Subject: "Welcome",
//			HTML:    <Welcome name={in.Name} />,   // the text part is made from it
//		})
//	}
//
// The default driver, log, sends nothing: it logs each message and keeps it
// for Sent. In development the toolbar's Mail panel shows the messages a
// request sent, the HTML in a preview.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	netmail "net/mail"
	"strings"
	"sync"
	"time"

	"github.com/vuka-lang/ui/v2"
	"github.com/vuka-lang/web/v2"
)

// Config configures a Mailer. ConfigFromEnv reads it from the environment.
type Config struct {
	// Driver is "log" (the default: log the message, send nothing) or
	// "smtp". Transport, when set, sends instead of either.
	Driver    string
	Transport Transport

	Host     string
	Port     int // 0: 465 with tls, 587 with starttls, 25 with none
	Username string
	Password string
	// Encryption is "starttls" (the default: upgrade the connection, and
	// fail when the server can't), "tls" (TLS from the start, port 465) or
	// "none" (local catchers only; the password isn't sent unencrypted
	// except to localhost).
	Encryption string
	TLS        *tls.Config // a private CA, or a test server's certificate
	LocalName  string      // the name the client gives in EHLO; "localhost"
	// Timeout bounds dialing the server, and each send when the context has
	// no deadline of its own (six times as long). Default 10s.
	Timeout time.Duration

	From     string // the sender when a message names none: "app@example.com" or "App <app@example.com>"
	FromName string // its name, when From has none

	// SendLater's retries after the first attempt (default 3) and the wait
	// before the first, doubling after each (default 2s). Only temporary
	// failures are retried: a 4xx answer, a connection that fails.
	Retries int
	Backoff time.Duration
	// Queue bounds the messages SendLater holds at once, sending or
	// waiting (default 1000): past it, SendLater refuses with ErrQueueFull
	// rather than hold more in memory.
	Queue int

	Logger *slog.Logger // the log driver's and SendLater's; slog's default
}

// ConfigFromEnv reads the configuration from the environment (and the env
// files web.New loaded): MAIL_DRIVER, MAIL_HOST, MAIL_PORT, MAIL_USERNAME,
// MAIL_PASSWORD, MAIL_ENCRYPTION, MAIL_FROM, MAIL_FROM_NAME. A MAIL_PORT
// that isn't a number fails the app's next Build or Start, naming it.
func ConfigFromEnv() Config {
	return Config{
		Driver:     web.Env[string]("MAIL_DRIVER"),
		Host:       web.Env[string]("MAIL_HOST"),
		Port:       web.Env[int]("MAIL_PORT"),
		Username:   web.Env[string]("MAIL_USERNAME"),
		Password:   web.Env[string]("MAIL_PASSWORD"),
		Encryption: web.Env[string]("MAIL_ENCRYPTION"),
		From:       web.Env[string]("MAIL_FROM"),
		FromName:   web.Env[string]("MAIL_FROM_NAME"),
	}
}

// Provide provides a *Mailer to the app: from cfg when given, else from
// the environment, read when the Mailer is first needed (after web.New
// loaded the env files).
func Provide(cfg ...Config) web.Option {
	return web.Provide(func() (*Mailer, error) {
		if len(cfg) > 0 {
			return New(cfg[0])
		}
		return New(ConfigFromEnv())
	})
}

// Transport sends a message built by the Mailer: from is the envelope
// sender, to every recipient (Bcc included), msg the whole MIME message.
// A Config's Transport replaces the driver, for an HTTP API, say.
type Transport interface {
	Send(ctx context.Context, from string, to []string, msg []byte) error
}

// Mailer sends messages. It is safe for concurrent use.
type Mailer struct {
	cfg    Config
	from   *netmail.Address // the default sender; nil when none
	driver string
	send   func(ctx context.Context, from string, to []string, msg []byte) error
	log    func() *slog.Logger

	mu      sync.Mutex
	sent    []Sent // the log driver's
	later   sync.WaitGroup
	pending int
	stopped bool
	quit    context.Context // cancelled when Stop gives up waiting
	abandon context.CancelFunc
	slots   chan struct{} // SendLater's deliveries at once
}

// New makes a Mailer, checking cfg.
func New(cfg Config) (*Mailer, error) {
	m := &Mailer{cfg: cfg, slots: make(chan struct{}, 4)}
	m.quit, m.abandon = context.WithCancel(context.Background())
	m.log = func() *slog.Logger {
		if cfg.Logger != nil {
			return cfg.Logger
		}
		return slog.Default()
	}
	if cfg.From != "" {
		a, err := parseAddress("MAIL_FROM", cfg.From)
		if err != nil {
			return nil, err
		}
		if a.Name == "" {
			if err := clean("MAIL_FROM_NAME", cfg.FromName); err != nil {
				return nil, err
			}
			a.Name = cfg.FromName
		}
		m.from = a
	}
	switch {
	case cfg.Transport != nil:
		m.driver, m.send = "custom", cfg.Transport.Send
	case cfg.Driver == "" || strings.EqualFold(cfg.Driver, "log"):
		m.driver, m.send = "log", nil
	case strings.EqualFold(cfg.Driver, "smtp"):
		t, err := newSMTP(cfg)
		if err != nil {
			return nil, err
		}
		m.driver, m.send = "smtp", t.Send
	default:
		return nil, fmt.Errorf("mail: unknown driver %q (MAIL_DRIVER): log or smtp", cfg.Driver)
	}
	if cfg.Retries == 0 {
		m.cfg.Retries = 3
	}
	if cfg.Backoff == 0 {
		m.cfg.Backoff = 2 * time.Second
	}
	return m, nil
}

// Driver is the driver sending: "log", "smtp" or "custom" (a Transport).
func (m *Mailer) Driver() string { return m.driver }

// Message is an email. Every address is "user@example.com" or
// "Name <user@example.com>".
type Message struct {
	From    string // the Mailer's From when empty
	To      []string
	Cc      []string
	Bcc     []string // receive it without being listed in it
	ReplyTo string
	Subject string
	// Text is the plain-text body; made from HTML when empty.
	Text string
	// HTML is the HTML body: a JSX or templ component, or ui.Safe(html)
	// for HTML you have as a string. It renders with Send's context.
	HTML        ui.Node
	Headers     map[string]string // more headers: List-Unsubscribe, X-Campaign, …
	Attachments []Attachment
}

// Attachment is a file sent with a message.
type Attachment struct {
	Filename    string
	ContentType string // from the file name's extension when empty
	Data        []byte
}

// Sent is a message the log driver kept: as given, and as built.
type Sent struct {
	Message
	From       string   // the sender, as written in the message
	Recipients []string // every address it went to, Bcc included
	HTML, Text string   // the bodies, rendered
	Raw        []byte   // the MIME message
	At         time.Time
}

// Send builds the message and sends it with the Mailer's driver. A message
// that isn't valid — no recipient, an address that doesn't parse, a line
// break in a header — is refused before anything is sent.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	b, err := m.build(ctx, msg)
	if err != nil {
		noteFailed(ctx, m.driver, msg, err)
		return err
	}
	n := note(ctx, m.driver, b, "sending")
	err = m.deliver(ctx, b)
	n.done(err)
	return err
}

func (m *Mailer) deliver(ctx context.Context, b *built) error {
	if m.send == nil { // the log driver
		m.mu.Lock()
		if len(m.sent) >= 1000 {
			m.sent = m.sent[1:]
		}
		m.sent = append(m.sent, b.sent())
		m.mu.Unlock()
		m.log().InfoContext(ctx, "mail not sent (log driver)", "from", b.from.String(), "to", strings.Join(b.rcpts, ", "),
			"subject", b.msg.Subject, "attachments", len(b.msg.Attachments), "text", b.text)
		return nil
	}
	if err := m.send(ctx, b.from.Address, b.rcpts, b.raw); err != nil {
		return fmt.Errorf("mail: sending %q to %s: %w", b.msg.Subject, strings.Join(b.rcpts, ", "), err)
	}
	return nil
}

// Sent is what the log driver kept, oldest first (the latest 1000); nil
// for another driver.
func (m *Mailer) Sent() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sent(nil), m.sent...)
}

// Reset forgets what the log driver kept.
func (m *Mailer) Reset() {
	m.mu.Lock()
	m.sent = nil
	m.mu.Unlock()
}

// ErrQueueFull is SendLater's error when Config.Queue messages are waiting
// already.
var ErrQueueFull = errors.New("mail: the queue is full")

func (m *Mailer) queue() int {
	if m.cfg.Queue > 0 {
		return m.cfg.Queue
	}
	return 1000
}

// SendLater builds the message now — so it is refused now when invalid,
// and its HTML renders with ctx — and sends it in the background, retrying
// temporary failures (Config.Retries, Config.Backoff). A failure after the
// last attempt is logged. Stop waits for the messages still queued. Past
// Config.Queue messages held at once, it refuses with ErrQueueFull.
func (m *Mailer) SendLater(ctx context.Context, msg Message) error {
	b, err := m.build(ctx, msg)
	if err != nil {
		noteFailed(ctx, m.driver, msg, err)
		return err
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return errors.New("mail: the mailer has stopped")
	}
	if m.pending >= m.queue() {
		m.mu.Unlock()
		noteFailed(ctx, m.driver, msg, ErrQueueFull)
		return ErrQueueFull
	}
	m.pending++
	m.later.Add(1)
	m.mu.Unlock()
	n := note(ctx, m.driver, b, "queued")
	go func() {
		defer func() {
			m.mu.Lock()
			m.pending--
			m.mu.Unlock()
			m.later.Done()
		}()
		m.slots <- struct{}{}
		defer func() { <-m.slots }()
		ctx, cancel := context.WithCancel(context.WithoutCancel(ctx)) // the request may end first
		defer cancel()
		defer context.AfterFunc(m.quit, cancel)()
		wait := m.cfg.Backoff
		for attempt := 0; ; attempt++ {
			err := m.deliver(ctx, b)
			if err == nil || attempt >= m.cfg.Retries || !temporary(err) || ctx.Err() != nil {
				n.done(err)
				if err != nil {
					m.log().ErrorContext(ctx, "mail: SendLater gave up", "subject", b.msg.Subject, "to", strings.Join(b.rcpts, ", "), "attempts", attempt+1, "err", err)
				}
				return
			}
			n.retrying(err, attempt+1)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
			wait *= 2
		}
	}()
	return nil
}

// Flush waits until the messages SendLater queued are sent or given up,
// or ctx is done: for tests.
func (m *Mailer) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.later.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop refuses SendLater from now on and waits for the messages queued
// until ctx is done; then it cancels them. The app calls it on Stop.
func (m *Mailer) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	m.mu.Unlock()
	if err := m.Flush(ctx); err != nil {
		m.mu.Lock()
		n := m.pending
		m.mu.Unlock()
		m.abandon()
		return fmt.Errorf("mail: %d messages still queued, cancelled: %w", n, err)
	}
	return nil
}
