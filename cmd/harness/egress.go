package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/config"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/egress"
)

// cmdEgress runs the worker egress allowlist proxy (design §4.1). Workers sit
// on an --internal docker network whose only route out is this process, so
// anything not on the allowlist is refused with 403 and logged.
func cmdEgress(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("egress", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	addr := fs.String("addr", "", "listen address (default egress.addr, else :3128)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	listen := *addr
	if listen == "" {
		listen = cfg.Egress.Addr
	}
	if listen == "" {
		listen = ":3128"
	}
	allow := cfg.EgressAllow()
	logger.Info("egress allowlist", "hosts", allow, "ports", cfg.Egress.Ports)
	p := &egress.Proxy{Allow: egress.NewAllowlist(allow, cfg.Egress.Ports...), Logger: logger}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := egress.Serve(ctx, listen, p); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	allowed, denied := p.Stats()
	logger.Info("egress proxy stopped", "allowed", allowed, "denied", denied)
	return nil
}
