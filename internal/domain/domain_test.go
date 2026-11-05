package domain

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{
		DedupeKey: "login-link:12345",
		TenantRef: SystemTenantRef,
		Message:   Message{Subject: "ログイン用のリンク", Text: "リンクを開いてください", URL: "http://localhost:5173/login/verify"},
		Recipients: []Recipient{
			{Ref: "user_1", Email: "coach@example.invalid"},
		},
	}
}

func TestValidate(t *testing.T) {
	tooMany := make([]Recipient, MaxRecipients+1)
	for i := range tooMany {
		tooMany[i] = Recipient{Ref: "r" + strings.Repeat("x", i%10) + string(rune('a'+i%26)), Email: "a@example.invalid"}
	}

	tests := []struct {
		name      string
		mutate    func(*Request)
		demo      bool
		wantField string // empty means valid
	}{
		{name: "valid request", mutate: func(*Request) {}},
		{name: "dedupe key too short", mutate: func(r *Request) { r.DedupeKey = "short" }, wantField: "/dedupe_key"},
		{name: "dedupe key with spaces", mutate: func(r *Request) { r.DedupeKey = "has spaces in it" }, wantField: "/dedupe_key"},
		{name: "missing tenant", mutate: func(r *Request) { r.TenantRef = "" }, wantField: "/tenant_ref"},
		{name: "empty subject", mutate: func(r *Request) { r.Message.Subject = "" }, wantField: "/message/subject"},
		{name: "subject with a line break (header injection)", mutate: func(r *Request) { r.Message.Subject = "a\r\nBcc: x@example.invalid" }, wantField: "/message/subject"},
		{name: "text too long", mutate: func(r *Request) { r.Message.Text = strings.Repeat("あ", 2001) }, wantField: "/message/text"},
		{name: "relative url", mutate: func(r *Request) { r.Message.URL = "/login" }, wantField: "/message/url"},
		{name: "javascript url", mutate: func(r *Request) { r.Message.URL = "javascript:alert(1)" }, wantField: "/message/url"},
		{name: "no recipients", mutate: func(r *Request) { r.Recipients = nil }, wantField: "/recipients"},
		{name: "AC-R001-05 more than 1000 recipients", mutate: func(r *Request) { r.Recipients = tooMany }, wantField: "/recipients"},
		{name: "invalid address", mutate: func(r *Request) { r.Recipients[0].Email = "not-an-address" }, wantField: "/recipients/0/email"},
		{name: "address with a display name", mutate: func(r *Request) { r.Recipients[0].Email = "Coach <coach@example.invalid>" }, wantField: "/recipients/0/email"},
		{name: "duplicate recipient ref", mutate: func(r *Request) {
			r.Recipients = append(r.Recipients, Recipient{Ref: "user_1", Email: "b@example.invalid"})
		}, wantField: "/recipients/1/recipient_ref"},
		{name: "AC-R001-06 demo mode rejects a real domain", demo: true, mutate: func(r *Request) { r.Recipients[0].Email = "parent@example.com" }, wantField: "/recipients/0/email"},
		{name: "AC-R001-06 demo mode accepts .invalid", demo: true, mutate: func(*Request) {}},
		{name: "AC-R001-06 demo mode check ignores case", demo: true, mutate: func(r *Request) { r.Recipients[0].Email = "a@EXAMPLE.INVALID" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validRequest()
			tt.mutate(&r)
			err := r.Validate(tt.demo)

			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() = %v, want *ValidationError", err)
			}
			for _, fe := range ve.Errors {
				if fe.Pointer == tt.wantField {
					return
				}
			}
			t.Fatalf("errors %+v do not include field %q", ve.Errors, tt.wantField)
		})
	}
}

func TestValidationErrorsDoNotContainTheSubmittedAddress(t *testing.T) {
	r := validRequest()
	r.Recipients[0].Email = "parent@example.com"
	err := r.Validate(true)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want validation error, got %v", err)
	}
	for _, fe := range ve.Errors {
		if strings.Contains(fe.Message, "parent@example.com") || strings.Contains(fe.Pointer, "parent@") {
			t.Fatalf("field error leaks the address: %+v", fe)
		}
	}
}

func TestContentHash(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	at := time.Date(2026, 11, 8, 8, 0, 0, 0, jst)
	atUTC := at.UTC()

	tests := []struct {
		name   string
		mutate func(*Request)
		same   bool
	}{
		{name: "AC-R001-03 identical content", mutate: func(*Request) {}, same: true},
		{name: "AC-R001-03 same instant in another time zone", mutate: func(r *Request) { r.ExpiresAt = &atUTC }, same: true},
		{name: "AC-R001-04 different text", mutate: func(r *Request) { r.Message.Text = "別の本文" }, same: false},
		{name: "AC-R001-04 different recipients", mutate: func(r *Request) { r.Recipients[0].Email = "other@example.invalid" }, same: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validRequest()
			a.ExpiresAt = &at
			b := validRequest()
			b.ExpiresAt = &at
			tt.mutate(&b)
			if got := a.ContentHash() == b.ContentHash(); got != tt.same {
				t.Fatalf("same hash = %v, want %v", got, tt.same)
			}
		})
	}
}

func TestNewDeliveryID(t *testing.T) {
	pattern := regexp.MustCompile(`^dlv_[0-9A-HJKMNP-TV-Z]{26}$`)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		entropy func() *bytes.Reader
	}{
		{name: "zero entropy", entropy: func() *bytes.Reader { return bytes.NewReader(make([]byte, 10)) }},
		{name: "full entropy", entropy: func() *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := NewDeliveryID(now, tt.entropy())
			if err != nil {
				t.Fatal(err)
			}
			if !pattern.MatchString(string(id)) {
				t.Fatalf("id %q does not match the OpenAPI pattern", id)
			}
		})
	}

	t.Run("later IDs sort after earlier ones", func(t *testing.T) {
		a, _ := NewDeliveryID(now, nil)
		b, _ := NewDeliveryID(now.Add(time.Millisecond), nil)
		if a >= b {
			t.Fatalf("%s should sort before %s", a, b)
		}
	})

	t.Run("not enough entropy is an error", func(t *testing.T) {
		if _, err := NewDeliveryID(now, bytes.NewReader([]byte{1, 2})); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name   string
		failed int
		want   time.Duration
		ok     bool
	}{
		{name: "AC-R001-10 first retry after 30s", failed: 1, want: 30 * time.Second, ok: true},
		{name: "AC-R001-10 second retry after 2m", failed: 2, want: 2 * time.Minute, ok: true},
		{name: "AC-R001-10 fifth retry after 1h", failed: 5, want: time.Hour, ok: true},
		{name: "AC-R001-10 no sixth retry", failed: 6, ok: false},
		{name: "zero failures is not a retry", failed: 0, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RetryDelay(tt.failed)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("RetryDelay(%d) = %v, %v; want %v, %v", tt.failed, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestStatusIsFinal(t *testing.T) {
	tests := []struct {
		status RecipientStatus
		final  bool
	}{
		{StatusPending, false}, {StatusSending, false},
		{StatusDelivered, true}, {StatusFailed, true}, {StatusSkipped, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := tt.status.IsFinal(); got != tt.final {
				t.Fatalf("IsFinal() = %v, want %v", got, tt.final)
			}
		})
	}
}

func TestEmailRefDoesNotRevealTheAddress(t *testing.T) {
	ref := EmailRef("coach@example.invalid")
	if len(ref) != 12 || strings.Contains(ref, "coach") {
		t.Fatalf("EmailRef = %q", ref)
	}
	if EmailRef("Coach@Example.invalid") != ref {
		t.Fatal("EmailRef should ignore case")
	}
}
