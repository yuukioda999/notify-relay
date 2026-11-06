// Package smtpmail sends mail over SMTP. Locally and in the demo the server is Mailpit;
// in production it is a mail service. The code is the same (api/openapi.yaml).
package smtpmail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// TLSMode chooses how the connection is protected.
type TLSMode string

// TLS modes.
const (
	TLSNone     TLSMode = "none"     // local Mailpit only
	TLSStartTLS TLSMode = "starttls" // upgrade after connecting (port 587)
	TLSImplicit TLSMode = "tls"      // TLS from the start (port 465)
)

// Mailer implements usecase.Mailer.
type Mailer struct {
	Addr     string // host:port
	From     string // envelope and header sender
	FromName string
	Username string
	Password string
	TLS      TLSMode
	Timeout  time.Duration
	// Footer is appended to every message (the common layout).
	Footer string
}

var _ usecase.Mailer = (*Mailer)(nil)

// Send delivers one message. Failures are *domain.SendError (AC-R001-10, AC-R001-11).
func (m *Mailer) Send(ctx context.Context, msg usecase.Mail) error {
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := m.compose(msg)
	if err != nil {
		return &domain.SendError{Code: domain.ErrInvalidAddress, Err: err}
	}

	c, err := m.dial(ctx)
	if err != nil {
		return temporary(err)
	}
	defer func() { _ = c.Close() }()

	if err := m.handshake(c); err != nil {
		return classify(stageSession, err)
	}
	if err := c.Mail(m.From); err != nil {
		return classify(stageSender, err)
	}
	if err := c.Rcpt(msg.To); err != nil {
		return classify(stageRecipient, err)
	}
	w, err := c.Data()
	if err != nil {
		return classify(stageMessage, err)
	}
	if _, err := w.Write(data); err != nil {
		return temporary(err)
	}
	if err := w.Close(); err != nil {
		return classify(stageMessage, err)
	}
	_ = c.Quit()
	return nil
}

func (m *Mailer) dial(ctx context.Context) (*smtp.Client, error) {
	host, _, err := net.SplitHostPort(m.Addr)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{}
	var conn net.Conn
	if m.TLS == TLSImplicit {
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
		conn, err = td.DialContext(ctx, "tcp", m.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", m.Addr)
	}
	if err != nil {
		return nil, err
	}
	// net/smtp has no context support, so the deadline bounds the whole conversation.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (m *Mailer) handshake(c *smtp.Client) error {
	host, _, _ := net.SplitHostPort(m.Addr)
	if m.TLS == TLSStartTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if m.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.Username, m.Password, host)); err != nil {
			return err
		}
	}
	return nil
}

// compose builds a UTF-8 plain text message.
func (m *Mailer) compose(msg usecase.Mail) ([]byte, error) {
	if _, err := mail.ParseAddress(msg.To); err != nil {
		return nil, err
	}
	if strings.ContainsAny(msg.Message.Subject, "\r\n") {
		return nil, errors.New("subject contains a line break")
	}

	var body strings.Builder
	body.WriteString(msg.Message.Text)
	if msg.Message.URL != "" {
		body.WriteString("\n\n")
		body.WriteString(msg.Message.URL)
	}
	if m.Footer != "" {
		body.WriteString("\n\n-- \n")
		body.WriteString(m.Footer)
	}

	from := (&mail.Address{Name: m.FromName, Address: m.From}).String()
	var buf bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	header("From", from)
	header("To", msg.To)
	header("Subject", mime.BEncoding.Encode("UTF-8", msg.Message.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+randomID()+"@"+domainOf(m.From)+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	header("Content-Transfer-Encoding", "base64")
	buf.WriteString("\r\n")

	normalized := strings.ReplaceAll(strings.ReplaceAll(body.String(), "\r\n", "\n"), "\n", "\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(normalized))
	for len(enc) > 76 {
		buf.WriteString(enc[:76])
		buf.WriteString("\r\n")
		enc = enc[76:]
	}
	buf.WriteString(enc)
	buf.WriteString("\r\n")
	return buf.Bytes(), nil
}

// stage is the step of the SMTP conversation a reply belongs to.
type stage int

const (
	stageSession   stage = iota // STARTTLS and AUTH: our own setup
	stageSender                 // MAIL FROM
	stageRecipient              // RCPT TO: the only step that judges the address
	stageMessage                // DATA
)

// classify maps SMTP replies to retry decisions: 4xx is temporary, 5xx is permanent.
// Only a refusal at RCPT TO means the address is invalid (AC-R001-11). A failure while
// setting up the session (TLS, authentication) is our configuration, not the recipient's:
// it is retried so that fixing the settings lets the mail go out.
func classify(st stage, err error) error {
	var tp *textproto.Error
	if !errors.As(err, &tp) || st == stageSession {
		return temporary(err)
	}
	switch {
	case tp.Code == 452:
		return &domain.SendError{Code: domain.ErrRateLimited, Temporary: true, Err: err}
	case tp.Code >= 400 && tp.Code < 500:
		return &domain.SendError{Code: domain.ErrProviderUnavailable, Temporary: true, Err: err}
	case st == stageRecipient && (tp.Code == 550 || tp.Code == 551 || tp.Code == 553 || tp.Code == 501):
		return &domain.SendError{Code: domain.ErrInvalidAddress, Err: err}
	default:
		return &domain.SendError{Code: domain.ErrProviderRejected, Err: err}
	}
}

func temporary(err error) error {
	return &domain.SendError{Code: domain.ErrProviderUnavailable, Temporary: true, Err: err}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "notify-relay.invalid"
}
