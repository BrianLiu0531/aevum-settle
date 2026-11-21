package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
)

type mandateEventJSON struct {
	Kind          string    `json:"kind"` // granted or revoked
	Owner         string    `json:"owner"`
	Agent         string    `json:"agent"`
	PerPaymentCap *amount   `json:"perPaymentCap,omitempty"`
	DailyCap      *amount   `json:"dailyCap,omitempty"`
	ExpiresAt     *int64    `json:"expiresAt,omitempty"`
	Merchants     []string  `json:"merchants,omitempty"` // empty = any merchant
	TxHash        string    `json:"txHash"`
	BlockNumber   uint64    `json:"blockNumber"`
	BlockTime     time.Time `json:"blockTime"`
	Assurance     string    `json:"assurance"`
	ExplorerURL   string    `json:"explorerUrl,omitempty"`
}

func (s *Server) mandateJSON(m store.MandateEventRow, st store.SyncState) mandateEventJSON {
	out := mandateEventJSON{
		Kind: m.Kind, Owner: m.Owner, Agent: m.Agent, ExpiresAt: m.ExpiresAt, Merchants: m.Merchants,
		TxHash: m.TxHash, BlockNumber: m.BlockNumber, BlockTime: time.Unix(m.BlockTime, 0).UTC(),
		Assurance: assuranceAt(m.BlockNumber, st).String(),
	}
	if m.PerPaymentCap != nil {
		a := toAmount(m.PerPaymentCap)
		out.PerPaymentCap = &a
	}
	if m.DailyCap != nil {
		a := toAmount(m.DailyCap)
		out.DailyCap = &a
	}
	if s.Network.Explorer != "" {
		out.ExplorerURL = s.Network.Explorer + "/tx/" + m.TxHash
	}
	return out
}

func assuranceAt(block uint64, st store.SyncState) store.Level {
	switch {
	case block <= st.Finalized:
		return store.Verified
	case block <= st.Virtual:
		return store.Virtual
	}
	return store.Trusted
}

type mandateSummaryJSON struct {
	Owner         string           `json:"owner"`
	Agent         string           `json:"agent"`
	Status        string           `json:"status"` // active, expired or revoked
	Current       mandateEventJSON `json:"current"`
	Grants        int              `json:"grants"`
	FirstGranted  time.Time        `json:"firstGranted"`
	Payments      int              `json:"payments"`
	Spent         amount           `json:"spent"`
	LastPaymentAt *time.Time       `json:"lastPaymentAt"`
}

// mandates is the audit trail of what agents may spend: ?owner=0x..&agent=0x..&limit=50.
// It returns the current state of each owner/agent pair and the events behind it, newest first.
func (s *Server) mandates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var owner, agent *common.Address
	for key, dst := range map[string]**common.Address{"owner": &owner, "agent": &agent} {
		if v := q.Get(key); v != "" {
			if !common.IsHexAddress(v) {
				writeError(w, http.StatusBadRequest, "invalid "+key+" address")
				return
			}
			a := common.HexToAddress(v)
			*dst = &a
		}
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	ctx := r.Context()
	st, err := s.Store.State(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	events, err := s.Store.ListMandateEvents(ctx, owner, agent, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	sums, err := s.Store.MandateSummaries(ctx, owner, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now().Unix()
	resp := struct {
		Agents []mandateSummaryJSON `json:"agents"`
		Events []mandateEventJSON   `json:"events"`
	}{Agents: []mandateSummaryJSON{}, Events: make([]mandateEventJSON, 0, len(events))}
	for _, m := range sums {
		if agent != nil && m.Latest.Agent != optLower(agent) {
			continue
		}
		status := "active"
		switch {
		case m.Latest.Kind == "revoked":
			status = "revoked"
		case m.Latest.ExpiresAt != nil && *m.Latest.ExpiresAt <= now:
			status = "expired"
		}
		j := mandateSummaryJSON{
			Owner: m.Latest.Owner, Agent: m.Latest.Agent, Status: status, Current: s.mandateJSON(m.Latest, st),
			Grants: m.Grants, FirstGranted: time.Unix(m.FirstGranted, 0).UTC(), Payments: m.Payments, Spent: toAmount(m.Spent),
		}
		if m.LastPaymentAt != nil {
			t := time.Unix(*m.LastPaymentAt, 0).UTC()
			j.LastPaymentAt = &t
		}
		resp.Agents = append(resp.Agents, j)
	}
	for _, e := range events {
		resp.Events = append(resp.Events, s.mandateJSON(e, st))
	}
	writeJSON(w, http.StatusOK, resp)
}

func optLower(a *common.Address) string { return strings.ToLower(a.Hex()) }
