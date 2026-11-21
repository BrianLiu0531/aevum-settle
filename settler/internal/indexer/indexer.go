// Package indexer follows the chain and records AgentPayRouter payments exactly once,
// surviving reorgs, RPC failures and restarts.
//
// Each Step moves the cursor (the highest stored block) forward by one batch:
//
//   - Blocks at or below the finalized (verified) height cannot reorg, so only the headers of the batch
//     boundaries and of blocks that contain payments are fetched.
//   - Above the finalized height every header is stored, so a reorg can be traced back to the
//     common ancestor. A batch is only committed if the first new block's parent hash matches the
//     cursor, every log's block hash matches its header, and the batch tip is unchanged after the
//     logs were fetched.
//   - If the parent hash does not match, the chain reorganised: the indexer walks back to the last
//     stored block that is still canonical and rewinds to it.
//
// The store writes each batch, together with the cursor and outbox events, in one transaction.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/metrics"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
)

// Config controls what is indexed and how fast.
type Config struct {
	Router common.Address // the AgentPayRouter contract
	// Merchants limits indexing to payments to these merchants; empty = every merchant.
	Merchants []common.Address
	// StartBlock is the first block to index. 0 means "the finalized block at first start-up".
	StartBlock uint64
	// MaxRange caps the blocks per eth_getLogs query. It shrinks automatically when a node rejects a range.
	MaxRange uint64
	// MaxUnfinalizedBatch caps the unfinalized blocks per step (each needs its header fetched).
	MaxUnfinalizedBatch uint64
	// Policy decides, by amount, the assurance level at which payment.confirmed is emitted.
	Policy       store.Policy
	PollInterval time.Duration
}

// Indexer syncs the router's InvoicePaid events.
type Indexer struct {
	cfg   Config
	chain chain.Client
	store *store.Store
	log   *slog.Logger
	span  uint64 // current log query range

	// ceiling is one less than the smallest range the node has rejected; span never grows past it.
	// It is forgotten after a run of successes, since result-count limits depend on log density.
	ceiling   uint64
	successes int
}

const forgetCeilingAfter = 200

// errChainMoved means the node's view changed while a batch was being fetched; the step is retried.
var errChainMoved = errors.New("chain changed while fetching a batch")

// errReorg means the cursor block is no longer canonical.
var errReorg = errors.New("reorg detected")

func New(cfg Config, c chain.Client, s *store.Store, log *slog.Logger) *Indexer {
	if cfg.MaxRange == 0 {
		cfg.MaxRange = 2000
	}
	if cfg.MaxUnfinalizedBatch == 0 {
		cfg.MaxUnfinalizedBatch = 100
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Indexer{cfg: cfg, chain: c, store: s, log: log, span: cfg.MaxRange, ceiling: cfg.MaxRange}
}

// StepResult describes what one Step did.
type StepResult struct {
	From, To  uint64 // blocks committed, when To >= From
	Payments  int    // new payments stored
	Confirmed int    // payments that reached their required level
	Orphaned  int    // payments removed by a reorg
	Reorg     bool
	CaughtUp  bool // nothing new to index
}

// Run calls Step until ctx is cancelled, backing off after errors.
func (ix *Indexer) Run(ctx context.Context) error {
	failures := 0
	for {
		res, err := ix.Step(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := time.Duration(0)
		switch {
		case err == nil:
			failures = 0
			if res.CaughtUp {
				wait = ix.cfg.PollInterval
			}
		case errors.Is(err, errChainMoved):
			ix.log.Debug("chain moved during fetch, retrying", "err", err)
		default:
			failures++
			wait = min(time.Minute, ix.cfg.PollInterval<<min(failures, 5))
			level := slog.LevelWarn
			if errors.Is(err, store.ErrBelowFinalized) {
				level = slog.LevelError // needs a human: the node disagrees with data we treated as final
			}
			ix.log.Log(ctx, level, "index step failed", "err", err, "retry_in", wait)
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

// Step indexes at most one batch, or handles a reorg, or applies new finality when caught up.
func (ix *Indexer) Step(ctx context.Context) (StepResult, error) {
	tips, err := ix.chain.Tips(ctx)
	if err != nil {
		return StepResult{}, fmt.Errorf("get chain tips: %w", err)
	}
	metrics.ChainHeight.WithLabelValues("trusted").Set(float64(tips.Latest))
	metrics.ChainHeight.WithLabelValues("virtual").Set(float64(tips.Virtual))
	metrics.ChainHeight.WithLabelValues("verified").Set(float64(tips.Finalized))

	cur, err := ix.store.Cursor(ctx)
	if err != nil {
		return StepResult{}, err
	}
	from := ix.cfg.StartBlock
	switch {
	case cur != nil:
		from = cur.Number + 1
	case from == 0:
		from = tips.Finalized // a finalized starting point can never be reorged away
	}

	if from > tips.Latest {
		return ix.idle(ctx, cur, tips)
	}

	var blocks []chain.Header
	var logs routerLogs
	if from <= tips.Finalized {
		blocks, logs, err = ix.finalizedBatch(ctx, cur, from, min(tips.Finalized, from+ix.span-1))
	} else {
		blocks, logs, err = ix.unfinalizedBatch(ctx, cur, from, min(tips.Latest, from+min(ix.span, ix.cfg.MaxUnfinalizedBatch)-1))
	}
	if errors.Is(err, errReorg) {
		return ix.rewind(ctx, cur)
	}
	if err != nil {
		return StepResult{}, err
	}

	res, err := ix.store.Apply(ctx, store.Batch{Parent: cur, Blocks: blocks, Payments: logs.payments, Mandates: logs.mandates, Tips: tips, Policy: ix.cfg.Policy})
	if err != nil {
		return StepResult{}, fmt.Errorf("apply blocks %d-%d: %w", from, blocks[len(blocks)-1].Number, err)
	}
	to := blocks[len(blocks)-1].Number
	metrics.IndexedBlock.Set(float64(to))
	metrics.PaymentsIndexed.Add(float64(res.Inserted))
	if res.Inserted > 0 || res.Confirmed > 0 || res.Verified > 0 {
		ix.log.Info("indexed", "from", from, "to", to, "payments", res.Inserted, "confirmed", res.Confirmed, "verified", res.Verified)
	}
	return StepResult{From: from, To: to, Payments: res.Inserted, Confirmed: res.Confirmed}, nil
}

// idle runs when there are no new blocks: it checks the cursor is still canonical
// (a reorg can replace blocks without making the chain longer) and applies new finality.
func (ix *Indexer) idle(ctx context.Context, cur *chain.Header, tips chain.Tips) (StepResult, error) {
	if cur == nil {
		return StepResult{CaughtUp: true}, nil
	}
	if tips.Latest < cur.Number {
		// The node is behind us. That is far more common (a lagging node behind a load balancer, or a
		// failover) than a reorg that shortens the chain, and the two look the same. Only rewind on
		// positive evidence: a hash mismatch once the node has a block at our height again.
		ix.log.Debug("node is behind the cursor, waiting", "node_head", tips.Latest, "cursor", cur.Number)
		return StepResult{CaughtUp: true}, nil
	}
	h, err := ix.chain.Header(ctx, cur.Number)
	if err != nil {
		return StepResult{}, err // includes ErrNotFound from an inconsistent node: retry, never rewind
	}
	if h.Hash != cur.Hash {
		return ix.rewind(ctx, cur)
	}
	res, err := ix.store.Apply(ctx, store.Batch{Parent: cur, Tips: tips, Policy: ix.cfg.Policy})
	if err != nil {
		return StepResult{}, err
	}
	if res.Confirmed > 0 || res.Verified > 0 {
		ix.log.Info("assurance advanced", "confirmed", res.Confirmed, "verified", res.Verified)
	}
	return StepResult{CaughtUp: true, Confirmed: res.Confirmed}, nil
}

// finalizedBatch indexes from..to, all at or below the finalized height.
func (ix *Indexer) finalizedBatch(ctx context.Context, cur *chain.Header, from, to uint64) ([]chain.Header, routerLogs, error) {
	logs, to, err := ix.fetchLogs(ctx, from, to)
	if err != nil {
		return nil, routerLogs{}, err
	}
	// Headers for the batch boundaries (parent check and new cursor) and for blocks with logs (timestamps)
	want := map[uint64]bool{from: true, to: true}
	for _, t := range logs.payments {
		want[t.BlockNumber] = true
	}
	for _, m := range logs.mandates {
		want[m.BlockNumber] = true
	}
	numbers := make([]uint64, 0, len(want))
	for n := range want {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	headers, err := ix.chain.Headers(ctx, numbers)
	if err != nil {
		return nil, routerLogs{}, err
	}
	if err := checkBatch(cur, headers, logs, false); err != nil {
		return nil, routerLogs{}, err
	}
	return headers, logs, nil
}

// unfinalizedBatch indexes from..to above the finalized height, storing every header.
func (ix *Indexer) unfinalizedBatch(ctx context.Context, cur *chain.Header, from, to uint64) ([]chain.Header, routerLogs, error) {
	headers, err := ix.chain.Headers(ctx, chain.Range(from, to))
	if err != nil {
		return nil, routerLogs{}, err
	}
	logs, to, err := ix.fetchLogs(ctx, from, to)
	if err != nil {
		return nil, routerLogs{}, err
	}
	headers = headers[:to-from+1]
	if err := checkBatch(cur, headers, logs, true); err != nil {
		return nil, routerLogs{}, err
	}
	// The logs were read after the headers. If the batch tip is unchanged, the whole range is
	// unchanged (each header commits to its parent), so the logs and headers describe the same chain.
	tip, err := ix.chain.Header(ctx, to)
	if err != nil {
		return nil, routerLogs{}, err
	}
	if tip.Hash != headers[len(headers)-1].Hash {
		return nil, routerLogs{}, fmt.Errorf("%w: block %d changed", errChainMoved, to)
	}
	return headers, logs, nil
}

// checkBatch verifies the batch extends the cursor and that every log belongs to its block's header.
func checkBatch(cur *chain.Header, headers []chain.Header, logs routerLogs, contiguous bool) error {
	if cur != nil && headers[0].ParentHash != cur.Hash {
		return errReorg
	}
	byNumber := make(map[uint64]chain.Header, len(headers))
	for i, h := range headers {
		if contiguous && i > 0 && h.ParentHash != headers[i-1].Hash {
			return fmt.Errorf("%w: block %d does not extend block %d", errChainMoved, h.Number, headers[i-1].Number)
		}
		byNumber[h.Number] = h
	}
	for _, t := range logs.payments {
		h, ok := byNumber[t.BlockNumber]
		if !ok || h.Hash != t.BlockHash {
			return fmt.Errorf("%w: log %s is from a different block %d", errChainMoved, t.Key(), t.BlockNumber)
		}
	}
	for _, m := range logs.mandates {
		h, ok := byNumber[m.BlockNumber]
		if !ok || h.Hash != m.BlockHash {
			return fmt.Errorf("%w: log %s is from a different block %d", errChainMoved, m.Key(), m.BlockNumber)
		}
	}
	return nil
}

// routerLogs are the router's logs in one batch range.
type routerLogs struct {
	payments []chain.Payment
	mandates []chain.MandateEvent
}

// fetchLogs queries payment and mandate logs for from..to, halving the range while the node says it is too large.
// It returns the payments and the end of the range actually covered. After a success the range
// doubles again, up to the largest size the node has not rejected.
func (ix *Indexer) fetchLogs(ctx context.Context, from, to uint64) (routerLogs, uint64, error) {
	for {
		ts, err := ix.chain.Payments(ctx, ix.cfg.Router, ix.cfg.Merchants, from, to)
		var ms []chain.MandateEvent
		if err == nil {
			ms, err = ix.chain.Mandates(ctx, ix.cfg.Router, from, to)
		}
		if err == nil {
			if ix.successes++; ix.successes >= forgetCeilingAfter {
				ix.ceiling, ix.successes = ix.cfg.MaxRange, 0
			}
			if to-from+1 >= ix.span {
				ix.span = min(ix.ceiling, ix.span*2)
			}
			return routerLogs{payments: ts, mandates: ms}, to, nil
		}
		size := to - from + 1
		if !chain.IsRangeError(err) || size == 1 {
			return routerLogs{}, 0, fmt.Errorf("get logs %d-%d: %w", from, to, err)
		}
		ix.ceiling, ix.successes = size-1, 0
		ix.span = max(1, size/2)
		to = from + ix.span - 1
		ix.log.Debug("log range too large, shrinking", "span", ix.span, "ceiling", ix.ceiling)
	}
}

// rewind walks back from the cursor to the newest stored block that is still canonical and removes
// everything above it. Unfinalized blocks are all stored, so the walk finds the exact common ancestor.
func (ix *Indexer) rewind(ctx context.Context, cur *chain.Header) (StepResult, error) {
	st, err := ix.store.State(ctx)
	if err != nil {
		return StepResult{}, err
	}
	n := cur.Number
	lowest := cur.Number
	var ancestor uint64
	for {
		stored, err := ix.store.BlockAtOrBelow(ctx, n)
		if err != nil {
			return StepResult{}, err
		}
		if stored == nil {
			// Every stored block was orphaned (only possible when StartBlock was unfinalized)
			ancestor = lowest - 1
			break
		}
		if stored.Number < st.Finalized {
			return StepResult{}, fmt.Errorf("%w: block %d no longer matches the chain", store.ErrBelowFinalized, stored.Number)
		}
		h, err := ix.chain.Header(ctx, stored.Number)
		if err != nil {
			return StepResult{}, err // a missing block below the node's head means an inconsistent node: retry
		}
		if h.Hash == stored.Hash {
			ancestor = stored.Number
			break
		}
		if stored.Number == 0 {
			return StepResult{}, errors.New("genesis block does not match: wrong network?")
		}
		lowest, n = stored.Number, stored.Number-1
	}

	orphaned, err := ix.store.Rewind(ctx, cur, ancestor)
	if err != nil {
		return StepResult{}, fmt.Errorf("rewind to %d: %w", ancestor, err)
	}
	depth := cur.Number - ancestor
	metrics.Reorgs.Inc()
	metrics.ReorgDepth.Observe(float64(depth))
	metrics.OrphanedPayments.Add(float64(orphaned))
	metrics.IndexedBlock.Set(float64(ancestor))
	ix.log.Warn("reorg: rewound", "from", cur.Number, "to", ancestor, "depth", depth, "orphaned_payments", orphaned)
	return StepResult{Reorg: true, Orphaned: orphaned}, nil
}
