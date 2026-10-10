package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver that goose needs
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// Migration files are compiled into the binary, so the image needs no
// migrations folder and the binary always runs the schema it was built with.
// Each SQL file runs in its own transaction (goose's default).
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrUnknownCommand is returned for a migrate command other than up, down or status.
var ErrUnknownCommand = errors.New("unknown migrate command")

// Migrate runs one goose command (up, down or status) against dsn and logs
// the result. A Postgres advisory lock serializes concurrent runs, so two
// migrate jobs that start together apply each migration once.
func Migrate(ctx context.Context, log *slog.Logger, dsn config.Secret, command string) error {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return migrate(ctx, log, dsn, command, files)
}

// migrate is Migrate with the migration files as an argument, so a test can
// supply a slow migration and prove the lock.
func migrate(ctx context.Context, log *slog.Logger, dsn config.Secret, command string, files fs.FS) error {
	if command != "up" && command != "down" && command != "status" {
		return fmt.Errorf("%w %q: use up, down or status", ErrUnknownCommand, command)
	}

	db, err := sql.Open("pgx", string(dsn))
	if err != nil {
		return errors.New("open DATABASE_URL: not a valid Postgres connection string")
	}
	defer db.Close()

	// Retry the lock every second for up to 2 minutes, instead of goose's
	// default 5 seconds, so a waiting run continues soon after the first ends.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 120))
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	switch command {
	case "up":
		results, err := provider.Up(ctx)
		for _, r := range results {
			log.InfoContext(ctx, "migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration.String())
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		if len(results) == 0 {
			log.InfoContext(ctx, "no migrations to apply")
		}
	case "down":
		r, err := provider.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		log.InfoContext(ctx, "migration rolled back", "version", r.Source.Version, "file", r.Source.Path)
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range statuses {
			attrs := []any{"version", s.Source.Version, "file", s.Source.Path, "state", string(s.State)}
			if !s.AppliedAt.IsZero() {
				attrs = append(attrs, "applied_at", s.AppliedAt)
			}
			log.InfoContext(ctx, "migration status", attrs...)
		}
	}
	return nil
}
