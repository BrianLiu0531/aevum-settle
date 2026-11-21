// Package chaintest provides an in-memory chain that can mine blocks, reorg and fail on demand.
package chaintest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
)

// Spec describes a payment to include in a mined block. Zero TxHash and InvoiceID get unique values.
type Spec struct {
	Merchant, Payer, Agent common.Address
	Amount                 int64
	InvoiceID              common.Hash
	TxHash                 common.Hash
}

type block struct {
	header   chain.Header
	payments []chain.Payment
}

// Fake is a chain.Client backed by memory. It is safe for concurrent use.
type Fake struct {
	mu        sync.Mutex
	chainID   uint64
	blocks    []block // canonical chain; index == block number
	virtual   uint64
	finalized uint64
	fork      uint64 // changes the hashes of blocks mined after a reorg
	seq       uint64

	rng      *rand.Rand
	failRate float64

	// MaxLogRange, when set, makes Payments reject ranges longer than this many blocks.
	MaxLogRange uint64
	// BeforePayments, when set, runs before each Payments call (outside the lock), e.g. to reorg mid-fetch.
	BeforePayments func()
}

// New returns a chain containing only the genesis block.
func New(chainID uint64, seed uint64) *Fake {
	f := &Fake{chainID: chainID, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	f.blocks = []block{{header: chain.Header{Number: 0, Hash: crypto.Keccak256Hash([]byte("genesis"))}}}
	return f
}

// SetFailRate makes each call fail with the given probability.
func (f *Fake) SetFailRate(p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failRate = p
}

// ErrInjected is returned by calls that SetFailRate made fail.
var ErrInjected = errors.New("injected RPC failure")

func (f *Fake) maybeFail() error {
	if f.failRate > 0 && f.rng.Float64() < f.failRate {
		return ErrInjected
	}
	return nil
}

func (f *Fake) unique(prefix string) common.Hash {
	f.seq++
	return crypto.Keccak256Hash([]byte(fmt.Sprintf("%s-%d", prefix, f.seq)))
}

// Mine appends one block containing the given payments and returns its number.
func (f *Fake) Mine(specs ...Spec) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.blocks[len(f.blocks)-1].header
	n := parent.Number + 1
	buf := make([]byte, 24)
	binary.BigEndian.PutUint64(buf, n)
	binary.BigEndian.PutUint64(buf[8:], f.fork)
	binary.BigEndian.PutUint64(buf[16:], uint64(len(specs)))
	h := chain.Header{Number: n, ParentHash: parent.Hash, Hash: crypto.Keccak256Hash(parent.Hash.Bytes(), buf), Time: 1_700_000_000 + n}
	b := block{header: h}
	for i, s := range specs {
		tx, inv := s.TxHash, s.InvoiceID
		if tx == (common.Hash{}) {
			tx = f.unique("tx")
		}
		if inv == (common.Hash{}) {
			inv = f.unique("invoice")
		}
		b.payments = append(b.payments, chain.Payment{
			BlockNumber: n, BlockHash: h.Hash, TxHash: tx, LogIndex: uint(i),
			InvoiceID: inv, Merchant: s.Merchant, Payer: s.Payer, Agent: s.Agent, Amount: big.NewInt(s.Amount),
		})
	}
	f.blocks = append(f.blocks, b)
	return n
}

// MineEmpty mines n blocks without payments.
func (f *Fake) MineEmpty(n int) {
	for range n {
		f.Mine()
	}
}

// Reorg removes the newest depth blocks; mine new ones afterwards to build the competing fork.
// It returns the payments that were removed, so tests can re-include some of them.
func (f *Fake) Reorg(depth int) []chain.Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	head := uint64(len(f.blocks) - 1)
	if uint64(depth) > head || head-uint64(depth) < f.finalized {
		panic(fmt.Sprintf("reorg of %d blocks would remove verified block %d", depth, f.finalized))
	}
	var removed []chain.Payment
	for _, b := range f.blocks[len(f.blocks)-depth:] {
		removed = append(removed, b.payments...)
	}
	f.blocks = f.blocks[:len(f.blocks)-depth]
	f.fork++
	f.virtual = min(f.virtual, head-uint64(depth))
	return removed
}

// SetAssurance moves the virtual and verified heights (they never go past the head).
func (f *Fake) SetAssurance(virtual, verified uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	head := uint64(len(f.blocks) - 1)
	f.virtual, f.finalized = min(virtual, head), min(verified, head)
}

// Head returns the latest block number.
func (f *Fake) Head() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.blocks) - 1)
}

// Finalized returns the verified height.
func (f *Fake) Finalized() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finalized
}

// Canonical returns every payment on the canonical chain (to watched merchants, if any are given).
func (f *Fake) Canonical(merchants []common.Address) []chain.Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.collect(merchants, 0, uint64(len(f.blocks)-1))
}

func (f *Fake) collect(merchants []common.Address, from, to uint64) []chain.Payment {
	w := map[common.Address]bool{}
	for _, a := range merchants {
		w[a] = true
	}
	var out []chain.Payment
	for n := from; n <= to && n < uint64(len(f.blocks)); n++ {
		for _, p := range f.blocks[n].payments {
			if len(w) == 0 || w[p.Merchant] {
				out = append(out, p)
			}
		}
	}
	return out
}

func (f *Fake) ChainID(context.Context) (uint64, error) { return f.chainID, nil }

func (f *Fake) Tips(context.Context) (chain.Tips, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.maybeFail(); err != nil {
		return chain.Tips{}, err
	}
	return chain.Tips{Latest: uint64(len(f.blocks) - 1), Virtual: f.virtual, Finalized: f.finalized}, nil
}

func (f *Fake) Header(ctx context.Context, n uint64) (chain.Header, error) {
	hs, err := f.Headers(ctx, []uint64{n})
	if err != nil {
		return chain.Header{}, err
	}
	return hs[0], nil
}

func (f *Fake) Headers(_ context.Context, numbers []uint64) ([]chain.Header, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	out := make([]chain.Header, len(numbers))
	for i, n := range numbers {
		if n >= uint64(len(f.blocks)) {
			return nil, fmt.Errorf("%w: %d", chain.ErrNotFound, n)
		}
		out[i] = f.blocks[n].header
	}
	return out, nil
}

func (f *Fake) Payments(_ context.Context, _ common.Address, merchants []common.Address, from, to uint64) ([]chain.Payment, error) {
	if f.BeforePayments != nil {
		f.BeforePayments()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	if f.MaxLogRange > 0 && to-from+1 > f.MaxLogRange {
		return nil, fmt.Errorf("%w: requested %d blocks", chain.ErrRangeTooLarge, to-from+1)
	}
	return f.collect(merchants, from, to), nil
}
