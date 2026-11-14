package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// email sends through an SMTP relay with authentication: submission on
// 587 with STARTTLS, or 465 with TLS from the first byte. It never sends
// in the clear and never logs in over plain text.
type email struct {
	base
	addr     string // host:port
	implicit bool   // port 465 style
	user     string
	password string
	from     *mail.Address
	to       []string
	roots    *x509.CertPool // tests trust their own certificate; nil means the system's
}

func newEmail(b base, c Config) (Channel, error) {
	host, port, err := net.SplitHostPort(c.SMTP)
	if err != nil || host == "" {
		return nil, fmt.Errorf("alert channel %s: smtp must be host:port", c.ID)
	}
	e := &email{base: b, addr: c.SMTP, implicit: port == "465", user: c.Username}
	if e.from, err = mail.ParseAddress(c.From); err != nil {
		return nil, fmt.Errorf("alert channel %s: from: %w", c.ID, err)
	}
	if len(c.To) == 0 {
		return nil, fmt.Errorf("alert channel %s: to needs at least one address", c.ID)
	}
	for _, t := range c.To {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return nil, fmt.Errorf("alert channel %s: to %q: %w", c.ID, t, err)
		}
		e.to = append(e.to, a.Address)
	}
	if e.user == "" {
		e.user = e.from.Address
	}
	if e.password, err = c.secret(c.Secrets.Password, c.PasswordFile, "password"); err != nil {
		return nil, err
	}
	return e, nil
}

// message builds the mail. Kept apart from sending so it can be tested.
func (e *email) message(n Notice, now time.Time) []byte {
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := e.from.Address[strings.LastIndex(e.from.Address, "@")+1:]
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", e.from.String())
	h("To", strings.Join(e.to, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", "[Cairn OBS] "+headline(n)))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("Auto-Submitted", "auto-generated") // RFC 3834: no vacation replies
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	text := body(n)
	if n.URL != "" {
		text += "\n\n" + n.URL
	}
	text += "\n\n-- \nCairn OBS\n"
	b.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
	return []byte(b.String())
}

func (e *email) Send(ctx context.Context, n Notice) error {
	host, _, _ := net.SplitHostPort(e.addr)
	tc := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: e.roots}
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if e.implicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tc}).DialContext(ctx, "tcp", e.addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", e.addr)
	}
	if err != nil {
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	defer c.Close()
	if !e.implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("email %s: the server doesn't offer STARTTLS; refusing to send in the clear", e.id)
		}
		if err := c.StartTLS(tc); err != nil {
			return fmt.Errorf("email %s: STARTTLS: %w", e.id, err)
		}
	}
	// PlainAuth only sends over TLS (or to localhost), which we have by now.
	if err := c.Auth(smtp.PlainAuth("", e.user, e.password, host)); err != nil {
		return fmt.Errorf("email %s: sign-in refused: %w", e.id, err)
	}
	if err := c.Mail(e.from.Address); err != nil {
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	for _, t := range e.to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("email %s: recipient %s: %w", e.id, t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	if _, err := w.Write(e.message(n, time.Now())); err != nil {
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email %s: %w", e.id, err)
	}
	return c.Quit()
}
