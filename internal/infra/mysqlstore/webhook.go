package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

var (
	_ usecase.WebhookStore = (*Store)(nil)
	_ usecase.StatusReader = (*Store)(nil)
)

// candidateLimit bounds how many deliveries one round looks at.
const candidateLimit = 100

const finalStatuses = "'delivered','failed','skipped'"

type resultItem struct {
	RecipientRef string  `json:"recipient_ref"`
	Status       string  `json:"status"`
	ErrorCode    *string `json:"error_code"`
	Attempts     int     `json:"attempts"`
	OccurredAt   string  `json:"occurred_at"`
}

type resultsPayload struct {
	EventID    string       `json:"event_id"`
	Type       string       `json:"type"`
	DeliveryID string       `json:"delivery_id"`
	DedupeKey  string       `json:"dedupe_key"`
	Results    []resultItem `json:"results"`
}

type completedPayload struct {
	EventID     string               `json:"event_id"`
	Type        string               `json:"type"`
	DeliveryID  string               `json:"delivery_id"`
	DedupeKey   string               `json:"dedupe_key"`
	Counts      usecase.ResultCounts `json:"counts"`
	CompletedAt string               `json:"completed_at"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// CreateResultEvents implements usecase.WebhookStore (AC-R001-16).
func (s *Store) CreateResultEvents(ctx context.Context, now time.Time, maxBatch int, wait time.Duration) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.delivery_id
		FROM delivery_recipients r JOIN deliveries d ON d.id = r.delivery_id
		WHERE r.reported_at IS NULL AND r.status IN (`+finalStatuses+`)
		GROUP BY r.delivery_id, d.completed_at
		HAVING COUNT(*) >= ? OR MIN(r.updated_at) <= ? OR d.completed_at IS NOT NULL
		LIMIT ?`, maxBatch, now.Add(-wait), candidateLimit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	made := 0
	for _, id := range ids {
		for {
			var n int
			var more bool
			err := retryOnLockConflict(func() error {
				var e error
				n, more, e = s.createResultEvent(ctx, id, now, maxBatch, wait)
				return e
			})
			if err != nil {
				return made, err
			}
			made += n
			if !more {
				break
			}
		}
	}
	return made, nil
}

// createResultEvent makes at most one event for the delivery. more is true when a full
// batch was taken and another may follow.
func (s *Store) createResultEvent(ctx context.Context, deliveryID string, now time.Time, maxBatch int, wait time.Duration) (made int, more bool, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Lock the delivery first, in the same order as Record, so the two do not deadlock.
	var dedupeKey string
	var traceparent sql.NullString
	var completed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT dedupe_key, traceparent, completed_at FROM deliveries WHERE id = ? FOR UPDATE`, deliveryID).
		Scan(&dedupeKey, &traceparent, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, tx.Commit() // purged meanwhile
	}
	if err != nil {
		return 0, false, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, recipient_ref, status, error_code, attempts, updated_at
		FROM delivery_recipients
		WHERE delivery_id = ? AND reported_at IS NULL AND status IN (`+finalStatuses+`)
		ORDER BY id LIMIT ?`, deliveryID, maxBatch)
	if err != nil {
		return 0, false, err
	}
	var ids []any
	var items []resultItem
	var oldest time.Time
	for rows.Next() {
		var id int64
		var it resultItem
		var code sql.NullString
		var at time.Time
		if err = rows.Scan(&id, &it.RecipientRef, &it.Status, &code, &it.Attempts, &at); err != nil {
			_ = rows.Close()
			return 0, false, err
		}
		if code.Valid {
			c := code.String
			it.ErrorCode = &c
		}
		it.OccurredAt = rfc3339(at)
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		ids = append(ids, id)
		items = append(items, it)
	}
	if err = rows.Close(); err != nil {
		return 0, false, err
	}
	full := len(items) == maxBatch
	if len(items) == 0 || (!full && !completed.Valid && oldest.After(now.Add(-wait))) {
		return 0, false, tx.Commit()
	}

	eventID, err := domain.NewEventID(now, nil)
	if err != nil {
		return 0, false, err
	}
	payload, err := json.Marshal(resultsPayload{EventID: eventID, Type: "delivery.results", DeliveryID: deliveryID, DedupeKey: dedupeKey, Results: items})
	if err != nil {
		return 0, false, err
	}
	if err = s.insertEvent(ctx, tx, eventID, deliveryID, "delivery.results", payload, traceparent, now); err != nil {
		return 0, false, err
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	//nolint:gosec // G202: only "?" placeholders are concatenated; the IDs are bound
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_recipients SET reported_at = ? WHERE id IN (`+in+`)`, append([]any{now}, ids...)...); err != nil {
		return 0, false, err
	}
	return 1, full, tx.Commit()
}

// CreateCompletedEvents implements usecase.WebhookStore (AC-R001-17).
func (s *Store) CreateCompletedEvents(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id FROM deliveries d
		WHERE d.completed_reported_at IS NULL AND d.completed_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM delivery_recipients r WHERE r.delivery_id = d.id AND r.reported_at IS NULL)
		LIMIT ?`, candidateLimit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	made := 0
	for _, id := range ids {
		var n int
		err := retryOnLockConflict(func() error {
			var e error
			n, e = s.createCompletedEvent(ctx, id, now)
			return e
		})
		if err != nil {
			return made, err
		}
		made += n
	}
	return made, nil
}

func (s *Store) createCompletedEvent(ctx context.Context, deliveryID string, now time.Time) (made int, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var dedupeKey string
	var traceparent sql.NullString
	var completed, reported sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT dedupe_key, traceparent, completed_at, completed_reported_at FROM deliveries WHERE id = ? FOR UPDATE`, deliveryID).
		Scan(&dedupeKey, &traceparent, &completed, &reported)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	if !completed.Valid || reported.Valid {
		return 0, tx.Commit()
	}
	counts, unreported, err := countRecipients(ctx, tx, deliveryID)
	if err != nil {
		return 0, err
	}
	if unreported > 0 {
		return 0, tx.Commit() // results go first
	}
	eventID, err := domain.NewEventID(now, nil)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(completedPayload{EventID: eventID, Type: "delivery.completed", DeliveryID: deliveryID, DedupeKey: dedupeKey, Counts: counts, CompletedAt: rfc3339(completed.Time)})
	if err != nil {
		return 0, err
	}
	if err = s.insertEvent(ctx, tx, eventID, deliveryID, "delivery.completed", payload, traceparent, now); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET completed_reported_at = ? WHERE id = ?`, now, deliveryID); err != nil {
		return 0, err
	}
	return 1, tx.Commit()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// countRecipients counts by status (sending as pending) and how many final results are not reported.
func countRecipients(ctx context.Context, q queryer, deliveryID string) (counts usecase.ResultCounts, unreported int, err error) {
	rows, err := q.QueryContext(ctx, `SELECT status, COUNT(*), SUM(CASE WHEN reported_at IS NULL AND status IN (`+finalStatuses+`) THEN 1 ELSE 0 END)
		FROM delivery_recipients WHERE delivery_id = ? GROUP BY status`, deliveryID)
	if err != nil {
		return counts, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var n, u int
		if err = rows.Scan(&status, &n, &u); err != nil {
			return counts, 0, err
		}
		unreported += u
		switch domain.RecipientStatus(status) {
		case domain.StatusDelivered:
			counts.Delivered += n
		case domain.StatusFailed:
			counts.Failed += n
		case domain.StatusSkipped:
			counts.Skipped += n
		default:
			counts.Pending += n
		}
	}
	return counts, unreported, rows.Err()
}

func (s *Store) insertEvent(ctx context.Context, tx *sql.Tx, id, deliveryID, typ string, payload []byte, traceparent sql.NullString, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO webhook_events (id, delivery_id, type, payload, traceparent, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)`, id, deliveryID, typ, string(payload), traceparent, now, now)
	return err
}

// ClaimDueEvents implements usecase.WebhookStore.
func (s *Store) ClaimDueEvents(ctx context.Context, now time.Time, limit int, lease time.Duration) (events []usecase.WebhookEvent, err error) {
	err = retryOnLockConflict(func() error {
		var e error
		events, e = s.claimDueEvents(ctx, now, limit, lease)
		return e
	})
	return events, err
}

func (s *Store) claimDueEvents(ctx context.Context, now time.Time, limit int, lease time.Duration) (events []usecase.WebhookEvent, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `SELECT id, payload, traceparent, attempts, created_at FROM webhook_events
		WHERE sent_at IS NULL AND gave_up_at IS NULL AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	var ids []any
	for rows.Next() {
		var e usecase.WebhookEvent
		var payload string
		var tp sql.NullString
		if err = rows.Scan(&e.ID, &payload, &tp, &e.Attempts, &e.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Payload = []byte(payload)
		e.Traceparent = tp.String
		events = append(events, e)
		ids = append(ids, e.ID)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	//nolint:gosec // G202: only "?" placeholders are concatenated; the IDs are bound
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_events SET next_attempt_at = ? WHERE id IN (`+in+`)`, append([]any{now.Add(lease)}, ids...)...); err != nil {
		return nil, err
	}
	return events, tx.Commit()
}

// MarkEventSent implements usecase.WebhookStore.
func (s *Store) MarkEventSent(ctx context.Context, id string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webhook_events SET sent_at = ?, attempts = attempts + 1 WHERE id = ?`, now, id)
	return err
}

// RescheduleEvent implements usecase.WebhookStore.
func (s *Store) RescheduleEvent(ctx context.Context, id string, attempts int, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webhook_events SET attempts = ?, next_attempt_at = ? WHERE id = ? AND sent_at IS NULL`, attempts, next, id)
	return err
}

// GiveUpEvent implements usecase.WebhookStore.
func (s *Store) GiveUpEvent(ctx context.Context, id string, attempts int, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webhook_events SET attempts = ?, gave_up_at = ? WHERE id = ? AND sent_at IS NULL`, attempts, now, id)
	return err
}

// DeliveryStatus implements usecase.StatusReader (AC-R001-21).
func (s *Store) DeliveryStatus(ctx context.Context, id domain.DeliveryID) (usecase.DeliveryStatus, error) {
	st := usecase.DeliveryStatus{ID: id}
	var completed sql.NullTime
	err := s.DB.QueryRowContext(ctx, `SELECT dedupe_key, accepted_at, completed_at FROM deliveries WHERE id = ?`, string(id)).
		Scan(&st.DedupeKey, &st.AcceptedAt, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return st, usecase.ErrNotFound
	}
	if err != nil {
		return st, err
	}
	st.Counts, _, err = countRecipients(ctx, s.DB, string(id))
	if err != nil {
		return st, err
	}
	var tried bool
	if err = s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM delivery_recipients WHERE delivery_id = ? AND (attempts > 0 OR status <> ?))`,
		string(id), string(domain.StatusPending)).Scan(&tried); err != nil {
		return st, err
	}
	switch {
	case completed.Valid:
		t := completed.Time
		st.CompletedAt = &t
		st.State = usecase.StateCompleted
	case tried:
		st.State = usecase.StateProcessing
	default:
		st.State = usecase.StateAccepted
	}
	return st, nil
}
