// Command gatewayctl runs one-shot admin tasks for llm-gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

const usage = `usage: gatewayctl migrate up|down|status

Commands:
  migrate up       apply every pending migration
  migrate down     roll back the latest migration
  migrate status   list each migration and whether it is applied

Settings come from the environment. DATABASE_URL is required.
`

// errUsage means the arguments were wrong; the usage text is already printed.
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop) // a second signal kills the process at once
	err := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	if err != nil && !errors.Is(err, errUsage) {
		fmt.Fprintf(os.Stderr, "gatewayctl: %v\n", err)
	}
	os.Exit(exitCode(err))
}

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		return 1
	}
}

// run checks the arguments before the config, so usage prints even without DATABASE_URL.
func run(ctx context.Context, args []string, getenv func(string) (string, bool), stdout, stderr io.Writer) error {
	if len(args) != 2 || args[0] != "migrate" || (args[1] != "up" && args[1] != "down" && args[1] != "status") {
		io.WriteString(stderr, usage)
		return errUsage
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := telemetry.NewLogger(stdout, cfg.LogLevel, cfg.LogFormat)
	return postgres.Migrate(ctx, log, cfg.DatabaseURL, args[1])
}
