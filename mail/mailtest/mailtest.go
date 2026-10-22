// Package mailtest tests the mail an app sends: a quiet log-driver Mailer
// to provide, and helpers reading what it kept.
//
//	m := mailtest.New()
//	app := web.New(web.Provide(m))
//	… sign up …
//	msg := mailtest.Last(t, m)
//	if msg.Recipients[0] != "ada@example.com" || !strings.Contains(msg.Text, "Welcome") { … }
package mailtest

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/vuka-lang/web/v2/mail"
)

// New is a log-driver Mailer that logs nothing, its sender
// test@example.com unless cfg says otherwise.
func New(cfg ...mail.Config) *mail.Mailer {
	var c mail.Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	c.Driver = "log"
	if c.From == "" {
		c.From = "test@example.com"
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m, err := mail.New(c)
	if err != nil {
		panic(err)
	}
	return m
}

// Sent waits for the messages SendLater queued, then returns every message
// m kept.
func Sent(t testing.TB, m *mail.Mailer) []mail.Sent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("mailtest: messages still queued: %v", err)
	}
	return m.Sent()
}

// Last is the last message m kept; the test fails when there is none.
func Last(t testing.TB, m *mail.Mailer) mail.Sent {
	t.Helper()
	all := Sent(t, m)
	if len(all) == 0 {
		t.Fatal("mailtest: no mail was sent")
	}
	return all[len(all)-1]
}

// To is the messages m kept that went to addr (To, Cc or Bcc).
func To(t testing.TB, m *mail.Mailer, addr string) []mail.Sent {
	t.Helper()
	var out []mail.Sent
	for _, s := range Sent(t, m) {
		if slices.Contains(s.Recipients, addr) {
			out = append(out, s)
		}
	}
	return out
}
