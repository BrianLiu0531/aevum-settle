-- One row per AgentPayRouter.MandateGranted / MandateRevoked log: the audit trail of what each
-- agent was allowed to spend for which owner, and when that ended. Rows above the common ancestor
-- are deleted on a reorg, like payments.
CREATE TABLE mandate_events (
    id              BIGSERIAL    PRIMARY KEY,
    chain_id        BIGINT       NOT NULL,
    block_number    BIGINT       NOT NULL,
    block_hash      TEXT         NOT NULL,
    block_time      BIGINT       NOT NULL,
    tx_hash         TEXT         NOT NULL,
    log_index       INTEGER      NOT NULL,
    kind            TEXT         NOT NULL CHECK (kind IN ('granted', 'revoked')),
    owner           TEXT         NOT NULL,
    agent           TEXT         NOT NULL,
    per_payment_cap NUMERIC(78),
    daily_cap       NUMERIC(78),
    expires_at      BIGINT,
    merchants       TEXT[]       NOT NULL DEFAULT '{}',
    UNIQUE (chain_id, tx_hash, log_index)
);
CREATE INDEX mandate_events_pair_idx ON mandate_events (chain_id, owner, agent, block_number DESC, log_index DESC);
CREATE INDEX mandate_events_block_idx ON mandate_events (chain_id, block_number);
CREATE INDEX payments_agent_idx ON payments (chain_id, payer, agent);
