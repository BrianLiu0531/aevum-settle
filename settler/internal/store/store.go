// Package store persists blocks, payments and outbox events in PostgreSQL.
//
// Every write that changes the settler's position happens in one transaction that also checks
// the cursor has not moved, so a crash or a second instance can never leave partial or duplicate data.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
)

var (
	// ErrCursorMoved means another writer changed the indexed chain since the caller read the cursor.
	ErrCursorMoved = errors.New("cursor moved since it was read")
	// ErrBelowFinalized means a rewind would remove verified blocks, which must never happen.
	ErrBelowFinalized = errors.New("refusing to rewind below the verified block")
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the PostgreSQL store for one chain.
type Store struct {
	pool    *pgxpool.Pool
	chainID int64
}

// Option customises Open.
type Option func(*pgxpool.Config)

// WithSchema puts all tables in the given schema (used to isolate tests).
func WithSchema(schema string) Option {
	return func(c *pgxpool.Config) { c.ConnConfig.RuntimeParams["search_path"] = schema }
}

// Open connects to PostgreSQL. Call Migrate before first use.
func Open(ctx context.Context, databaseURL string, chainID uint64, opts ...Option) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	for _, o := range opts {
		o(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{pool: pool, chainID: int64(chainID)}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ChainID is the chain this store holds data for.
func (s *Store) ChainID() uint64 { return uint64(s.chainID) }

// Migrate applies any migrations that have not run yet.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialise concurrent start-ups
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4400501)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, name := range names {
			version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			sql, err := migrations.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return err
			}
		}
		return nil
	})
}

// Cursor returns the highest stored block, or nil before the first batch.
func (s *Store) Cursor(ctx context.Context) (*chain.Header, error) {
	return cursor(ctx, s.pool, s.chainID)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func cursor(ctx context.Context, q querier, chainID int64) (*chain.Header, error) {
	return scanHeader(q.QueryRow(ctx, `SELECT number, hash, parent_hash, time FROM blocks WHERE chain_id = $1 ORDER BY number DESC LIMIT 1`, chainID))
}

// BlockAtOrBelow returns the highest stored block at or below n, or nil if there is none.
func (s *Store) BlockAtOrBelow(ctx context.Context, n uint64) (*chain.Header, error) {
	return scanHeader(s.pool.QueryRow(ctx,
		`SELECT number, hash, parent_hash, time FROM blocks WHERE chain_id = $1 AND number <= $2 ORDER BY number DESC LIMIT 1`, s.chainID, int64(n)))
}

func scanHeader(row pgx.Row) (*chain.Header, error) {
	var (
		number, time int64
		hash, parent string
	)
	if err := row.Scan(&number, &hash, &parent, &time); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &chain.Header{Number: uint64(number), Hash: common.HexToHash(hash), ParentHash: common.HexToHash(parent), Time: uint64(time)}, nil
}

// SyncState is the assurance the settler has applied, and the latest head it has seen.
type SyncState struct {
	Virtual, Finalized, LatestSeen uint64
}

func (s *Store) State(ctx context.Context) (SyncState, error) {
	var st struct{ virtual, fin, latest int64 }
	err := s.pool.QueryRow(ctx, `SELECT virtual_block, finalized_block, latest_seen FROM sync_state WHERE chain_id = $1`, s.chainID).
		Scan(&st.virtual, &st.fin, &st.latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncState{}, nil
	}
	return SyncState{Virtual: uint64(st.virtual), Finalized: uint64(st.fin), LatestSeen: uint64(st.latest)}, err
}

// Batch is one step forward: new canonical blocks on top of Parent, the payments in them,
// and the chain's current assurance heights.
type Batch struct {
	Parent   *chain.Header // the cursor the batch was built on; nil for the first batch
	Blocks   []chain.Header
	Payments []chain.Payment
	Tips     chain.Tips
	Policy   Policy
}

// Result reports what a batch changed.
type Result struct {
	Inserted  int // new payment rows
	Confirmed int // payments that reached their required level (one payment.confirmed each)
	Verified  int // payments that became verified
}

// blocksKeptBelowFinalized is a margin of block hashes kept under the verified height.
const blocksKeptBelowFinalized = 128

// Apply writes a batch atomically. It fails with ErrCursorMoved if Parent is no longer the cursor.
// Payments that are already stored are skipped, so replaying a batch is harmless.
func (s *Store) Apply(ctx context.Context, b Batch) (Result, error) {
	var res Result
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAndCheck(ctx, tx, b.Parent); err != nil {
			return err
		}

		top := b.Parent
		for i := range b.Blocks {
			h := b.Blocks[i]
			if _, err := tx.Exec(ctx, `INSERT INTO blocks (chain_id, number, hash, parent_hash, time) VALUES ($1, $2, $3, $4, $5)`,
				s.chainID, int64(h.Number), h.Hash.Hex(), h.ParentHash.Hex(), int64(h.Time)); err != nil {
				return fmt.Errorf("insert block %d: %w", h.Number, err)
			}
			top = &b.Blocks[i]
		}
		if top == nil {
			return nil
		}

		if len(b.Payments) > 0 {
			n, err := s.insertPayments(ctx, tx, b.Payments, b.Blocks, b.Policy)
			if err != nil {
				return err
			}
			res.Inserted = n
		}

		// Assurance can only be applied to blocks we have stored
		virtual, fin := min(b.Tips.Virtual, top.Number), min(b.Tips.Finalized, top.Number)
		if _, err := tx.Exec(ctx, `UPDATE payments SET status = 2 WHERE chain_id = $1 AND status < 2 AND block_number <= $2`, s.chainID, int64(fin)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET status = 1 WHERE chain_id = $1 AND status < 1 AND block_number <= $2`, s.chainID, int64(virtual)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO sync_state (chain_id, virtual_block, finalized_block, latest_seen) VALUES ($1, $2, $3, $4)
			ON CONFLICT (chain_id) DO UPDATE SET
				virtual_block = GREATEST(sync_state.virtual_block, EXCLUDED.virtual_block),
				finalized_block = GREATEST(sync_state.finalized_block, EXCLUDED.finalized_block),
				latest_seen = EXCLUDED.latest_seen,
				updated_at = now()`,
			s.chainID, int64(virtual), int64(fin), int64(b.Tips.Latest)); err != nil {
			return err
		}

		var err error
		if res.Confirmed, err = s.emitConfirmations(ctx, tx); err != nil {
			return err
		}
		if res.Verified, err = s.emitVerified(ctx, tx); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `DELETE FROM blocks WHERE chain_id = $1 AND number < $2`, s.chainID, int64(fin)-blocksKeptBelowFinalized)
		return err
	})
	return res, err
}

// lockAndCheck serialises writers for this chain and verifies the cursor is still `expected`.
func (s *Store) lockAndCheck(ctx context.Context, tx pgx.Tx, expected *chain.Header) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4400502, $1::int)`, int32(s.chainID)); err != nil {
		return err
	}
	cur, err := cursor(ctx, tx, s.chainID)
	if err != nil {
		return err
	}
	switch {
	case cur == nil && expected == nil:
		return nil
	case cur == nil || expected == nil, cur.Number != expected.Number, cur.Hash != expected.Hash:
		return ErrCursorMoved
	}
	return nil
}

func (s *Store) insertPayments(ctx context.Context, tx pgx.Tx, ps []chain.Payment, blocks []chain.Header, policy Policy) (int, error) {
	times := make(map[uint64]uint64, len(blocks))
	for _, h := range blocks {
		times[h.Number] = h.Time
	}
	var (
		numbers, blockTimes                                         []int64
		logIndexes                                                  []int32
		required                                                    []int16
		blockHashes, txHashes, invoices, merchants, payers, amounts []string
		agents                                                      []*string
	)
	for _, p := range ps {
		bt, ok := times[p.BlockNumber]
		if !ok {
			return 0, fmt.Errorf("payment %s is in block %d, which is not part of the batch", p.Key(), p.BlockNumber)
		}
		if p.Amount.Sign() <= 0 {
			continue // the router rejects zero amounts; never announce one
		}
		numbers = append(numbers, int64(p.BlockNumber))
		blockTimes = append(blockTimes, int64(bt))
		blockHashes = append(blockHashes, p.BlockHash.Hex())
		txHashes = append(txHashes, p.TxHash.Hex())
		logIndexes = append(logIndexes, int32(p.LogIndex))
		invoices = append(invoices, p.InvoiceID.Hex())
		merchants = append(merchants, addr(p.Merchant))
		payers = append(payers, addr(p.Payer))
		var agent *string
		if p.Agent != (common.Address{}) {
			a := addr(p.Agent)
			agent = &a
		}
		agents = append(agents, agent)
		amounts = append(amounts, p.Amount.String())
		required = append(required, int16(policy.Required(p.Amount)))
	}
	if len(numbers) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO payments (chain_id, block_number, block_time, block_hash, tx_hash, log_index, invoice_id, merchant, payer, agent, amount, status, required)
		SELECT $1, n, bt, bh, th, li, inv, m, p, a, v::numeric, 0, r
		FROM unnest($2::bigint[], $3::bigint[], $4::text[], $5::text[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::smallint[])
			AS u(n, bt, bh, th, li, inv, m, p, a, v, r)
		ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
		s.chainID, numbers, blockTimes, blockHashes, txHashes, logIndexes, invoices, merchants, payers, agents, amounts, required)
	if err != nil {
		return 0, fmt.Errorf("insert payments: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// eventPayload builds an outbox (event_id, type, payload) triple from a payments-shaped row alias.
func eventPayload(eventType, alias string) string {
	return fmt.Sprintf(`'%[1]s:' || %[2]s.chain_id || ':' || %[2]s.tx_hash || ':' || %[2]s.log_index || ':' || %[2]s.block_hash,
		'%[1]s',
		jsonb_build_object(
			'type', '%[1]s',
			'chainId', %[2]s.chain_id,
			'invoiceId', %[2]s.invoice_id,
			'merchant', %[2]s.merchant,
			'payer', %[2]s.payer,
			'agent', %[2]s.agent,
			'amount', %[2]s.amount::text,
			'txHash', %[2]s.tx_hash,
			'logIndex', %[2]s.log_index,
			'blockNumber', %[2]s.block_number,
			'blockHash', %[2]s.block_hash,
			'blockTime', %[2]s.block_time,
			'assurance', (ARRAY['trusted','virtual','verified'])[%[2]s.status + 1],
			'required', (ARRAY['trusted','virtual','verified'])[%[2]s.required + 1]
		)`, eventType, alias)
}

// emitConfirmations marks payments that reached their required level and queues payment.confirmed for each.
func (s *Store) emitConfirmations(ctx context.Context, tx pgx.Tx) (int, error) {
	tag, err := tx.Exec(ctx, `
		WITH c AS (
			UPDATE payments SET confirmed_at = now()
			WHERE chain_id = $1 AND confirmed_at IS NULL AND status >= required
			RETURNING *
		)
		INSERT INTO outbox (event_id, type, payload)
		SELECT `+eventPayload("payment.confirmed", "c")+` FROM c
		ORDER BY c.block_number, c.log_index
		ON CONFLICT (event_id) DO NOTHING`, s.chainID)
	if err != nil {
		return 0, fmt.Errorf("emit confirmations: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// emitVerified marks payments that became verified. Those confirmed earlier on less assurance get a
// payment.verified event, which closes the window in which they could have been reverted.
func (s *Store) emitVerified(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		WITH v AS (
			UPDATE payments SET verified_at = now()
			WHERE chain_id = $1 AND verified_at IS NULL AND status = 2
			RETURNING *
		), events AS (
			INSERT INTO outbox (event_id, type, payload)
			SELECT `+eventPayload("payment.verified", "v")+` FROM v WHERE v.required < 2
			ORDER BY v.block_number, v.log_index
			ON CONFLICT (event_id) DO NOTHING
		)
		SELECT count(*) FROM v`, s.chainID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("emit verified: %w", err)
	}
	return n, nil
}

// Rewind removes every block above ancestor after a reorg. Removed payments are copied to
// orphaned_payments, and a payment.reverted event is queued for any that had been confirmed.
func (s *Store) Rewind(ctx context.Context, expected *chain.Header, ancestor uint64) (orphaned int, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockAndCheck(ctx, tx, expected); err != nil {
			return err
		}
		var fin int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT finalized_block FROM sync_state WHERE chain_id = $1), 0)`, s.chainID).Scan(&fin); err != nil {
			return err
		}
		if int64(ancestor) < fin {
			return fmt.Errorf("%w: ancestor %d < verified %d", ErrBelowFinalized, ancestor, fin)
		}
		if err := tx.QueryRow(ctx, `
			WITH gone AS (
				DELETE FROM payments WHERE chain_id = $1 AND block_number > $2 RETURNING *
			), audit AS (
				INSERT INTO orphaned_payments (chain_id, block_number, block_hash, tx_hash, log_index, invoice_id, merchant, payer, amount, was_confirmed)
				SELECT chain_id, block_number, block_hash, tx_hash, log_index, invoice_id, merchant, payer, amount, confirmed_at IS NOT NULL FROM gone
			), events AS (
				INSERT INTO outbox (event_id, type, payload)
				SELECT `+eventPayload("payment.reverted", "gone")+` FROM gone WHERE gone.confirmed_at IS NOT NULL
				ON CONFLICT (event_id) DO NOTHING
			)
			SELECT count(*) FROM gone`, s.chainID, int64(ancestor)).Scan(&orphaned); err != nil {
			return fmt.Errorf("remove orphaned payments: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM blocks WHERE chain_id = $1 AND number > $2`, s.chainID, int64(ancestor)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sync_state SET virtual_block = LEAST(virtual_block, $2), updated_at = now() WHERE chain_id = $1`, s.chainID, int64(ancestor))
		return err
	})
	return orphaned, err
}

func addr(a common.Address) string { return strings.ToLower(a.Hex()) }

func parseBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}
