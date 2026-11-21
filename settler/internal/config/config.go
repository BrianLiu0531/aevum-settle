// Package config reads settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/token"
)

type Config struct {
	Network             token.Network
	RPCURLs             []string
	DatabaseURL         string
	Router              common.Address
	Merchants           []common.Address
	StartBlock          uint64
	Policy              store.Policy
	MaxBlockRange       uint64
	MaxUnfinalizedBatch uint64
	PollInterval        time.Duration
	HTTPAddr            string
	RPCMaxRPS           float64
	RPCMaxAttempts      int
	RPCTimeout          time.Duration
	LogLevel            slog.Level
	LogJSON             bool
	WebhookURL          string
	WebhookSecret       string
	LocalVirtualDepth   uint64
	LocalVerifiedDepth  uint64
}

// DefaultPolicy confirms up to 1 tUSD8 on the sequencer's word, up to 100 once the batch is on L1,
// and anything larger once its validity proof is verified on L1.
const DefaultPolicy = "trusted<=1,virtual<=100,verified"

// FromEnv loads the configuration; see .env.example for every variable.
func FromEnv() (Config, error) {
	e := &env{}
	c := Config{
		RPCURLs:             e.list("RPC_URLS", false),
		DatabaseURL:         e.str("DATABASE_URL", "", true),
		StartBlock:          e.uint("START_BLOCK", 0),
		MaxBlockRange:       e.uint("MAX_BLOCK_RANGE", 1000),
		MaxUnfinalizedBatch: e.uint("MAX_UNFINALIZED_BATCH", 100),
		PollInterval:        e.duration("POLL_INTERVAL", time.Second),
		HTTPAddr:            e.str("HTTP_ADDR", ":8080", false),
		RPCMaxRPS:           e.float("RPC_MAX_RPS", 20),
		RPCMaxAttempts:      int(e.uint("RPC_MAX_ATTEMPTS", 5)),
		RPCTimeout:          e.duration("RPC_TIMEOUT", 15*time.Second),
		LogJSON:             e.str("LOG_FORMAT", "json", false) == "json",
		WebhookURL:          e.str("WEBHOOK_URL", "", false),
		WebhookSecret:       e.str("WEBHOOK_SECRET", "", false),
		LocalVirtualDepth:   e.uint("LOCAL_VIRTUAL_DEPTH", 0),
		LocalVerifiedDepth:  e.uint("LOCAL_VERIFIED_DEPTH", 0),
	}
	if c.WebhookURL != "" && len(c.WebhookSecret) < 16 {
		e.add(errors.New("WEBHOOK_SECRET must be at least 16 characters"))
	}

	network, err := token.Lookup(e.str("NETWORK", "qday-testnet", false))
	e.add(err)
	if raw := e.str("TOKEN_ADDRESS", "", false); raw != "" {
		network.Token = e.address("TOKEN_ADDRESS", raw)
	}
	if v := e.str("EXPLORER_URL", "", false); v != "" {
		network.Explorer = strings.TrimRight(v, "/")
	}
	if len(c.RPCURLs) == 0 && network.Name == "qday-testnet" {
		c.RPCURLs = []string{"https://rpc-test.qday.info"}
	}
	if len(c.RPCURLs) == 0 {
		e.add(errors.New("RPC_URLS is required"))
	}
	c.Network = network

	c.Router = e.address("ROUTER_ADDRESS", e.str("ROUTER_ADDRESS", "", true))
	for _, raw := range e.list("MERCHANTS", false) {
		c.Merchants = append(c.Merchants, e.address("MERCHANTS", raw))
	}

	policy, err := store.ParsePolicy(e.str("CONFIRM_POLICY", DefaultPolicy, false))
	e.add(err)
	c.Policy = policy

	e.add(c.LogLevel.UnmarshalText([]byte(e.str("LOG_LEVEL", "info", false))))
	if c.LocalVerifiedDepth < c.LocalVirtualDepth {
		e.add(errors.New("LOCAL_VERIFIED_DEPTH must be at least LOCAL_VIRTUAL_DEPTH"))
	}
	if c.MaxBlockRange == 0 || c.MaxUnfinalizedBatch == 0 {
		e.add(errors.New("MAX_BLOCK_RANGE and MAX_UNFINALIZED_BATCH must be positive"))
	}
	return c, errors.Join(e.errs...)
}

type env struct{ errs []error }

func (e *env) add(err error) {
	if err != nil {
		e.errs = append(e.errs, err)
	}
}

func (e *env) address(key, raw string) common.Address {
	if raw == "" {
		return common.Address{}
	}
	if !common.IsHexAddress(raw) {
		e.add(fmt.Errorf("%s: %q is not an address", key, raw))
		return common.Address{}
	}
	return common.HexToAddress(raw)
}

func (e *env) str(key, def string, required bool) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		if required {
			e.add(fmt.Errorf("%s is required", key))
		}
		return def
	}
	return v
}

func (e *env) list(key string, required bool) []string {
	var out []string
	for _, part := range strings.Split(e.str(key, "", required), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (e *env) uint(key string, def uint64) uint64 {
	v := e.str(key, "", false)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		e.add(fmt.Errorf("%s: %w", key, err))
	}
	return n
}

func (e *env) float(key string, def float64) float64 {
	v := e.str(key, "", false)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		e.add(fmt.Errorf("%s must be a positive number", key))
	}
	return f
}

func (e *env) duration(key string, def time.Duration) time.Duration {
	v := e.str(key, "", false)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.add(fmt.Errorf("%s: %w", key, err))
	}
	return d
}
