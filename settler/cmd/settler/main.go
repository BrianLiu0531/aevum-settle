// Command settler follows AgentPayRouter payments on QDay Aevum and tells merchants, through a
// signed webhook, when each one is safe to act on under their confirmation policy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"

	"github.com/BrianLiu0531/aevum-settle/settler/internal/api"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/chain"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/config"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/indexer"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/metrics"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/outbox"
	"github.com/BrianLiu0531/aevum-settle/settler/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("settler stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var log *slog.Logger
	if cfg.LogJSON {
		log = slog.New(slog.NewJSONHandler(os.Stdout, opts))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rpc, err := chain.DialRPC(ctx, chain.RPCConfig{
		URLs: cfg.RPCURLs, MaxRPS: cfg.RPCMaxRPS, MaxAttempts: cfg.RPCMaxAttempts, Timeout: cfg.RPCTimeout, ZkEVM: cfg.Network.ZkEVM,
		LocalVirtualDepth: cfg.LocalVirtualDepth, LocalVerifiedDepth: cfg.LocalVerifiedDepth,
	}, log)
	if err != nil {
		return err
	}
	defer rpc.Close()
	id, err := rpc.ChainID(ctx)
	if err != nil {
		return err
	}
	if id != cfg.Network.ChainID {
		return fmt.Errorf("RPC is on chain %d but NETWORK=%s expects %d", id, cfg.Network.Name, cfg.Network.ChainID)
	}

	st, err := store.Open(ctx, cfg.DatabaseURL, id)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	metrics.Register(reg)
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	ix := indexer.New(indexer.Config{
		Router: cfg.Router, Merchants: cfg.Merchants, StartBlock: cfg.StartBlock, MaxRange: cfg.MaxBlockRange,
		MaxUnfinalizedBatch: cfg.MaxUnfinalizedBatch, Policy: cfg.Policy, PollInterval: cfg.PollInterval,
	}, rpc, st, log)

	var publisher outbox.Publisher = outbox.LogPublisher{Log: log}
	if cfg.WebhookURL != "" {
		publisher = outbox.WebhookPublisher{URL: cfg.WebhookURL, Secret: []byte(cfg.WebhookSecret), Explorer: cfg.Network.Explorer}
		log.Info("delivering events to a webhook", "url", cfg.WebhookURL)
	}
	relay := &outbox.Relay{Store: st, Publisher: publisher, BatchSize: 50, Interval: time.Second, Log: log}

	server := &api.Server{Store: st, Network: cfg.Network, Router: cfg.Router, Policy: cfg.Policy, Metrics: reg, Log: log}
	if cfg.Network.ZkEVM {
		server.Evidence = rpc
	}
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}

	log.Info("starting", "network", cfg.Network.Name, "chain_id", id, "router", cfg.Router.Hex(),
		"merchants", len(cfg.Merchants), "policy", cfg.Policy.String(), "http", cfg.HTTPAddr)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ix.Run(ctx) })
	g.Go(func() error { return relay.Run(ctx) })
	g.Go(func() error {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})

	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		log.Info("shut down cleanly")
		return nil
	}
	return err
}
