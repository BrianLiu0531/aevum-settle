// Package chain defines what the settler needs from a node, and a JSON-RPC implementation of it.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Header is the part of a block header the settler stores to detect reorgs.
type Header struct {
	Number     uint64
	Hash       common.Hash
	ParentHash common.Hash
	Time       uint64
}

// Tips are the chain's current heights at each level of assurance.
//
// QDay Aevum is a Polygon CDK zkEVM chain (validium). A block is:
//   - trusted (Latest) as soon as the sequencer produces it, about a second after submission;
//   - virtual once its batch has been sequenced on L1, about ten minutes later;
//   - verified (Finalized) once a zero-knowledge proof of the batch has been verified on L1,
//     after roughly an hour and a half. A verified block cannot be reorged.
type Tips struct {
	Latest    uint64
	Virtual   uint64
	Finalized uint64
}

// Payment is one decoded AgentPayRouter.InvoicePaid log.
type Payment struct {
	BlockNumber uint64
	BlockHash   common.Hash
	TxHash      common.Hash
	LogIndex    uint
	InvoiceID   common.Hash
	Merchant    common.Address
	Payer       common.Address
	Agent       common.Address // zero for a payment the payer made directly
	Amount      *big.Int
}

// Key identifies a log independently of the block it landed in.
// After a reorg the same transaction can be re-mined in a different block, keeping this key.
func (p Payment) Key() string { return fmt.Sprintf("%s:%d", p.TxHash.Hex(), p.LogIndex) }

// Evidence links a block to the L1 transactions that sequenced and verified its batch.
type Evidence struct {
	Batch          uint64       `json:"batch"`
	SequenceTxHash *common.Hash `json:"l1SequenceTx"` // nil until the batch is virtual
	VerifyTxHash   *common.Hash `json:"l1VerifyTx"`   // nil until the batch is verified
}

// Client is the read-only view of the chain used by the indexer and API.
type Client interface {
	ChainID(ctx context.Context) (uint64, error)
	Tips(ctx context.Context) (Tips, error)
	Header(ctx context.Context, number uint64) (Header, error)
	// Headers returns the headers for the given block numbers, in the same order.
	Headers(ctx context.Context, numbers []uint64) ([]Header, error)
	// Payments returns InvoicePaid logs of router in from..to, ordered by block and log index.
	// A non-empty merchants list keeps only payments to those merchants.
	Payments(ctx context.Context, router common.Address, merchants []common.Address, from, to uint64) ([]Payment, error)
}

// ErrRangeTooLarge means the node refused a log query because the block range or result set was too big.
// The caller should retry with a smaller range.
var ErrRangeTooLarge = errors.New("log query range too large")

// ErrNodeBehind means the node answering a request has not reached the requested block yet.
var ErrNodeBehind = errors.New("node is behind")

// ErrNotFound means the requested block does not exist on the node's current chain.
var ErrNotFound = errors.New("block not found")

// Range returns the block numbers from..to inclusive.
func Range(from, to uint64) []uint64 {
	if to < from {
		return nil
	}
	out := make([]uint64, 0, to-from+1)
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}
