package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain/chaintest"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store/storetest"
)

const chainID = 44005

var (
	router  = common.HexToAddress("0x00000000000000000000000000000000000c0de1")
	shop    = common.HexToAddress("0x0000000000000000000000000000000000005b0b") // merchant
	cafe    = common.HexToAddress("0x000000000000000000000000000000000000cafe") // merchant
	alice   = common.HexToAddress("0x00000000000000000000000000000000000a11ce") // payer
	bob     = common.HexToAddress("0x0000000000000000000000000000000000000b0b") // payer
	robot   = common.HexToAddress("0x00000000000000000000000000000000000a6e47") // agent
	payers  = []common.Address{alice, bob}
	shops   = []common.Address{shop, cafe}
	agents  = []common.Address{{}, robot}
	quiet   = slog.New(slog.NewTextHandler(io.Discard, nil))
	onlyFin = mustPolicy("verified")
)

func mustPolicy(s string) store.Policy {
	p, err := store.ParsePolicy(s)
	if err != nil {
		panic(err)
	}
	return p
}

func newIndexer(f chain.Client, s *store.Store, mutate ...func(*Config)) *Indexer {
	cfg := Config{Router: router, StartBlock: 1, MaxRange: 50, MaxUnfinalizedBatch: 8, Policy: onlyFin}
	for _, m := range mutate {
		m(&cfg)
	}
	return New(cfg, f, s, quiet)
}

func withPolicy(p store.Policy) func(*Config) { return func(c *Config) { c.Policy = p } }

func pay(merchant, payer common.Address, amount int64) chaintest.Spec {
	return chaintest.Spec{Merchant: merchant, Payer: payer, Amount: amount}
}

// sync steps until the indexer is caught up, tolerating the transient errors a real node produces.
func sync(t *testing.T, ix *Indexer) {
	t.Helper()
	for range 2000 {
		res, err := ix.Step(context.Background())
		switch {
		case err == nil && res.CaughtUp:
			return
		case err == nil, errors.Is(err, chaintest.ErrInjected), errors.Is(err, errChainMoved):
		default:
			t.Fatalf("step: %v", err)
		}
	}
	t.Fatal("indexer did not catch up")
}

func eventCounts(t *testing.T, s *store.Store) map[string]int {
	t.Helper()
	events, err := s.OutboxEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range events {
		out[e.Type]++
	}
	return out
}

// assertMatchesChain checks the database holds exactly the canonical payments, with the right
// assurance, and that replaying the outbox credits each confirmed payment exactly once.
func assertMatchesChain(t *testing.T, s *store.Store, f *chaintest.Fake, start uint64, policy store.Policy) {
	t.Helper()
	ctx := context.Background()
	tips, _ := f.Tips(ctx)

	var want []chain.Payment
	for _, p := range f.Canonical(nil) {
		if p.BlockNumber >= start {
			want = append(want, p)
		}
	}
	got, err := s.AllPayments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d payments, chain has %d", len(got), len(want))
	}
	agentOf := func(a common.Address) string {
		if a == (common.Address{}) {
			return ""
		}
		return strings.ToLower(a.Hex())
	}
	for i, w := range want {
		g := got[i]
		if g.TxHash != w.TxHash.Hex() || g.LogIndex != int(w.LogIndex) || g.BlockHash != w.BlockHash.Hex() ||
			g.Amount.Cmp(w.Amount) != 0 || g.InvoiceID != w.InvoiceID.Hex() || g.Merchant != strings.ToLower(w.Merchant.Hex()) ||
			g.Payer != strings.ToLower(w.Payer.Hex()) || g.Agent != agentOf(w.Agent) {
			t.Fatalf("payment %d: got %+v, want %+v", i, g, w)
		}
		wantStatus := store.Trusted
		switch {
		case w.BlockNumber <= tips.Finalized:
			wantStatus = store.Verified
		case w.BlockNumber <= tips.Virtual:
			wantStatus = store.Virtual
		}
		if g.Status != wantStatus {
			t.Fatalf("payment in block %d: status %v, want %v", w.BlockNumber, g.Status, wantStatus)
		}
		if g.Required != policy.Required(w.Amount) {
			t.Fatalf("payment of %s: required %v, want %v", w.Amount, g.Required, policy.Required(w.Amount))
		}
		if (g.ConfirmedAt != nil) != (wantStatus >= g.Required) {
			t.Fatalf("payment in block %d: confirmed=%v at status %v, required %v", w.BlockNumber, g.ConfirmedAt != nil, g.Status, g.Required)
		}
		if (g.VerifiedAt != nil) != (wantStatus == store.Verified) {
			t.Fatalf("payment in block %d: verified=%v at status %v", w.BlockNumber, g.VerifiedAt != nil, g.Status)
		}
	}

	// Replaying the outbox must leave exactly the confirmed canonical payments credited, and each
	// payment confirmed early must be followed by one payment.verified once it is final.
	events, err := s.OutboxEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credited, verified := map[string]int{}, map[string]int{}
	ids := map[string]bool{}
	for _, e := range events {
		if ids[e.EventID] {
			t.Fatalf("duplicate event id %s", e.EventID)
		}
		ids[e.EventID] = true
		var p struct {
			TxHash    string `json:"txHash"`
			LogIndex  int    `json:"logIndex"`
			BlockHash string `json:"blockHash"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("%s:%d:%s", p.TxHash, p.LogIndex, p.BlockHash)
		switch e.Type {
		case "payment.confirmed":
			credited[key]++
		case "payment.reverted":
			credited[key]--
		case "payment.verified":
			verified[key]++
		}
	}
	for _, g := range got {
		key := fmt.Sprintf("%s:%d:%s", g.TxHash, g.LogIndex, g.BlockHash)
		want := 0
		if g.ConfirmedAt != nil {
			want = 1
		}
		if credited[key] != want {
			t.Fatalf("payment %s credited %d times, want %d", key, credited[key], want)
		}
		wantVerified := 0
		if g.VerifiedAt != nil && g.Required < store.Verified {
			wantVerified = 1
		}
		if verified[key] != wantVerified {
			t.Fatalf("payment %s has %d verified events, want %d", key, verified[key], wantVerified)
		}
		delete(credited, key)
		delete(verified, key)
	}
	for key, n := range credited {
		if n != 0 {
			t.Fatalf("orphaned payment %s is still credited %d times", key, n)
		}
	}
	for key := range verified {
		t.Fatalf("orphaned payment %s was announced as verified", key)
	}
}

func TestIndexesEachPaymentOnce(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	f.Mine(pay(shop, alice, 100))
	f.Mine(pay(cafe, bob, 30), chaintest.Spec{Merchant: shop, Payer: alice, Agent: robot, Amount: 999})
	f.MineEmpty(5)
	f.SetAssurance(6, 4)

	sync(t, newIndexer(f, s))
	assertMatchesChain(t, s, f, 1, onlyFin)
	if got, _ := s.AllPayments(context.Background()); len(got) != 3 {
		t.Fatalf("got %d payments, want 3", len(got))
	}

	// A restarted indexer resumes from the cursor and writes nothing twice
	sync(t, newIndexer(f, s))
	assertMatchesChain(t, s, f, 1, onlyFin)
}

func TestFiltersByMerchant(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	f.Mine(pay(shop, alice, 1), pay(cafe, alice, 2))
	sync(t, newIndexer(f, s, func(c *Config) { c.Merchants = []common.Address{cafe} }))
	got, _ := s.AllPayments(context.Background())
	if len(got) != 1 || got[0].Merchant != strings.ToLower(cafe.Hex()) {
		t.Fatalf("got %+v, want only the cafe payment", got)
	}
}

func TestRejectsStaleBatches(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	f.Mine(pay(shop, alice, 100))
	f.MineEmpty(3)
	sync(t, newIndexer(f, s))

	// A second writer that read an older cursor must not be able to commit
	stale := &chain.Header{Number: 1, Hash: common.HexToHash("0x01")}
	_, err := s.Apply(ctx, store.Batch{Parent: stale, Blocks: []chain.Header{{Number: 2}}})
	if !errors.Is(err, store.ErrCursorMoved) {
		t.Fatalf("got %v, want ErrCursorMoved", err)
	}
	assertMatchesChain(t, s, f, 1, onlyFin)
}

func TestTieredConfirmation(t *testing.T) {
	// up to 1 tUSD8 on the sequencer's word, up to 50 once on L1, anything larger once proven
	policy := mustPolicy("trusted<=1,virtual<=50,verified")
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	ix := newIndexer(f, s, withPolicy(policy))
	f.Mine(pay(shop, alice, 500_000), pay(shop, bob, 1_000_000)) // 0.5 and 1.0: trusted
	f.Mine(pay(shop, alice, 20_000_000))                         // 20: virtual
	f.Mine(pay(shop, bob, 75_000_000))                           // 75: verified
	f.MineEmpty(3)

	sync(t, ix)
	assertMatchesChain(t, s, f, 1, policy)
	if c := eventCounts(t, s); c["payment.confirmed"] != 2 || c["payment.verified"] != 0 {
		t.Fatalf("after trusted: %v", c)
	}

	f.SetAssurance(3, 0)
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, policy)
	if c := eventCounts(t, s); c["payment.confirmed"] != 3 { // the 75 tUSD8 payment still waits for the proof
		t.Fatalf("after virtual: %v", c)
	}

	f.SetAssurance(6, 6)
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, policy)
	// the three early confirmations are each followed by payment.verified; the large one needs none
	if c := eventCounts(t, s); c["payment.confirmed"] != 4 || c["payment.verified"] != 3 {
		t.Fatalf("after verified: %v", c)
	}
}

func TestReorgRevertsEarlyConfirmations(t *testing.T) {
	policy := mustPolicy("trusted")
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	ix := newIndexer(f, s, withPolicy(policy))

	for i := range 6 {
		f.Mine(pay(shop, alice, int64(10+i)))
	}
	f.SetAssurance(3, 2)
	sync(t, ix)

	// Blocks 5 and 6 are replaced; the payment from block 6 is re-mined in the new fork
	removed := f.Reorg(2)
	f.MineEmpty(1)
	r := removed[1]
	f.Mine(chaintest.Spec{Merchant: r.Merchant, Payer: r.Payer, Amount: r.Amount.Int64(), InvoiceID: r.InvoiceID, TxHash: r.TxHash})
	f.Mine(pay(cafe, bob, 3))

	sync(t, ix)
	assertMatchesChain(t, s, f, 1, policy)
	// 6 original + 2 new confirmations; 2 reverted
	if c := eventCounts(t, s); c["payment.confirmed"] != 8 || c["payment.reverted"] != 2 {
		t.Fatalf("events by type: %v", c)
	}
}

func TestDetectsReorgWithoutNewBlocks(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	ix := newIndexer(f, s)
	f.MineEmpty(3)
	f.Mine(pay(shop, alice, 1))
	sync(t, ix)

	// Same height after the reorg, then a shorter chain
	f.Reorg(1)
	f.Mine(pay(shop, bob, 2))
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, onlyFin)

	// A shorter chain looks exactly like a node that is behind, so nothing is rewound yet...
	cur, _ := s.Cursor(context.Background())
	f.Reorg(2)
	f.MineEmpty(1)
	sync(t, ix)
	if after, _ := s.Cursor(context.Background()); after.Hash != cur.Hash {
		t.Fatal("rewound without evidence of a reorg")
	}
	// ...until the chain grows past the cursor and the parent hash proves the fork
	f.MineEmpty(2)
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, onlyFin)
}

// lagging is a node that has only reached block head, as served by a load balancer or a failover endpoint.
type lagging struct {
	*chaintest.Fake
	head uint64
}

func (l *lagging) Tips(ctx context.Context) (chain.Tips, error) {
	tips, err := l.Fake.Tips(ctx)
	return chain.Tips{Latest: min(tips.Latest, l.head), Virtual: min(tips.Virtual, l.head), Finalized: min(tips.Finalized, l.head)}, err
}

func (l *lagging) Header(ctx context.Context, n uint64) (chain.Header, error) {
	if n > l.head {
		return chain.Header{}, chain.ErrNotFound
	}
	return l.Fake.Header(ctx, n)
}

func TestWaitsForALaggingNode(t *testing.T) {
	ctx := context.Background()
	policy := mustPolicy("trusted")
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	for range 10 {
		f.Mine(pay(shop, alice, 1))
	}
	sync(t, newIndexer(f, s, withPolicy(policy)))

	// Fail over to a node 4 blocks behind: the indexer must wait, not treat the missing blocks as a reorg
	slow := newIndexer(&lagging{Fake: f, head: 6}, s, withPolicy(policy))
	for range 5 {
		res, err := slow.Step(ctx)
		if err != nil || !res.CaughtUp || res.Reorg {
			t.Fatalf("step on lagging node: %+v, %v", res, err)
		}
	}
	if eventCounts(t, s)["payment.reverted"] != 0 {
		t.Fatal("a lagging node caused a revert")
	}
	assertMatchesChain(t, s, f, 1, policy)
}

// TestConcurrentIndexersNeverDuplicate runs two indexers against one database at the same time.
func TestConcurrentIndexersNeverDuplicate(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	for i := range 120 {
		f.Mine(pay(shop, alice, int64(i+1)), pay(cafe, bob, 1))
	}
	f.SetAssurance(115, 100)

	errs := make(chan error, 2)
	for range 2 {
		ix := newIndexer(f, s, func(c *Config) { c.MaxRange = 7 })
		go func() {
			for range 5000 {
				res, err := ix.Step(context.Background())
				if err != nil && !errors.Is(err, store.ErrCursorMoved) {
					errs <- err
					return
				}
				if err == nil && res.CaughtUp {
					errs <- nil
					return
				}
			}
			errs <- errors.New("did not catch up")
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	assertMatchesChain(t, s, f, 1, onlyFin)
}

func TestSurvivesReorgDuringFetch(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	ix := newIndexer(f, s)
	for range 5 {
		f.Mine(pay(shop, alice, 7))
	}
	fired := false
	f.BeforePayments = func() {
		if !fired {
			fired = true
			f.Reorg(3)
			f.Mine(pay(cafe, bob, 8))
			f.MineEmpty(3)
		}
	}
	_, err := ix.Step(context.Background())
	if !errors.Is(err, errChainMoved) {
		t.Fatalf("first step: got %v, want errChainMoved", err)
	}
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, onlyFin)
}

func TestShrinksTheLogRangeWhenRejected(t *testing.T) {
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	f.MaxLogRange = 7
	for i := range 120 {
		if i%9 == 0 {
			f.Mine(pay(shop, alice, int64(i+1)))
		} else {
			f.Mine()
		}
	}
	f.SetAssurance(115, 110)
	ix := newIndexer(f, s, func(c *Config) { c.MaxRange = 100 })
	sync(t, ix)
	assertMatchesChain(t, s, f, 1, onlyFin)
	if ix.span > 7 {
		t.Fatalf("span %d exceeds the node limit", ix.span)
	}
}

func TestRefusesToRewindVerifiedData(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t, chainID)
	f := chaintest.New(chainID, 1)
	f.Mine(pay(shop, alice, 1))
	f.MineEmpty(4)
	f.SetAssurance(5, 5)
	sync(t, newIndexer(f, s))

	// A node on a different chain disagrees with blocks we already treat as final
	other := chaintest.New(chainID, 2)
	other.Mine(pay(cafe, bob, 1), pay(cafe, bob, 2))
	other.MineEmpty(6)
	other.SetAssurance(7, 7)
	_, err := newIndexer(other, s).Step(ctx)
	if !errors.Is(err, store.ErrBelowFinalized) {
		t.Fatalf("got %v, want ErrBelowFinalized", err)
	}
	assertMatchesChain(t, s, f, 1, onlyFin)
}

// TestRandomisedChainHistory mines, reorgs, advances assurance, fails RPC calls and restarts the
// indexer at random, and checks that after every round the database matches the canonical chain.
func TestRandomisedChainHistory(t *testing.T) {
	seeds := 12
	if testing.Short() {
		seeds = 3
	}
	if n, err := strconv.Atoi(os.Getenv("SOAK_SEEDS")); err == nil {
		seeds = n // e.g. SOAK_SEEDS=500 go test -run Randomised ./internal/indexer
	}
	policies := []store.Policy{
		mustPolicy("trusted"), mustPolicy("virtual"), mustPolicy("verified"),
		mustPolicy("trusted<=0.0002,virtual<=0.0006,verified"), // raw amounts below are 1..1000
		mustPolicy("trusted<=0.0005,verified"),
	}
	for seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(seed), 44005))
			s := storetest.Open(t, chainID)
			f := chaintest.New(chainID, uint64(seed))
			f.MaxLogRange = uint64(3 + rng.IntN(20))
			policy := policies[rng.IntN(len(policies))]
			newIx := func() *Indexer { return newIndexer(f, s, withPolicy(policy)) }
			ix := newIx()
			var virtual, verified uint64

			for round := range 40 {
				switch op := rng.IntN(10); {
				case op < 5: // mine a few blocks
					for range 1 + rng.IntN(4) {
						var specs []chaintest.Spec
						for range rng.IntN(3) {
							specs = append(specs, chaintest.Spec{
								Merchant: shops[rng.IntN(2)], Payer: payers[rng.IntN(2)], Agent: agents[rng.IntN(2)],
								Amount: int64(1 + rng.IntN(1000)),
							})
						}
						f.Mine(specs...)
					}
				case op < 7: // reorg, sometimes re-including a removed payment
					if depth := min(int(f.Head()-f.Finalized()), 1+rng.IntN(6)); depth > 0 {
						removed := f.Reorg(depth)
						for range depth + rng.IntN(3) {
							if len(removed) > 0 && rng.IntN(2) == 0 {
								// A transaction can be mined at most once on the new fork
								i := rng.IntN(len(removed))
								r := removed[i]
								removed = append(removed[:i], removed[i+1:]...)
								f.Mine(chaintest.Spec{Merchant: r.Merchant, Payer: r.Payer, Agent: r.Agent, Amount: r.Amount.Int64(), InvoiceID: r.InvoiceID, TxHash: r.TxHash})
							} else {
								f.MineEmpty(1)
							}
						}
					}
				case op < 9: // assurance advances
					head := f.Head()
					virtual = max(virtual, head-min(head, uint64(rng.IntN(4))))
					verified = max(verified, virtual-min(virtual, uint64(rng.IntN(6))))
					f.SetAssurance(virtual, verified)
				default: // crash and restart
					ix = newIx()
				}
				f.SetFailRate([]float64{0, 0, 0.1, 0.3}[rng.IntN(4)])
				for range rng.IntN(4) {
					_, _ = ix.Step(context.Background())
				}
				if round%10 == 9 {
					f.SetFailRate(0)
					sync(t, ix)
					assertMatchesChain(t, s, f, 1, policy)
				}
			}
			f.SetFailRate(0)
			sync(t, ix)
			assertMatchesChain(t, s, f, 1, policy)
		})
	}
}
