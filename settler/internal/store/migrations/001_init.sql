-- Block headers the settler has accepted as canonical. Every unverified block is kept so a reorg
-- can be traced back to the common ancestor; below the verified height only sparse checkpoints remain.
CREATE TABLE blocks (
    chain_id    BIGINT  NOT NULL,
    number      BIGINT  NOT NULL,
    hash        TEXT    NOT NULL,
    parent_hash TEXT    NOT NULL,
    time        BIGINT  NOT NULL,
    PRIMARY KEY (chain_id, number)
);

-- One row per AgentPayRouter.InvoicePaid log.
-- status:   0 = trusted (sequencer), 1 = virtual (batch on L1), 2 = verified (proof on L1)
-- required: the status this payment's amount needs before it is announced as confirmed
CREATE TABLE payments (
    id           BIGSERIAL    PRIMARY KEY,
    chain_id     BIGINT       NOT NULL,
    block_number BIGINT       NOT NULL,
    block_hash   TEXT         NOT NULL,
    block_time   BIGINT       NOT NULL,
    tx_hash      TEXT         NOT NULL,
    log_index    INTEGER      NOT NULL,
    invoice_id   TEXT         NOT NULL,
    merchant     TEXT         NOT NULL,
    payer        TEXT         NOT NULL,
    agent        TEXT,
    amount       NUMERIC(78)  NOT NULL CHECK (amount > 0),
    status       SMALLINT     NOT NULL CHECK (status BETWEEN 0 AND 2),
    required     SMALLINT     NOT NULL CHECK (required BETWEEN 0 AND 2),
    confirmed_at TIMESTAMPTZ,
    verified_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- The database, not application code, guarantees a log is recorded once
    UNIQUE (chain_id, tx_hash, log_index)
);
CREATE INDEX payments_block_idx ON payments (chain_id, block_number);
CREATE INDEX payments_merchant_idx ON payments (chain_id, merchant, block_number DESC);
CREATE INDEX payments_invoice_idx ON payments (chain_id, merchant, invoice_id);
CREATE INDEX payments_pending_idx ON payments (chain_id, status) WHERE verified_at IS NULL;

-- Audit trail of payments removed by reorgs
CREATE TABLE orphaned_payments (
    id            BIGSERIAL    PRIMARY KEY,
    chain_id      BIGINT       NOT NULL,
    block_number  BIGINT       NOT NULL,
    block_hash    TEXT         NOT NULL,
    tx_hash       TEXT         NOT NULL,
    log_index     INTEGER      NOT NULL,
    invoice_id    TEXT         NOT NULL,
    merchant      TEXT         NOT NULL,
    payer         TEXT         NOT NULL,
    amount        NUMERIC(78)  NOT NULL,
    was_confirmed BOOLEAN      NOT NULL,
    orphaned_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE sync_state (
    chain_id        BIGINT PRIMARY KEY,
    virtual_block   BIGINT NOT NULL DEFAULT 0,
    finalized_block BIGINT NOT NULL DEFAULT 0,
    latest_seen     BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transactional outbox: events are written in the same transaction as the payments they describe,
-- then relayed to the merchant's webhook. event_id is deterministic, so receivers can deduplicate.
CREATE TABLE outbox (
    id           BIGSERIAL   PRIMARY KEY,
    event_id     TEXT        NOT NULL UNIQUE,
    type         TEXT        NOT NULL,
    payload      JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INTEGER     NOT NULL DEFAULT 0,
    last_error   TEXT
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
