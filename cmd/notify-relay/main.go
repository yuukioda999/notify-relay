// Command notify-relay runs the email delivery service.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/yuukioda999/notify-relay/internal/adapter/httpapi"
	"github.com/yuukioda999/notify-relay/internal/adapter/smtpmail"
	"github.com/yuukioda999/notify-relay/internal/config"
	"github.com/yuukioda999/notify-relay/internal/infra/mysqlstore"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// idlePoll is how long the sender waits when nothing is due.
const idlePoll = time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("notify-relay stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := openDatabase(ctx, cfg.DatabaseDSN, logger)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := mysqlstore.Migrate(ctx, db); err != nil {
		return err
	}
	store := &mysqlstore.Store{DB: db}

	sender := &usecase.Sender{
		Queue: store,
		Mailer: &smtpmail.Mailer{
			Addr:     cfg.SMTP.Addr,
			From:     cfg.SMTP.From,
			FromName: cfg.SMTP.FromName,
			Username: cfg.SMTP.Username,
			Password: cfg.SMTP.Password,
			TLS:      smtpmail.TLSMode(cfg.SMTP.TLS),
			Footer:   cfg.SMTP.Footer,
		},
		Concurrency: cfg.SendConcurrency,
		DemoMode:    cfg.DemoMode,
		Logger:      logger,
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Checks:   map[string]httpapi.ReadinessCheck{"database": db.PingContext},
			Acceptor: &usecase.Acceptor{Store: store, DemoMode: cfg.DemoMode},
			Keys:     cfg.SigningKeys,
			Logger:   logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sender.Run(ctx, idlePoll)
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("notify-relay listening", "addr", cfg.Addr, "demo_mode", cfg.DemoMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
	}

	// Stop accepting new requests, then let mail that is already being sent finish (AC-R001-14).
	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Warn("sending did not finish before the shutdown timeout; interrupted recipients are retried on the next start")
	}
	return err
}

// openDatabase retries for up to a minute so the service can start alongside MySQL.
func openDatabase(ctx context.Context, dsn string, logger *slog.Logger) (*sql.DB, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		db, err := mysqlstore.Open(ctx, dsn)
		if err == nil {
			return db, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		logger.Warn("database is not ready; retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
