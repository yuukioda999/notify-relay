-- Webhook events (AC-R001-16 to 20, team-ops ADR-0019). An event is saved before it is sent,
-- so a crash does not lose it, and a recipient is put into one results event only.
ALTER TABLE delivery_recipients
    ADD COLUMN reported_at DATETIME(6) NULL,
    ADD KEY delivery_recipients_unreported_index (reported_at, status);

ALTER TABLE deliveries
    ADD COLUMN completed_reported_at DATETIME(6) NULL,
    ADD KEY deliveries_completed_unreported_index (completed_reported_at, completed_at);

-- Results reached before webhooks existed were never expected by the business app; do not send them now.
UPDATE delivery_recipients SET reported_at = updated_at WHERE status IN ('delivered', 'failed', 'skipped');

UPDATE deliveries SET completed_reported_at = completed_at WHERE completed_at IS NOT NULL;

CREATE TABLE webhook_events (
    id              CHAR(30)     NOT NULL PRIMARY KEY,
    delivery_id     CHAR(30)     NOT NULL,
    type            VARCHAR(32)  NOT NULL,
    payload         MEDIUMTEXT   NOT NULL, -- the body without sent_at; recipient_ref and statuses only, no addresses
    traceparent     VARCHAR(55)  NULL,
    attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(6)  NOT NULL,
    created_at      DATETIME(6)  NOT NULL,
    sent_at         DATETIME(6)  NULL,
    gave_up_at      DATETIME(6)  NULL,
    KEY webhook_events_due_index (sent_at, gave_up_at, next_attempt_at),
    CONSTRAINT webhook_events_delivery_fk FOREIGN KEY (delivery_id) REFERENCES deliveries (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
