package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// smtpTransport sends over SMTP with net/smtp: implicit TLS, STARTTLS or
// neither, and PLAIN authentication.
type smtpTransport struct {
	host, addr, enc string
	user, pass      string
	local           string
	timeout         time.Duration
	tls             *tls.Config
}

func newSMTP(cfg Config) (*smtpTransport, error) {
	if cfg.Host == "" {
		return nil, errors.New("mail: the smtp driver needs a host (MAIL_HOST)")
	}
	t := &smtpTransport{host: cfg.Host, enc: strings.ToLower(cfg.Encryption), user: cfg.Username, pass: cfg.Password,
		local: cfg.LocalName, timeout: cfg.Timeout}
	port := cfg.Port
	switch t.enc {
	case "", "starttls":
		t.enc = "starttls"
		port = cmpOr(port, 587)
	case "tls", "ssl":
		t.enc = "tls"
		port = cmpOr(port, 465)
	case "none":
		port = cmpOr(port, 25)
	default:
		return nil, fmt.Errorf("mail: unknown encryption %q (MAIL_ENCRYPTION): none, starttls or tls", cfg.Encryption)
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("mail: port %d (MAIL_PORT) is out of range", port)
	}
	t.addr = net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	if t.local == "" {
		t.local = "localhost"
	}
	if t.timeout <= 0 {
		t.timeout = 10 * time.Second
	}
	t.tls = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLS != nil {
		t.tls = cfg.TLS.Clone()
	}
	if t.tls.ServerName == "" {
		t.tls.ServerName = cfg.Host
	}
	return t, nil
}

func cmpOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func (t *smtpTransport) Send(ctx context.Context, from string, to []string, msg []byte) (err error) {
	d := &net.Dialer{Timeout: t.timeout}
	var conn net.Conn
	if t.enc == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: t.tls}).DialContext(ctx, "tcp", t.addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", t.addr)
	}
	if err != nil {
		return fmt.Errorf("smtp %s: %w", t.addr, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(6 * t.timeout)
	}
	conn.SetDeadline(deadline)
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })()
	c, err := smtp.NewClient(conn, t.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp %s: %w", t.addr, err)
	}
	defer c.Close()
	step := func(what string, err error) error {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("smtp %s: %s: %w", t.addr, what, err)
		}
		return nil
	}
	if err := step("EHLO", c.Hello(t.local)); err != nil {
		return err
	}
	if t.enc == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp %s: the server doesn't offer STARTTLS; for a local mail catcher set MAIL_ENCRYPTION=none", t.addr)
		}
		if err := step("STARTTLS", c.StartTLS(t.tls)); err != nil {
			return err
		}
	}
	if t.user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp %s: the server doesn't offer AUTH, and a username is set", t.addr)
		}
		if err := step("AUTH", c.Auth(smtp.PlainAuth("", t.user, t.pass, t.host))); err != nil {
			return err
		}
	}
	if err := step("MAIL FROM", c.Mail(from)); err != nil {
		return err
	}
	for _, r := range to {
		if err := step("RCPT TO "+r, c.Rcpt(r)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err := step("DATA", err); err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return step("DATA", err)
	}
	if err := step("DATA", w.Close()); err != nil {
		return err
	}
	return step("QUIT", c.Quit())
}

// temporary reports whether a failure may pass on a retry: a 4xx answer,
// a connection that failed or timed out.
func temporary(err error) bool {
	var invalid *InvalidError
	if errors.As(err, &invalid) || errors.Is(err, context.Canceled) {
		return false
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code >= 400 && te.Code < 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
