package store

import (
	"context"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// MandateEventRow is one stored MandateGranted or MandateRevoked log.
type MandateEventRow struct {
	BlockNumber   uint64
	BlockTime     int64
	TxHash        string
	LogIndex      int
	Kind          string // granted or revoked
	Owner, Agent  string
	PerPaymentCap *big.Int // nil for revoked
	DailyCap      *big.Int // nil for revoked
	ExpiresAt     *int64   // nil for revoked
	Merchants     []string
}

func optBig(s *string) *big.Int {
	if s == nil {
		return nil
	}
	return parseBig(*s)
}

// ListMandateEvents returns mandate events newest first, optionally for one owner and/or agent.
func (s *Store) ListMandateEvents(ctx context.Context, owner, agent *common.Address, limit int) ([]MandateEventRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT block_number, block_time, tx_hash, log_index, kind, owner, agent,
			per_payment_cap::text, daily_cap::text, expires_at, merchants
		FROM mandate_events
		WHERE chain_id = $1 AND ($2::text IS NULL OR owner = $2) AND ($3::text IS NULL OR agent = $3)
		ORDER BY block_number DESC, log_index DESC
		LIMIT $4`, s.chainID, optAddr(owner), optAddr(agent), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MandateEventRow, error) {
		var (
			m          MandateEventRow
			n          int64
			per, daily *string
		)
		err := r.Scan(&n, &m.BlockTime, &m.TxHash, &m.LogIndex, &m.Kind, &m.Owner, &m.Agent, &per, &daily, &m.ExpiresAt, &m.Merchants)
		m.BlockNumber, m.PerPaymentCap, m.DailyCap = uint64(n), optBig(per), optBig(daily)
		return m, err
	})
}

// MandateSummary is the current state of one owner/agent pair: its latest mandate event and what
// the agent has paid from the owner's wallet so far.
type MandateSummary struct {
	Latest        MandateEventRow
	Grants        int
	FirstGranted  int64
	Payments      int
	Spent         *big.Int
	LastPaymentAt *int64
}

// MandateSummaries lists every owner/agent pair, most recently changed first.
func (s *Store) MandateSummaries(ctx context.Context, owner *common.Address, limit int) ([]MandateSummary, error) {
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (owner, agent) *
			FROM mandate_events
			WHERE chain_id = $1 AND ($2::text IS NULL OR owner = $2)
			ORDER BY owner, agent, block_number DESC, log_index DESC
		), history AS (
			SELECT owner, agent, count(*) FILTER (WHERE kind = 'granted') AS grants, min(block_time) AS first_time
			FROM mandate_events WHERE chain_id = $1 GROUP BY owner, agent
		), paid AS (
			SELECT payer, agent, count(*) AS n, sum(amount) AS total, max(block_time) AS last_time
			FROM payments WHERE chain_id = $1 AND agent IS NOT NULL GROUP BY payer, agent
		)
		SELECT l.block_number, l.block_time, l.tx_hash, l.log_index, l.kind, l.owner, l.agent,
			l.per_payment_cap::text, l.daily_cap::text, l.expires_at, l.merchants,
			h.grants, h.first_time, COALESCE(p.n, 0), COALESCE(p.total, 0)::text, p.last_time
		FROM latest l
		JOIN history h USING (owner, agent)
		LEFT JOIN paid p ON p.payer = l.owner AND p.agent = l.agent
		ORDER BY l.block_number DESC, l.log_index DESC
		LIMIT $3`, s.chainID, optAddr(owner), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MandateSummary, error) {
		var (
			m          MandateSummary
			n          int64
			per, daily *string
			spent      string
		)
		err := r.Scan(&n, &m.Latest.BlockTime, &m.Latest.TxHash, &m.Latest.LogIndex, &m.Latest.Kind, &m.Latest.Owner, &m.Latest.Agent,
			&per, &daily, &m.Latest.ExpiresAt, &m.Latest.Merchants, &m.Grants, &m.FirstGranted, &m.Payments, &spent, &m.LastPaymentAt)
		m.Latest.BlockNumber, m.Latest.PerPaymentCap, m.Latest.DailyCap = uint64(n), optBig(per), optBig(daily)
		m.Spent = parseBig(spent)
		return m, err
	})
}

func optAddr(a *common.Address) *string {
	if a == nil {
		return nil
	}
	s := strings.ToLower(a.Hex())
	return &s
}
