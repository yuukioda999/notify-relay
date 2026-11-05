-- Delivery requests and their recipients (AC-R001-01). Kept for 7 days (ADR-0003).
CREATE TABLE deliveries (
    id              CHAR(30)      NOT NULL PRIMARY KEY,
    dedupe_key      VARCHAR(128)  NOT NULL,
    content_hash    CHAR(64)      NOT NULL,
    tenant_ref      VARCHAR(64)   NOT NULL,
    subject         VARCHAR(200)  NOT NULL,
    body_text       TEXT          NOT NULL,
    url             VARCHAR(2048) NULL,
    expires_at      DATETIME(6)   NULL,
    traceparent     VARCHAR(55)   NULL,
    recipient_count INT           NOT NULL,
    accepted_at     DATETIME(6)   NOT NULL,
    completed_at    DATETIME(6)   NULL,
    UNIQUE KEY deliveries_dedupe_key_unique (dedupe_key),
    KEY deliveries_accepted_at_index (accepted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- One row per address. email is set to NULL as soon as the row reaches a final status (AC-R001-15).
CREATE TABLE delivery_recipients (
    id              BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    delivery_id     CHAR(30)     NOT NULL,
    recipient_ref   VARCHAR(64)  NOT NULL,
    email           VARCHAR(254) NULL,
    status          VARCHAR(16)  NOT NULL,
    error_code      VARCHAR(32)  NULL,
    attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(6)  NOT NULL,
    updated_at      DATETIME(6)  NOT NULL,
    UNIQUE KEY delivery_recipients_ref_unique (delivery_id, recipient_ref),
    KEY delivery_recipients_due_index (status, next_attempt_at),
    CONSTRAINT delivery_recipients_delivery_fk FOREIGN KEY (delivery_id) REFERENCES deliveries (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
