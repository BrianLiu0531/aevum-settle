// Package api serves the settler's read-only HTTP API and dashboard.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/outbox"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/token"
)

//go:embed dashboard.html
var dashboard []byte

// EvidenceSource looks up the L1 transactions behind a block (nil on chains without them).
type EvidenceSource interface {
	Evidence(ctx context.Context, block uint64) (*chain.Evidence, error)
}

type Server struct {
	Store    *store.Store
	Network  token.Network
	Router   common.Address
	Policy   store.Policy
	Evidence EvidenceSource
	Metrics  prometheus.Gatherer
	Log      *slog.Logger

	mu    sync.Mutex
	cache map[uint64]*chain.Evidence // only complete (verified) evidence is cached
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboard)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/payments", s.payments)
	mux.HandleFunc("GET /v1/invoices/{merchant}/{invoice}", s.invoice)
	mux.HandleFunc("GET /v1/evidence/{block}", s.evidence)
	mux.HandleFunc("GET /v1/events", s.events)
	if s.Metrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics, promhttp.HandlerOpts{}))
	}
	return cors(mux)
}

// cors lets browser apps (the demo merchant and agent console) read the API. It is read-only.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

type amount struct {
	Raw     string `json:"raw"`     // base units (6 decimals)
	Display string `json:"display"` // e.g. "1.25"
}

func toAmount(v *big.Int) amount { return amount{Raw: v.String(), Display: token.FormatAmount(v)} }

type statusResponse struct {
	Network       string `json:"network"`
	ChainID       uint64 `json:"chainId"`
	Token         string `json:"token"`
	Router        string `json:"router"`
	Explorer      string `json:"explorer"`
	Policy        string `json:"policy"`
	TrustedBlock  uint64 `json:"trustedBlock"`
	VirtualBlock  uint64 `json:"virtualBlock"`
	VerifiedBlock uint64 `json:"verifiedBlock"`
	IndexedBlock  uint64 `json:"indexedBlock"`
	LagBlocks     uint64 `json:"lagBlocks"`
	Totals        struct {
		Payments  int    `json:"payments"`
		Confirmed int    `json:"confirmed"`
		Verified  int    `json:"verified"`
		Orphaned  int    `json:"orphaned"`
		Volume    amount `json:"volume"`
	} `json:"totals"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := s.Store.Cursor(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.Store.State(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	totals, err := s.Store.Totals(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := statusResponse{
		Network: s.Network.Name, ChainID: s.Network.ChainID, Token: s.Network.Token.Hex(), Router: s.Router.Hex(),
		Explorer: s.Network.Explorer, Policy: s.Policy.String(),
		TrustedBlock: st.LatestSeen, VirtualBlock: st.Virtual, VerifiedBlock: st.Finalized,
	}
	resp.Totals.Payments, resp.Totals.Confirmed, resp.Totals.Verified = totals.Count, totals.Confirmed, totals.Verified
	resp.Totals.Orphaned, resp.Totals.Volume = totals.Orphaned, toAmount(totals.Volume)
	if cur != nil {
		resp.IndexedBlock = cur.Number
		resp.LagBlocks = st.LatestSeen - min(st.LatestSeen, cur.Number)
	}
	writeJSON(w, http.StatusOK, resp)
}

type paymentJSON struct {
	InvoiceID   string     `json:"invoiceId"`
	Merchant    string     `json:"merchant"`
	Payer       string     `json:"payer"`
	Agent       *string    `json:"agent"` // null for a payment the payer made directly
	Amount      amount     `json:"amount"`
	TxHash      string     `json:"txHash"`
	LogIndex    int        `json:"logIndex"`
	BlockNumber uint64     `json:"blockNumber"`
	BlockTime   time.Time  `json:"blockTime"`
	Assurance   string     `json:"assurance"` // trusted, virtual or verified
	Required    string     `json:"required"`  // level the merchant's policy needs for this amount
	Confirmed   bool       `json:"confirmed"` // safe to deliver under the policy
	ConfirmedAt *time.Time `json:"confirmedAt"`
	VerifiedAt  *time.Time `json:"verifiedAt"`
	ExplorerURL string     `json:"explorerUrl,omitempty"`
}

func (s *Server) toJSON(p store.PaymentRow) paymentJSON {
	out := paymentJSON{
		InvoiceID: p.InvoiceID, Merchant: p.Merchant, Payer: p.Payer, Amount: toAmount(p.Amount),
		TxHash: p.TxHash, LogIndex: p.LogIndex, BlockNumber: p.BlockNumber, BlockTime: p.BlockTime,
		Assurance: p.Status.String(), Required: p.Required.String(), Confirmed: p.ConfirmedAt != nil,
		ConfirmedAt: p.ConfirmedAt, VerifiedAt: p.VerifiedAt,
	}
	if p.Agent != "" {
		a := p.Agent
		out.Agent = &a
	}
	if s.Network.Explorer != "" {
		out.ExplorerURL = s.Network.Explorer + "/tx/" + p.TxHash
	}
	return out
}

type paymentsResponse struct {
	Payments []paymentJSON `json:"payments"`
	Next     *string       `json:"next"` // pass as ?before= for the next page
}

// payments lists payments newest first: ?merchant=0x..&limit=25&before=<block>:<logIndex>
func (s *Server) payments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := store.Page{Limit: 25}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		page.Limit = n
	}
	if v := q.Get("before"); v != "" {
		if _, err := fmt.Sscanf(v, "%d:%d", &page.BeforeBlock, &page.BeforeLog); err != nil {
			writeError(w, http.StatusBadRequest, "before must look like <block>:<logIndex>")
			return
		}
	}
	var merchant *common.Address
	if v := q.Get("merchant"); v != "" {
		if !common.IsHexAddress(v) {
			writeError(w, http.StatusBadRequest, "invalid merchant address")
			return
		}
		m := common.HexToAddress(v)
		merchant = &m
	}
	rows, err := s.Store.ListPayments(r.Context(), merchant, page)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := paymentsResponse{Payments: make([]paymentJSON, 0, len(rows))}
	for _, p := range rows {
		resp.Payments = append(resp.Payments, s.toJSON(p))
	}
	if len(rows) == page.Limit {
		last := rows[len(rows)-1]
		next := fmt.Sprintf("%d:%d", last.BlockNumber, last.LogIndex)
		resp.Next = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

// InvoiceID parses an invoice id: a 0x-prefixed 32-byte hex value is used as is, anything else
// is hashed with keccak256, matching ethers.id("INV-42") in the agent SDK.
func InvoiceID(raw string) common.Hash {
	if len(raw) == 66 && strings.HasPrefix(raw, "0x") {
		if b := common.FromHex(raw); len(b) == 32 {
			return common.BytesToHash(b)
		}
	}
	return crypto.Keccak256Hash([]byte(raw))
}

type invoiceResponse struct {
	Merchant  string       `json:"merchant"`
	InvoiceID string       `json:"invoiceId"`
	Paid      bool         `json:"paid"`
	Payment   *paymentJSON `json:"payment"`
}

// invoice answers "has this invoice been paid, and is it safe to deliver?" — the question a
// merchant (or a paying agent waiting for its goods) polls when it does not take webhooks.
func (s *Server) invoice(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("merchant")
	if !common.IsHexAddress(raw) {
		writeError(w, http.StatusBadRequest, "invalid merchant address")
		return
	}
	merchant, id := common.HexToAddress(raw), InvoiceID(r.PathValue("invoice"))
	p, err := s.Store.Invoice(r.Context(), merchant, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := invoiceResponse{Merchant: strings.ToLower(merchant.Hex()), InvoiceID: id.Hex(), Paid: p != nil}
	if p != nil {
		j := s.toJSON(*p)
		resp.Payment = &j
	}
	writeJSON(w, http.StatusOK, resp)
}

type evidenceResponse struct {
	Block uint64 `json:"block"`
	*chain.Evidence
}

// evidence returns the zkEVM batch that holds a block and the L1 transactions that sequenced and
// verified it: the proof, beyond the settler's word, that a payment is final.
func (s *Server) evidence(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseUint(r.PathValue("block"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "block must be a number")
		return
	}
	if s.Evidence == nil {
		writeError(w, http.StatusNotFound, "this network has no L1 evidence")
		return
	}
	s.mu.Lock()
	ev := s.cache[n]
	s.mu.Unlock()
	if ev == nil {
		ev, err = s.Evidence.Evidence(r.Context(), n)
		if err != nil {
			s.Log.Warn("evidence lookup failed", "block", n, "err", err)
			writeError(w, http.StatusBadGateway, "could not read the batch from the node")
			return
		}
		if ev == nil {
			writeError(w, http.StatusNotFound, "this network has no L1 evidence")
			return
		}
		if ev.VerifyTxHash != nil {
			s.mu.Lock()
			if s.cache == nil || len(s.cache) > 4096 {
				s.cache = map[uint64]*chain.Evidence{}
			}
			s.cache[n] = ev
			s.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, evidenceResponse{Block: n, Evidence: ev})
}

type eventJSON struct {
	EventID     string          `json:"eventId"`
	Type        string          `json:"type"`
	CreatedAt   time.Time       `json:"createdAt"`
	DeliveredAt *time.Time      `json:"deliveredAt"`
	Attempts    int             `json:"attempts"`
	LastError   *string         `json:"lastError"`
	Body        json.RawMessage `json:"body"`
}

// events is the webhook delivery log, newest first.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	evs, err := s.Store.RecentEvents(r.Context(), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]eventJSON, 0, len(evs))
	for _, e := range evs {
		body, err := outbox.Message(e, s.Network.Explorer)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, eventJSON{EventID: e.EventID, Type: e.Type, CreatedAt: e.CreatedAt, DeliveredAt: e.PublishedAt,
			Attempts: e.Attempts, LastError: e.LastError, Body: body})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("api error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep "trusted<=1" readable
	_ = enc.Encode(v)
}
