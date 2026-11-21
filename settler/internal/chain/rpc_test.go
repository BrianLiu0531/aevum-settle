package chain

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/token"
)

func TestDecodesMandateEvents(t *testing.T) {
	u128, _ := abi.NewType("uint128", "", nil)
	u64, _ := abi.NewType("uint64", "", nil)
	addrs, _ := abi.NewType("address[]", "", nil)
	args := abi.Arguments{{Type: u128}, {Type: u128}, {Type: u64}, {Type: addrs}}
	shop, cafe := common.HexToAddress("0x5b0b"), common.HexToAddress("0xcafe")
	data, err := args.Pack(big.NewInt(150_000_000), big.NewInt(200_000_000), uint64(1_900_000_000), []common.Address{shop, cafe})
	if err != nil {
		t.Fatal(err)
	}
	owner, agent := common.HexToAddress("0xa11ce"), common.HexToAddress("0xa6e47")
	topics := []common.Hash{token.MandateGrantedTopic, common.BytesToHash(owner.Bytes()), common.BytesToHash(agent.Bytes())}

	m, ok := rpcLog{Topics: topics, Data: data, BlockNumber: 7, Index: 2}.mandate()
	if !ok || m.Revoked || m.Owner != owner || m.Agent != agent || m.BlockNumber != 7 || m.LogIndex != 2 {
		t.Fatalf("decoded %+v (ok=%v)", m, ok)
	}
	if m.PerPaymentCap.Int64() != 150_000_000 || m.DailyCap.Int64() != 200_000_000 || m.ExpiresAt != 1_900_000_000 {
		t.Fatalf("caps %+v", m)
	}
	if len(m.Merchants) != 2 || m.Merchants[0] != shop || m.Merchants[1] != cafe {
		t.Fatalf("merchants %v", m.Merchants)
	}

	// No merchants (any merchant allowed)
	empty, _ := args.Pack(big.NewInt(1), big.NewInt(1), uint64(1), []common.Address{})
	if m, ok := (rpcLog{Topics: topics, Data: empty}).mandate(); !ok || len(m.Merchants) != 0 {
		t.Fatalf("empty allowlist: %+v %v", m, ok)
	}

	// Revoke carries no data
	rev := []common.Hash{token.MandateRevokedTopic, topics[1], topics[2]}
	if m, ok := (rpcLog{Topics: rev}).mandate(); !ok || !m.Revoked || m.Agent != agent {
		t.Fatalf("revoke: %+v %v", m, ok)
	}

	// Truncated or removed logs are ignored, never half-decoded
	if _, ok := (rpcLog{Topics: topics, Data: data[:5*32]}).mandate(); ok {
		t.Fatal("accepted a truncated merchant list")
	}
	if _, ok := (rpcLog{Topics: topics, Data: data, Removed: true}).mandate(); ok {
		t.Fatal("accepted a removed log")
	}
	if _, ok := (rpcLog{Topics: []common.Hash{token.InvoicePaidTopic, topics[1], topics[2]}, Data: data}).mandate(); ok {
		t.Fatal("decoded an InvoicePaid log as a mandate")
	}
}
