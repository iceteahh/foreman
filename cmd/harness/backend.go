package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/100xteam-ai/foreman/internal/config"
	"github.com/100xteam-ai/foreman/internal/queue"
	queuepostgres "github.com/100xteam-ai/foreman/internal/queue/postgres"
	queuesqlite "github.com/100xteam-ai/foreman/internal/queue/sqlite"
	"github.com/100xteam-ai/foreman/internal/session"
	storepostgres "github.com/100xteam-ai/foreman/internal/store/postgres"
	storesqlite "github.com/100xteam-ai/foreman/internal/store/sqlite"
)

// openBackend opens the store and the queue for the configured driver. The
// two share one database so a run and its job are written to the same place,
// and a single-node install needs no extra process (plan Step 22, design §10).
func openBackend(ctx context.Context, cfg config.Config, logger *slog.Logger) (backendStore, queue.Queue, error) {
	switch cfg.Database.Driver {
	case "postgres":
		st, err := storepostgres.Open(ctx, cfg.PostgresDSN())
		if err != nil {
			return nil, nil, err
		}
		q, err := queuepostgres.New(ctx, st.Pool())
		if err != nil {
			_ = st.Close()
			return nil, nil, err
		}
		logger.Info("store postgres", "queue", "postgres")
		return st, q, nil
	default:
		st, err := storesqlite.Open(cfg.DBPath())
		if err != nil {
			return nil, nil, err
		}
		q, err := queuesqlite.New(st.DB())
		if err != nil {
			_ = st.Close()
			return nil, nil, err
		}
		return st, q, nil
	}
}

// openSessions opens the transcript snapshot store. A worker that runs as a
// Kubernetes Job needs the object-storage one: the next attempt may be
// scheduled on another node, where a local snapshot no longer exists
// (design §4.4, §10).
func openSessions(cfg config.Config) (session.Store, error) {
	switch cfg.Session.Store {
	case "s3":
		s3, err := session.NewS3(cfg.SessionS3Config())
		if err != nil {
			return nil, fmt.Errorf("session store: %w", err)
		}
		return s3, nil
	default:
		return session.NewLocal(cfg.SessionRoot())
	}
}
