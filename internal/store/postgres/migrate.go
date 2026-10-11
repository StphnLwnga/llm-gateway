package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrUnknownCommand is returned for a migrate command other than up, down or status.
var ErrUnknownCommand = errors.New("unknown migrate command")

// Migrate runs up, down or status against dsn; an advisory lock serializes concurrent runs.
func Migrate(ctx context.Context, log *slog.Logger, dsn config.Secret, command string) error {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return migrate(ctx, log, dsn, command, files)
}

// migrate takes the files as an argument so a test can supply a slow migration.
func migrate(ctx context.Context, log *slog.Logger, dsn config.Secret, command string, files fs.FS) error {
	if command != "up" && command != "down" && command != "status" {
		return fmt.Errorf("%w %q: use up, down or status", ErrUnknownCommand, command)
	}

	db, err := sql.Open("pgx", string(dsn))
	if err != nil {
		return errors.New("open DATABASE_URL: not a valid Postgres connection string")
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.WarnContext(ctx, "close database", "err", err)
		}
	}()

	// Retry every 1s, up to 120 times (goose's default: every 5s for 5 minutes).
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
