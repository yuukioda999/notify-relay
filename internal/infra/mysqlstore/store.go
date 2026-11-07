package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

const (
	mysqlDuplicateEntry  = 1062
	mysqlDeadlock        = 1213
	mysqlLockWaitTimeout = 1205
	insertChunk          = 500
)

// Store implements usecase.DeliveryStore and usecase.SendQueue.
type Store struct {
	DB *sql.DB
	// RefKey makes the email_ref kept after the address is deleted (ADR-0003).
	RefKey domain.EmailRefKey
}

var (
	_ usecase.DeliveryStore = (*Store)(nil)
	_ usecase.SendQueue     = (*Store)(nil)
)

// Open connects to MySQL and checks the connection.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DB_DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(20)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Create saves the delivery and all recipients in one transaction (AC-R001-01).
func (s *Store) Create(ctx context.Context, d usecase.NewDelivery) (err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	r := d.Request
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries
		(id, dedupe_key, content_hash, tenant_ref, subject, body_text, url, expires_at, traceparent, recipient_count, accepted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(d.ID), d.DedupeKey, d.ContentHash, r.TenantRef, r.Message.Subject, r.Message.Text,
		nullString(r.Message.URL), nullTime(r.ExpiresAt), nullString(d.Traceparent), d.RecipientCount, d.AcceptedAt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
			return usecase.ErrDuplicateDedupeKey
		}
		return err
	}

	for start := 0; start < len(r.Recipients); start += insertChunk {
		chunk := r.Recipients[start:min(start+insertChunk, len(r.Recipients))]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*6)
		for i, rc := range chunk {
			placeholders[i] = "(?, ?, ?, ?, ?, ?, ?)"
			args = append(args, string(d.ID), rc.Ref, rc.Email, s.RefKey.Ref(rc.Email), string(domain.StatusPending), d.AcceptedAt, d.AcceptedAt)
		}
		//nolint:gosec // G202: only "(?, ?, ...)" placeholder groups are concatenated; values are bound
		_, err = tx.ExecContext(ctx, `INSERT INTO delivery_recipients
			(delivery_id, recipient_ref, email, email_ref, status, next_attempt_at, updated_at) VALUES `+strings.Join(placeholders, ","), args...)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FindByDedupeKey returns the delivery stored under key.
func (s *Store) FindByDedupeKey(ctx context.Context, key string) (usecase.Accepted, error) {
	var a usecase.Accepted
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id, dedupe_key, content_hash, recipient_count, accepted_at
		FROM deliveries WHERE dedupe_key = ?`, key).Scan(&id, &a.DedupeKey, &a.ContentHash, &a.RecipientCount, &a.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return usecase.Accepted{}, usecase.ErrNotFound
	}
	a.ID = domain.DeliveryID(id)
	return a, err
}

// Claim locks due recipients, skipping rows another sender holds, and marks them "sending".
//
// Concurrent claims can still deadlock in MySQL (SKIP LOCKED skips locked rows, but the index
// locks taken by the UPDATE can cross). The claim is then rolled back before anything is sent,
// so it is simply run again (AC-R001-08).
func (s *Store) Claim(ctx context.Context, now time.Time, limit int) (jobs []usecase.Job, err error) {
	err = retryOnLockConflict(func() error {
		var claimErr error
		jobs, claimErr = s.claim(ctx, now, limit)
		return claimErr
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) claim(ctx context.Context, now time.Time, limit int) (jobs []usecase.Job, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM delivery_recipients
		WHERE status = ? AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, string(domain.StatusPending), now, limit)
	if err != nil {
		return nil, err
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}

	// Only "?" placeholders are concatenated into the queries below; the IDs are bound (gosec G202).
	in := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := append([]any{string(domain.StatusSending), now}, ids...)
	//nolint:gosec // G202: see above
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_recipients SET status = ?, updated_at = ? WHERE id IN (`+in+`)`, args...); err != nil {
		return nil, err
	}

	//nolint:gosec // G202: see above
	rows, err = tx.QueryContext(ctx, `SELECT r.id, r.delivery_id, r.email, r.attempts, d.subject, d.body_text, d.url, d.expires_at
		FROM delivery_recipients r JOIN deliveries d ON d.id = r.delivery_id
		WHERE r.id IN (`+in+`) ORDER BY r.id`, ids...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var j usecase.Job
		var deliveryID string
		var url, text sql.NullString
		var expires sql.NullTime
		if err = rows.Scan(&j.RecipientID, &deliveryID, &j.Email, &j.Attempts, &j.Message.Subject, &text, &url, &expires); err != nil {
			return nil, err
		}
		j.DeliveryID = domain.DeliveryID(deliveryID)
		j.Message.URL = url.String
		j.Message.Text = text.String
		if expires.Valid {
			t := expires.Time
			j.ExpiresAt = &t
		}
		jobs = append(jobs, j)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return jobs, tx.Commit()
}

// Record stores the outcome of an attempt. A final status deletes the address (AC-R001-15),
// and the delivery is marked complete once no recipient is left to send.
//
// The delivery row is locked first, so recipients of the same delivery are recorded one at a
// time. Without it, two transactions recording the last two recipients wait for each other
// (deadlock) and both roll back after the mail was sent. A deadlock that still happens is retried.
func (s *Store) Record(ctx context.Context, recipientID int64, o usecase.Outcome, now time.Time) error {
	return retryOnLockConflict(func() error { return s.record(ctx, recipientID, o, now) })
}

// lockConflictAttempts is how many times a transaction that lost a lock conflict is run.
const lockConflictAttempts = 3

// retryOnLockConflict runs fn again when MySQL rolled it back for a deadlock or a lock wait
// timeout. Both roll back the whole transaction, so running it again is safe.
func retryOnLockConflict(fn func() error) error {
	var err error
	for attempt := range lockConflictAttempts {
		if err = fn(); !isLockConflict(err) {
			return err
		}
		if attempt < lockConflictAttempts-1 {
			time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
		}
	}
	return err
}

func isLockConflict(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == mysqlDeadlock || me.Number == mysqlLockWaitTimeout)
}

func (s *Store) record(ctx context.Context, recipientID int64, o usecase.Outcome, now time.Time) (err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var deliveryID string
	if err = tx.QueryRowContext(ctx, `SELECT delivery_id FROM delivery_recipients WHERE id = ?`, recipientID).Scan(&deliveryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recipient %d: %w", recipientID, usecase.ErrRecipientGone)
		}
		return fmt.Errorf("recipient %d: %w", recipientID, err)
	}
	var locked string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE id = ? FOR UPDATE`, deliveryID).Scan(&locked); err != nil {
		return err
	}

	final := o.Status.IsFinal()
	next := now
	if !final {
		next = o.NextAttempt
	}
	res, err := tx.ExecContext(ctx, `UPDATE delivery_recipients
		SET status = ?, error_code = ?, attempts = ?, next_attempt_at = ?, updated_at = ?,
		    email = CASE WHEN ? THEN NULL ELSE email END
		WHERE id = ? AND status = ?`,
		string(o.Status), nullString(string(o.Code)), o.Attempts, next, now, final, recipientID, string(domain.StatusSending))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("recipient %d is not being sent", recipientID)
	}

	if final {
		// The body and URL can hold a login link; they are not needed once nothing is left to send.
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET completed_at = ?, body_text = NULL, url = NULL
			WHERE id = ? AND completed_at IS NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM delivery_recipients
			    WHERE delivery_id = ? AND status IN (?, ?)
			  )`, now, deliveryID, deliveryID, string(domain.StatusPending), string(domain.StatusSending))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Release returns claimed recipients that were not sent (for example at shutdown) to "pending".
func (s *Store) Release(ctx context.Context, recipientIDs []int64) error {
	if len(recipientIDs) == 0 {
		return nil
	}
	args := []any{string(domain.StatusPending), string(domain.StatusSending)}
	for _, id := range recipientIDs {
		args = append(args, id)
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(recipientIDs)), ",")
	//nolint:gosec // G202: only "?" placeholders are concatenated; the IDs are bound
	_, err := s.DB.ExecContext(ctx, `UPDATE delivery_recipients SET status = ? WHERE status = ? AND id IN (`+in+`)`, args...)
	return err
}

// RequeueInterrupted returns recipients left "sending" to "pending". It must run before the
// sender starts and assumes one notify-relay process (AC-R001-13, ADR-0002).
func (s *Store) RequeueInterrupted(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE delivery_recipients SET status = ? WHERE status = ?`,
		string(domain.StatusPending), string(domain.StatusSending))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

// DeleteAcceptedBefore deletes up to limit deliveries accepted before the given time. Their
// recipients go with them (ON DELETE CASCADE), and so does the dedupe key, which lives on
// the delivery row (AC-R001-22).
func (s *Store) DeleteAcceptedBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM deliveries WHERE accepted_at < ? ORDER BY accepted_at LIMIT ?", before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete old deliveries: %w", err)
	}
	return res.RowsAffected()
}
