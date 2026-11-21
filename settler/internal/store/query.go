package store

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

const paymentColumns = `block_number, block_hash, block_time, tx_hash, log_index, invoice_id, merchant, payer, COALESCE(agent, ''),
	amount::text, status, required, confirmed_at, verified_at`

func scanPayment(r pgx.CollectableRow) (PaymentRow, error) {
	var (
		p              PaymentRow
		n, bt          int64
		amount         string
		status, needed int16
	)
	err := r.Scan(&n, &p.BlockHash, &bt, &p.TxHash, &p.LogIndex, &p.InvoiceID, &p.Merchant, &p.Payer, &p.Agent,
		&amount, &status, &needed, &p.ConfirmedAt, &p.VerifiedAt)
	p.BlockNumber, p.BlockTime, p.Amount = uint64(n), time.Unix(bt, 0).UTC(), parseBig(amount)
	p.Status, p.Required = Level(status), Level(needed)
	return p, err
}

// PaymentRow is a stored payment.
type PaymentRow struct {
	BlockNumber uint64
	BlockHash   string
	BlockTime   time.Time
	TxHash      string
	LogIndex    int
	InvoiceID   string
	Merchant    string
	Payer       string
	Agent       string // empty for a direct payment
	Amount      *big.Int
	Status      Level
	Required    Level
	ConfirmedAt *time.Time
	VerifiedAt  *time.Time
}

// Page is a keyset position: payments strictly older than (BeforeBlock, BeforeLog).
type Page struct {
	BeforeBlock uint64
	BeforeLog   int
	Limit       int
}

// ListPayments returns payments newest first, optionally only those to merchant.
func (s *Store) ListPayments(ctx context.Context, merchant *common.Address, p Page) ([]PaymentRow, error) {
	before := int64(p.BeforeBlock)
	if before == 0 {
		before = 1<<62 - 1
	}
	m := ""
	if merchant != nil {
		m = addr(*merchant)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+paymentColumns+`
		FROM payments
		WHERE chain_id = $1 AND ($2 = '' OR merchant = $2) AND (block_number, log_index) < ($3, $4)
		ORDER BY block_number DESC, log_index DESC
		LIMIT $5`, s.chainID, m, before, p.BeforeLog, p.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPayment)
}

// Invoice returns the canonical payment of a merchant's invoice, or nil if it has not been seen.
// The router accepts one payment per (merchant, invoice), so there is at most one.
func (s *Store) Invoice(ctx context.Context, merchant common.Address, invoiceID common.Hash) (*PaymentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+paymentColumns+` FROM payments
		WHERE chain_id = $1 AND merchant = $2 AND invoice_id = $3
		ORDER BY block_number DESC LIMIT 1`, s.chainID, addr(merchant), invoiceID.Hex())
	if err != nil {
		return nil, err
	}
	p, err := pgx.CollectOneRow(rows, scanPayment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Totals summarises stored payments.
type Totals struct {
	Count, Confirmed, Verified int
	Volume                     *big.Int
	Orphaned                   int
}

func (s *Store) Totals(ctx context.Context) (Totals, error) {
	var t Totals
	var vol string
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(confirmed_at), count(verified_at), COALESCE(SUM(amount), 0)::text,
			(SELECT count(*) FROM orphaned_payments WHERE chain_id = $1)
		FROM payments WHERE chain_id = $1`, s.chainID).Scan(&t.Count, &t.Confirmed, &t.Verified, &vol, &t.Orphaned)
	t.Volume = parseBig(vol)
	return t, err
}

// Event is an outbox entry.
type Event struct {
	ID          int64
	EventID     string
	Type        string
	Payload     json.RawMessage
	CreatedAt   time.Time
	PublishedAt *time.Time
	Attempts    int
	LastError   *string
}

func scanEvent(r pgx.CollectableRow) (Event, error) {
	var e Event
	err := r.Scan(&e.ID, &e.EventID, &e.Type, &e.Payload, &e.CreatedAt, &e.PublishedAt, &e.Attempts, &e.LastError)
	return e, err
}

const eventColumns = `id, event_id, type, payload, created_at, published_at, attempts, last_error`

// PublishOutbox hands up to limit unpublished events, oldest first, to publish, and marks them
// published only if it succeeds. Rows are locked with SKIP LOCKED, so several relays can run at once.
// Delivery is at-least-once: if the process dies after publishing but before committing, the events
// are sent again, which receivers handle by deduplicating on EventID.
func (s *Store) PublishOutbox(ctx context.Context, limit int, publish func(context.Context, []Event) error) (int, error) {
	var (
		n          int
		publishErr error
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+eventColumns+` FROM outbox
			WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		events, err := pgx.CollectRows(rows, scanEvent)
		if err != nil || len(events) == 0 {
			return err
		}
		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		if publishErr = publish(ctx, events); publishErr != nil {
			// Record the failure for the dashboard and keep the events queued
			msg := publishErr.Error()
			_, err := tx.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = ANY($1)`, ids, msg)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now(), attempts = attempts + 1, last_error = NULL WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
		n = len(events)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, publishErr
}

// OutboxEvents returns every outbox event in order (for tests and debugging).
func (s *Store) OutboxEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM outbox ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEvent)
}

// RecentEvents returns the newest outbox events first.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM outbox ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEvent)
}

// AllPayments returns every stored payment in chain order (for tests and debugging).
func (s *Store) AllPayments(ctx context.Context) ([]PaymentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+paymentColumns+`
		FROM payments WHERE chain_id = $1 ORDER BY block_number, log_index`, s.chainID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPayment)
}
