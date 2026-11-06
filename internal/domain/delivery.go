package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits from api/openapi.yaml.
const (
	MaxRecipients     = 1000
	MaxBodyBytes      = 2 << 20 // 2MB (AC-R001-05)
	maxDedupeKeyLen   = 128
	minDedupeKeyLen   = 8
	maxTenantRefLen   = 64
	maxRecipientRef   = 64
	maxEmailLen       = 254
	maxSubjectLen     = 200
	maxTextLen        = 2000
	maxURLLen         = 2048
	demoDomainSuffix  = ".invalid"
	fieldDedupeKey    = "/dedupe_key"
	fieldTenantRef    = "/tenant_ref"
	fieldRecipients   = "/recipients"
	fieldSubject      = "/message/subject"
	fieldText         = "/message/text"
	fieldURL          = "/message/url"
	fieldExpiresAt    = "/expires_at"
	recipientsPattern = "/recipients/%d/%s"
)

var dedupeKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// SystemTenantRef is used for mail that belongs to no team, such as login links.
const SystemTenantRef = "system"

// Message is the content shared by every recipient. The business app finalizes the wording.
type Message struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	URL     string `json:"url,omitempty"`
}

// Recipient is one address. Ref is the business app's opaque ID and is the only value reported back.
type Recipient struct {
	Ref   string `json:"recipient_ref"`
	Email string `json:"email"`
}

// Request is a delivery request as received from the business app.
type Request struct {
	DedupeKey  string      `json:"dedupe_key"`
	TenantRef  string      `json:"tenant_ref"`
	Message    Message     `json:"message"`
	Recipients []Recipient `json:"recipients"`
	ExpiresAt  *time.Time  `json:"expires_at,omitempty"`
}

// FieldError describes one invalid field as a JSON Pointer (api/openapi.yaml Problem.errors).
// It never contains the submitted value.
type FieldError struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// ValidationError is returned when a request breaks the contract (422).
type ValidationError struct {
	Errors []FieldError
}

// Error implements error. Details are in Errors.
func (e *ValidationError) Error() string {
	return "delivery request is invalid"
}

// Validate checks the request against the contract. In demo mode only .invalid recipients
// are allowed, so a demo environment can never send to real people (AC-R001-06).
func (r Request) Validate(demoMode bool) error {
	var errs []FieldError
	add := func(pointer, message string) { errs = append(errs, FieldError{Pointer: pointer, Message: message}) }

	switch n := utf8.RuneCountInString(r.DedupeKey); {
	case n < minDedupeKeyLen || n > maxDedupeKeyLen:
		add(fieldDedupeKey, "length must be 8 to 128")
	case !dedupeKeyPattern.MatchString(r.DedupeKey):
		add(fieldDedupeKey, "may contain only letters, digits and . _ : -")
	}

	if r.TenantRef == "" || utf8.RuneCountInString(r.TenantRef) > maxTenantRefLen {
		add(fieldTenantRef, "is required and at most 64 characters")
	}

	if n := utf8.RuneCountInString(r.Message.Subject); n < 1 || n > maxSubjectLen {
		add(fieldSubject, "length must be 1 to 200")
	} else if strings.ContainsAny(r.Message.Subject, "\r\n") {
		add(fieldSubject, "must be a single line")
	}
	if n := utf8.RuneCountInString(r.Message.Text); n < 1 || n > maxTextLen {
		add(fieldText, "length must be 1 to 2000")
	}
	if len(r.Message.URL) > maxURLLen {
		add(fieldURL, "must be at most 2048 characters")
	} else if r.Message.URL != "" {
		u, err := url.Parse(r.Message.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add(fieldURL, "must be an absolute http(s) URL")
		}
	}

	switch n := len(r.Recipients); {
	case n == 0:
		add(fieldRecipients, "at least one recipient is required")
	case n > MaxRecipients:
		add(fieldRecipients, "at most 1000 recipients per request")
	default:
		seen := make(map[string]struct{}, n)
		for i, rc := range r.Recipients {
			if rc.Ref == "" || utf8.RuneCountInString(rc.Ref) > maxRecipientRef {
				add(fmt.Sprintf(recipientsPattern, i, "recipient_ref"), "is required and at most 64 characters")
			} else if _, dup := seen[rc.Ref]; dup {
				add(fmt.Sprintf(recipientsPattern, i, "recipient_ref"), "must be unique within the request")
			} else {
				seen[rc.Ref] = struct{}{}
			}
			if !validEmail(rc.Email) {
				add(fmt.Sprintf(recipientsPattern, i, "email"), "is not a valid address")
			} else if demoMode && !IsDemoAddress(rc.Email) {
				add(fmt.Sprintf(recipientsPattern, i, "email"), "demo mode accepts only .invalid addresses")
			}
		}
	}

	if r.ExpiresAt != nil && r.ExpiresAt.IsZero() {
		add(fieldExpiresAt, "must be a date-time")
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

func validEmail(s string) bool {
	if s == "" || len(s) > maxEmailLen || strings.ContainsAny(s, "\r\n<> ") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == ""
}

// ContentHash identifies the request's content. Two requests with the same dedupe key are
// the same request only if their hashes match (AC-R001-03, AC-R001-04).
func (r Request) ContentHash() string {
	canon := r
	if canon.ExpiresAt != nil {
		t := canon.ExpiresAt.UTC()
		canon.ExpiresAt = &t
	}
	b, _ := json.Marshal(canon) // fields are plain strings and times; Marshal cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IsDemoAddress reports whether the address is in the reserved .invalid domain, the only
// domain a demo environment may send to (AC-R001-06).
func IsDemoAddress(email string) bool {
	return strings.HasSuffix(strings.ToLower(email), demoDomainSuffix)
}

// MinEmailRefKeyBytes is the shortest key EmailRefKey accepts.
const MinEmailRefKeyBytes = 32

// EmailRefKey is the secret for the value logs and delivery records keep instead of an
// address (CLAUDE.md, ADR-0003 "配信記録にはハッシュ値だけを残す"). The value is keyed so that
// someone who can read the logs cannot test a guessed address against them.
type EmailRefKey []byte

// Ref returns the first 12 hex characters of HMAC-SHA256(key, lower-cased address).
// It panics on a missing or short key: config.Load refuses those at startup, so reaching
// this with one is a wiring mistake that must not silently produce unkeyed values.
func (k EmailRefKey) Ref(email string) string {
	if len(k) < MinEmailRefKeyBytes {
		panic("domain: EmailRefKey is missing or shorter than 32 bytes")
	}
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(strings.ToLower(email)))
	return hex.EncodeToString(mac.Sum(nil)[:6])
}
