package indexer

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain/chaintest"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store/storetest"
)

func grant(owner, agent common.Address, per, daily int64, merchants ...common.Address) chain.MandateEvent {
	return chain.MandateEvent{Owner: owner, Agent: agent, PerPaymentCap: big.NewInt(per), DailyCap: big.NewInt(daily), ExpiresAt: 1_900_000_000, Merchants: merchants}
}

func revoke(owner, agent common.Address) chain.MandateEvent {
	return chain.MandateEvent{Owner: owner, Agent: agent, Revoked: true}
}

func TestIndexesMandateAuditTrail(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 7)
	ix := newIndexer(f, s, withPolicy(mustPolicy("trusted")))

	f.MineMandates(grant(alice, robot, 150, 200, shop))
	f.Mine(chaintest.Spec{Merchant: shop, Payer: alice, Agent: robot, Amount: 20})
	f.Mine(chaintest.Spec{Merchant: shop, Payer: alice, Agent: robot, Amount: 5})
	f.MineMandates(grant(bob, robot, 10, 10))
	f.SetAssurance(4, 4)
	f.MineMandates(revoke(alice, robot))
	sync(t, ix)

	events, err := s.ListMandateEvents(ctx, nil, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != "revoked" || events[2].Kind != "granted" {
		t.Fatalf("events: %+v", events)
	}
	if g := events[2]; g.PerPaymentCap.Int64() != 150 || g.DailyCap.Int64() != 200 || len(g.Merchants) != 1 || g.Merchants[0] != addrString(shop) {
		t.Fatalf("grant decoded wrong: %+v", g)
	}

	sums, err := s.MandateSummaries(ctx, &alice, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].Latest.Kind != "revoked" || sums[0].Payments != 2 || sums[0].Spent.Int64() != 25 || sums[0].Grants != 1 {
		t.Fatalf("alice/robot summary: %+v", sums)
	}

	// A reorg removes the revoke: the audit trail must show the mandate as granted again
	f.Reorg(1)
	f.MineEmpty(2)
	sync(t, ix)
	sums, err = s.MandateSummaries(ctx, &alice, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].Latest.Kind != "granted" {
		t.Fatalf("after reorg: %+v", sums)
	}
	if all, _ := s.ListMandateEvents(ctx, nil, nil, 50); len(all) != len(f.CanonicalMandates()) {
		t.Fatalf("stored %d mandate events, chain has %d", len(all), len(f.CanonicalMandates()))
	}
}

func addrString(a common.Address) string { return strings.ToLower(a.Hex()) }
