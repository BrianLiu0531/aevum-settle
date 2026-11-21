package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/time/rate"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/metrics"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/token"
)

// RPCConfig configures the JSON-RPC client.
type RPCConfig struct {
	URLs        []string      // tried in order; the client sticks to the last one that worked
	MaxRPS      float64       // client-side rate limit across all endpoints
	MaxAttempts int           // attempts per call, rotating through endpoints
	Timeout     time.Duration // per attempt
	// ZkEVM reads finality from the zkevm_* namespace instead of the safe/finalized block tags.
	ZkEVM bool
	// LocalVirtualDepth and LocalVerifiedDepth simulate the zkEVM assurance levels on a local dev
	// chain (where safe and finalized equal latest): a block counts as virtual or verified once it
	// is that many blocks deep. Ignored when ZkEVM is set.
	LocalVirtualDepth, LocalVerifiedDepth uint64
}

type endpoint struct {
	label string // scheme://host only, safe to log (URLs often embed API keys)
	url   string
	rpc   *rpc.Client
}

// RPC is a Client backed by one or more JSON-RPC endpoints, with retries, failover and rate limiting.
type RPC struct {
	endpoints []*endpoint
	preferred atomic.Int32
	limiter   *rate.Limiter
	attempts  int
	timeout   time.Duration
	log       *slog.Logger
	batchSize int // max requests per JSON-RPC batch; providers rate-limit each batch element
	zkevm     bool
	simDepth  [2]uint64 // virtual, verified

	mu          sync.Mutex
	batchBlocks map[uint64]uint64 // batch number -> its last block number (immutable once virtual)
}

const headerBatchSize = 100

// DialRPC connects to every configured endpoint.
func DialRPC(ctx context.Context, cfg RPCConfig, log *slog.Logger) (*RPC, error) {
	if len(cfg.URLs) == 0 {
		return nil, errors.New("at least one RPC URL is required")
	}
	c := &RPC{
		limiter:     rate.NewLimiter(rate.Limit(cfg.MaxRPS), max(3, int(cfg.MaxRPS))), // burst covers the tips batch
		attempts:    max(1, cfg.MaxAttempts),
		timeout:     cfg.Timeout,
		log:         log,
		zkevm:       cfg.ZkEVM,
		simDepth:    [2]uint64{cfg.LocalVirtualDepth, cfg.LocalVerifiedDepth},
		batchBlocks: map[uint64]uint64{},
	}
	c.batchSize = min(headerBatchSize, c.limiter.Burst())
	for _, raw := range cfg.URLs {
		rc, err := rpc.DialContext(ctx, raw)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("dial %s: %w", redactURL(raw), scrub(err, raw))
		}
		c.endpoints = append(c.endpoints, &endpoint{label: redactURL(raw), url: raw, rpc: rc})
	}
	return c, nil
}

// Close closes every endpoint.
func (c *RPC) Close() {
	for _, ep := range c.endpoints {
		ep.rpc.Close()
	}
}

// do runs fn with retries and exponential backoff, moving to the next endpoint after each failure.
// cost is the number of requests fn makes (batch elements count individually towards provider limits).
func (c *RPC) do(ctx context.Context, method string, cost int, fn func(ctx context.Context, ep *endpoint) error) error {
	var err error
	start := int(c.preferred.Load())
	for attempt := 0; attempt < c.attempts; attempt++ {
		if werr := c.limiter.WaitN(ctx, cost); werr != nil {
			return werr
		}
		i := (start + attempt) % len(c.endpoints)
		ep := c.endpoints[i]

		actx, cancel := context.WithTimeout(ctx, c.timeout)
		began := time.Now()
		err = fn(actx, ep)
		cancel()
		metrics.RPCDuration.WithLabelValues(method).Observe(time.Since(began).Seconds())

		if err == nil {
			c.preferred.Store(int32(i))
			return nil
		}
		err = scrub(err, ep.url)
		switch {
		case errors.Is(err, ErrNotFound):
			return err
		case IsRangeError(err):
			return fmt.Errorf("%s: %w: %v", method, ErrRangeTooLarge, err)
		case ctx.Err() != nil:
			return ctx.Err()
		}
		metrics.RPCErrors.WithLabelValues(method).Inc()
		c.log.Warn("rpc call failed", "method", method, "endpoint", ep.label, "attempt", attempt+1, "err", err)
		if attempt+1 < c.attempts {
			if serr := sleep(ctx, backoff(attempt)); serr != nil {
				return serr
			}
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", method, c.attempts, err)
}

// call makes one JSON-RPC request with retries.
func (c *RPC) call(ctx context.Context, result any, method string, args ...any) error {
	return c.do(ctx, method, 1, func(ctx context.Context, ep *endpoint) error {
		return ep.rpc.CallContext(ctx, result, method, args...)
	})
}

func (c *RPC) ChainID(ctx context.Context) (uint64, error) {
	var id hexutil.Uint64
	if err := c.call(ctx, &id, "eth_chainId"); err != nil {
		return 0, err
	}
	return uint64(id), nil
}

type rpcHeader struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	Timestamp  hexutil.Uint64 `json:"timestamp"`
}

func (h *rpcHeader) header() Header {
	return Header{Number: uint64(h.Number), Hash: h.Hash, ParentHash: h.ParentHash, Time: uint64(h.Timestamp)}
}

// getBlocks fetches block headers by tag or number in a single batch request.
// Only the header fields are decoded, which keeps this independent of chain-specific transaction types.
func (c *RPC) getBlocks(ctx context.Context, method string, tags []string) ([]Header, error) {
	out := make([]Header, len(tags))
	err := c.do(ctx, method, len(tags), func(ctx context.Context, ep *endpoint) error {
		results := make([]*rpcHeader, len(tags))
		batch := make([]rpc.BatchElem, len(tags))
		for i, tag := range tags {
			batch[i] = rpc.BatchElem{Method: "eth_getBlockByNumber", Args: []any{tag, false}, Result: &results[i]}
		}
		if err := ep.rpc.BatchCallContext(ctx, batch); err != nil {
			return err
		}
		for i, el := range batch {
			if el.Error != nil {
				return el.Error
			}
			if results[i] == nil {
				return fmt.Errorf("%w: %s", ErrNotFound, tags[i])
			}
			out[i] = results[i].header()
		}
		return nil
	})
	return out, err
}

func (c *RPC) Tips(ctx context.Context) (Tips, error) {
	if !c.zkevm {
		hs, err := c.getBlocks(ctx, "tips", []string{"latest", "safe", "finalized"})
		if err != nil {
			return Tips{}, err
		}
		t := Tips{Latest: hs[0].Number, Virtual: hs[1].Number, Finalized: hs[2].Number}
		t.Finalized = min(t.Finalized, t.Latest-min(t.Latest, c.simDepth[1]))
		t.Virtual = max(t.Finalized, min(t.Virtual, t.Latest-min(t.Latest, c.simDepth[0])))
		return t, nil
	}
	// The finalized tag is the last block of the last verified batch. The virtual height is not
	// exposed as a tag, so it is derived from the last virtual batch.
	hs, err := c.getBlocks(ctx, "tips", []string{"latest", "finalized"})
	if err != nil {
		return Tips{}, err
	}
	var vb hexutil.Uint64
	if err := c.call(ctx, &vb, "zkevm_virtualBatchNumber"); err != nil {
		return Tips{}, err
	}
	virtual, err := c.lastBlockOfBatch(ctx, uint64(vb))
	if err != nil {
		return Tips{}, err
	}
	t := Tips{Latest: hs[0].Number, Finalized: hs[1].Number}
	t.Virtual = max(t.Finalized, min(virtual, t.Latest))
	return t, nil
}

type rpcBatch struct {
	Number          hexutil.Uint64 `json:"number"`
	Blocks          []common.Hash  `json:"blocks"`
	SendSequencesTx *common.Hash   `json:"sendSequencesTxHash"`
	VerifyBatchTx   *common.Hash   `json:"verifyBatchTxHash"`
}

// lastBlockOfBatch returns the number of the newest block in batch n, or in the closest earlier
// batch that has blocks. Results are cached: a virtual batch never changes.
func (c *RPC) lastBlockOfBatch(ctx context.Context, n uint64) (uint64, error) {
	c.mu.Lock()
	cached, ok := c.batchBlocks[n]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	for b := n; b+8 > n && b > 0; b-- {
		var batch *rpcBatch
		if err := c.call(ctx, &batch, "zkevm_getBatchByNumber", hexutil.EncodeUint64(b), false); err != nil {
			return 0, err
		}
		if batch == nil || len(batch.Blocks) == 0 {
			continue
		}
		var h *rpcHeader
		if err := c.call(ctx, &h, "eth_getBlockByHash", batch.Blocks[len(batch.Blocks)-1], false); err != nil {
			return 0, err
		}
		if h == nil {
			return 0, fmt.Errorf("%w: last block of batch %d", ErrNotFound, b)
		}
		c.mu.Lock()
		if len(c.batchBlocks) > 1024 {
			clear(c.batchBlocks)
		}
		c.batchBlocks[n] = uint64(h.Number)
		c.mu.Unlock()
		return uint64(h.Number), nil
	}
	return 0, fmt.Errorf("no blocks in batches up to %d", n)
}

// Evidence returns the batch that contains block n and the L1 transactions that sequenced and
// verified it. It is only available on zkEVM networks.
func (c *RPC) Evidence(ctx context.Context, n uint64) (*Evidence, error) {
	if !c.zkevm {
		return nil, nil
	}
	var bn hexutil.Uint64
	if err := c.call(ctx, &bn, "zkevm_batchNumberByBlockNumber", hexutil.EncodeUint64(n)); err != nil {
		return nil, err
	}
	var batch *rpcBatch
	if err := c.call(ctx, &batch, "zkevm_getBatchByNumber", bn, false); err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, fmt.Errorf("%w: batch %d", ErrNotFound, bn)
	}
	return &Evidence{Batch: uint64(bn), SequenceTxHash: nonZero(batch.SendSequencesTx), VerifyTxHash: nonZero(batch.VerifyBatchTx)}, nil
}

func nonZero(h *common.Hash) *common.Hash {
	if h == nil || *h == (common.Hash{}) {
		return nil
	}
	return h
}

func (c *RPC) Header(ctx context.Context, number uint64) (Header, error) {
	hs, err := c.getBlocks(ctx, "eth_getBlockByNumber", []string{hexutil.EncodeUint64(number)})
	if err != nil {
		return Header{}, err
	}
	return hs[0], nil
}

func (c *RPC) Headers(ctx context.Context, numbers []uint64) ([]Header, error) {
	out := make([]Header, 0, len(numbers))
	for start := 0; start < len(numbers); start += c.batchSize {
		chunk := numbers[start:min(len(numbers), start+c.batchSize)]
		tags := make([]string, len(chunk))
		for i, n := range chunk {
			tags[i] = hexutil.EncodeUint64(n)
		}
		hs, err := c.getBlocks(ctx, "eth_getBlockByNumber", tags)
		if err != nil {
			return nil, err
		}
		out = append(out, hs...)
	}
	return out, nil
}

func (c *RPC) Payments(ctx context.Context, router common.Address, merchants []common.Address, from, to uint64) ([]Payment, error) {
	var merchantTopics []common.Hash
	for _, m := range merchants {
		merchantTopics = append(merchantTopics, common.BytesToHash(m.Bytes()))
	}
	topics := [][]common.Hash{{token.InvoicePaidTopic}}
	if len(merchantTopics) > 0 {
		topics = append(topics, nil, merchantTopics) // merchant is the second indexed argument
	}
	filter := map[string]any{
		"address":   []common.Address{router},
		"topics":    topics,
		"fromBlock": hexutil.EncodeUint64(from),
		"toBlock":   hexutil.EncodeUint64(to),
	}
	logs, err := c.getLogs(ctx, filter, to)
	if err != nil {
		return nil, err
	}
	seen := map[string]Payment{}
	for _, l := range logs {
		if p, ok := l.payment(); ok {
			seen[p.Key()] = p
		}
	}
	return sortPayments(seen), nil
}

func (c *RPC) Mandates(ctx context.Context, router common.Address, from, to uint64) ([]MandateEvent, error) {
	filter := map[string]any{
		"address":   []common.Address{router},
		"topics":    [][]common.Hash{{token.MandateGrantedTopic, token.MandateRevokedTopic}},
		"fromBlock": hexutil.EncodeUint64(from),
		"toBlock":   hexutil.EncodeUint64(to),
	}
	logs, err := c.getLogs(ctx, filter, to)
	if err != nil {
		return nil, err
	}
	seen := map[string]MandateEvent{}
	for _, l := range logs {
		if m, ok := l.mandate(); ok {
			seen[m.Key()] = m
		}
	}
	out := make([]MandateEvent, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockNumber != out[j].BlockNumber {
			return out[i].BlockNumber < out[j].BlockNumber
		}
		return out[i].LogIndex < out[j].LogIndex
	})
	return out, nil
}

// getLogs runs eth_getLogs on an endpoint whose head has reached block to.
func (c *RPC) getLogs(ctx context.Context, filter map[string]any, to uint64) ([]rpcLog, error) {
	var logs []rpcLog
	err := c.do(ctx, "eth_getLogs", 2, func(ctx context.Context, ep *endpoint) error {
		// Many nodes answer a range past their head with the logs they have, silently dropping the rest.
		// Behind a load balancer each request may hit a different node, so check the same endpoint first.
		var head hexutil.Uint64
		if err := ep.rpc.CallContext(ctx, &head, "eth_blockNumber"); err != nil {
			return err
		}
		if uint64(head) < to {
			return fmt.Errorf("%w: node head %d is below requested block %d", ErrNodeBehind, head, to)
		}
		logs = nil
		return ep.rpc.CallContext(ctx, &logs, "eth_getLogs", filter)
	})
	return logs, err
}

// rpcLog decodes only the log fields used here.
type rpcLog struct {
	Topics      []common.Hash  `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	BlockHash   common.Hash    `json:"blockHash"`
	TxHash      common.Hash    `json:"transactionHash"`
	Index       hexutil.Uint   `json:"logIndex"`
	Removed     bool           `json:"removed"`
}

// payment decodes InvoicePaid(bytes32 indexed invoiceId, address indexed merchant,
// address indexed payer, address agent, uint256 amount).
func (l rpcLog) payment() (Payment, bool) {
	if l.Removed || len(l.Topics) != 4 || l.Topics[0] != token.InvoicePaidTopic || len(l.Data) != 64 {
		return Payment{}, false
	}
	return Payment{
		BlockNumber: uint64(l.BlockNumber),
		BlockHash:   l.BlockHash,
		TxHash:      l.TxHash,
		LogIndex:    uint(l.Index),
		InvoiceID:   l.Topics[1],
		Merchant:    common.BytesToAddress(l.Topics[2].Bytes()),
		Payer:       common.BytesToAddress(l.Topics[3].Bytes()),
		Agent:       common.BytesToAddress(l.Data[:32]),
		Amount:      new(big.Int).SetBytes(l.Data[32:]),
	}, true
}

// mandate decodes MandateGranted(address indexed owner, address indexed agent, uint128 perPaymentCap,
// uint128 dailyCap, uint64 expiresAt, address[] merchants) and MandateRevoked(address indexed owner,
// address indexed agent).
func (l rpcLog) mandate() (MandateEvent, bool) {
	if l.Removed || len(l.Topics) != 3 {
		return MandateEvent{}, false
	}
	m := MandateEvent{
		BlockNumber: uint64(l.BlockNumber), BlockHash: l.BlockHash, TxHash: l.TxHash, LogIndex: uint(l.Index),
		Owner: common.BytesToAddress(l.Topics[1].Bytes()), Agent: common.BytesToAddress(l.Topics[2].Bytes()),
	}
	switch l.Topics[0] {
	case token.MandateRevokedTopic:
		m.Revoked = true
		return m, true
	case token.MandateGrantedTopic:
		word := func(i int) []byte { return l.Data[i*32 : (i+1)*32] }
		if len(l.Data) < 5*32 {
			return MandateEvent{}, false
		}
		m.PerPaymentCap = new(big.Int).SetBytes(word(0))
		m.DailyCap = new(big.Int).SetBytes(word(1))
		m.ExpiresAt = new(big.Int).SetBytes(word(2)).Uint64()
		offset := new(big.Int).SetBytes(word(3))
		if !offset.IsUint64() || offset.Uint64() != 4*32 {
			return MandateEvent{}, false
		}
		n := new(big.Int).SetBytes(word(4))
		if !n.IsUint64() || n.Uint64() > 1024 || len(l.Data) < (5+int(n.Uint64()))*32 {
			return MandateEvent{}, false
		}
		for i := 0; i < int(n.Uint64()); i++ {
			m.Merchants = append(m.Merchants, common.BytesToAddress(word(5+i)))
		}
		return m, true
	}
	return MandateEvent{}, false
}

func sortPayments(m map[string]Payment) []Payment {
	out := make([]Payment, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockNumber != out[j].BlockNumber {
			return out[i].BlockNumber < out[j].BlockNumber
		}
		return out[i].LogIndex < out[j].LogIndex
	})
	return out
}

// IsRangeError reports whether a node rejected a log query for covering too many blocks or results.
// Providers word this differently, so it matches on the common phrasings. Rate limits are not range errors.
func IsRangeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRangeTooLarge) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") {
		return false
	}
	for _, s := range []string{"query returned more than", "too many results", "too many logs", "response size", "exceeds max results"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	// QDay: "block range too large, max range: 1000"; others: "limited to a 200 range", ...
	if strings.Contains(msg, "range") {
		for _, s := range []string{"limit", "exceed", "too large", "too wide", "too big", "max", "up to"} {
			if strings.Contains(msg, s) {
				return true
			}
		}
	}
	return false
}

// backoff is exponential with full jitter: a random delay in [0, min(8s, 250ms*2^attempt)].
func backoff(attempt int) time.Duration {
	d := min(8*time.Second, 250*time.Millisecond<<attempt)
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// redactURL keeps only the scheme and host, dropping paths and query strings that may hold API keys.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<rpc>"
	}
	return u.Scheme + "://" + u.Host
}

// scrub removes a full endpoint URL from an error message (net/http errors include it).
func scrub(err error, raw string) error {
	if err == nil || !strings.Contains(err.Error(), raw) {
		return err
	}
	return &scrubbedError{msg: strings.ReplaceAll(err.Error(), raw, redactURL(raw)), err: err}
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }
