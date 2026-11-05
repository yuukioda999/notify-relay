package smtpmail

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// fakeSMTP is a tiny SMTP server. rcptReply is the reply to RCPT TO (for example "250 OK").
type fakeSMTP struct {
	ln        net.Listener
	rcptReply string
	mu        sync.Mutex
	data      []string
}

func startFakeSMTP(t *testing.T, rcptReply string) *fakeSMTP {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, rcptReply: rcptReply}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	reply := func(line string) { _, _ = fmt.Fprintf(conn, "%s\r\n", line) }
	reply("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			reply("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO"):
			reply(s.rcptReply)
		case cmd == "DATA":
			reply("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = append(s.data, b.String())
			s.mu.Unlock()
			reply("250 queued")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (s *fakeSMTP) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data...)
}

func mailerFor(addr string) *Mailer {
	return &Mailer{Addr: addr, From: "no-reply@team-ops.invalid", FromName: "チーム運営", TLS: TLSNone, Timeout: 2 * time.Second, Footer: "このメールは送信専用です。"}
}

var testMail = usecase.Mail{
	To:      "coach@example.invalid",
	Message: domain.Message{Subject: "【中止】11/8(土)の練習", Text: "雨のため中止です。", URL: "http://localhost:5173/notices/1"},
}

func TestSendClassifiesReplies(t *testing.T) {
	tests := []struct {
		name          string
		rcptReply     string
		wantErr       bool
		wantCode      domain.ErrorCode
		wantTemporary bool
	}{
		{name: "AC-R001-08 accepted by the server", rcptReply: "250 OK"},
		{name: "AC-R001-10 451 is a temporary failure", rcptReply: "451 4.3.0 try again later", wantErr: true, wantCode: domain.ErrProviderUnavailable, wantTemporary: true},
		{name: "AC-R001-10 452 is treated as rate limiting", rcptReply: "452 4.5.3 too many recipients", wantErr: true, wantCode: domain.ErrRateLimited, wantTemporary: true},
		{name: "AC-R001-11 550 means the address is invalid", rcptReply: "550 5.1.1 user unknown", wantErr: true, wantCode: domain.ErrInvalidAddress},
		{name: "AC-R001-11 554 is a permanent rejection", rcptReply: "554 5.7.1 rejected", wantErr: true, wantCode: domain.ErrProviderRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startFakeSMTP(t, tt.rcptReply)
			err := mailerFor(srv.ln.Addr().String()).Send(context.Background(), testMail)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Send() = %v", err)
				}
				return
			}
			var se *domain.SendError
			if !errors.As(err, &se) {
				t.Fatalf("Send() = %v, want *domain.SendError", err)
			}
			if se.Code != tt.wantCode || se.Temporary != tt.wantTemporary {
				t.Fatalf("got code=%s temporary=%v, want %s %v", se.Code, se.Temporary, tt.wantCode, tt.wantTemporary)
			}
		})
	}
}

func TestSendToUnreachableServerIsTemporary(t *testing.T) {
	ln, _ := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens here any more

	err := mailerFor(addr).Send(context.Background(), testMail)
	var se *domain.SendError
	if !errors.As(err, &se) || !se.Temporary || se.Code != domain.ErrProviderUnavailable {
		t.Fatalf("Send() = %v, want a temporary provider_unavailable error (AC-R001-10)", err)
	}
}

func TestComposedMessage(t *testing.T) {
	srv := startFakeSMTP(t, "250 OK")
	if err := mailerFor(srv.ln.Addr().String()).Send(context.Background(), testMail); err != nil {
		t.Fatal(err)
	}
	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("server received %d messages", len(msgs))
	}
	parsed, err := mail.ReadMessage(strings.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}

	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || subject != testMail.Message.Subject {
		t.Fatalf("subject = %q, %v", subject, err)
	}
	if got := parsed.Header.Get("Content-Type"); got != "text/plain; charset=UTF-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	encoded, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"雨のため中止です。", "http://localhost:5173/notices/1", "このメールは送信専用です。"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body %q lacks %q", body, want)
		}
	}
}

func TestSubjectWithLineBreakIsRefused(t *testing.T) {
	srv := startFakeSMTP(t, "250 OK")
	m := testMail
	m.Message.Subject = "hi\r\nBcc: someone@example.invalid"
	err := mailerFor(srv.ln.Addr().String()).Send(context.Background(), m)
	var se *domain.SendError
	if !errors.As(err, &se) || se.Temporary {
		t.Fatalf("Send() = %v, want a permanent error", err)
	}
	if len(srv.messages()) != 0 {
		t.Fatal("a message with an injected header was sent")
	}
}
