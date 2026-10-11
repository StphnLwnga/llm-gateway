// Command gatewayctl runs one-shot admin tasks for llm-gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/money"
	"github.com/StphnLwnga/llm-gateway/internal/router"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

const usage = `usage: gatewayctl migrate up|down|status
       gatewayctl routes add --alias NAME --provider groq|openai|ollama --upstream-model MODEL --priority N
                             [--input-price USD] [--output-price USD] [--default-max-tokens N] [--max-max-tokens N] [--disabled]
       gatewayctl routes list
       gatewayctl routes enable|disable ID

Commands:
  migrate up       apply every pending migration
  migrate down     roll back the latest migration
  migrate status   list each migration and whether it is applied
  routes add       add a route from a model alias to a provider; prices are USD per million tokens
  routes list      print every route
  routes enable    enable the route with this ID
  routes disable   disable the route with this ID

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

// command is one parsed invocation.
type command struct {
	name    string // "migrate", "routes add", "routes list", "routes enable", "routes disable"
	arg     string // the migrate direction
	id      int64
	route   router.Route
	invalid error // invalid values, reported after the shape is known to be right
}

// run checks the arguments before the config, so usage prints even without DATABASE_URL.
func run(ctx context.Context, args []string, getenv func(string) (string, bool), stdout, stderr io.Writer) error {
	cmd, ok := parse(args, stderr)
	if !ok {
		_, _ = io.WriteString(stderr, usage)
		return errUsage
	}
	if cmd.invalid != nil {
		return cmd.invalid
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := telemetry.NewLogger(stdout, cfg.LogLevel, cfg.LogFormat)
	if cmd.name == "migrate" {
		return postgres.Migrate(ctx, log, cfg.DatabaseURL, cmd.arg)
	}

	pool, err := postgres.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	routes := postgres.Routes{Pool: pool}

	switch cmd.name {
	case "routes add":
		id, err := routes.Add(ctx, cmd.route)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "route %d added: %s -> %s/%s, priority %d\n",
			id, cmd.route.Alias, cmd.route.Provider, cmd.route.UpstreamModel, cmd.route.Priority)
		return err
	case "routes list":
		all, err := routes.List(ctx)
		if err != nil {
			return err
		}
		return printRoutes(stdout, all)
	default: // routes enable, routes disable
		enabled := cmd.name == "routes enable"
		if err := routes.SetEnabled(ctx, cmd.id, enabled); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "route %d %sd\n", cmd.id, cmd.name[len("routes "):])
		return err
	}
}

// parse reports false when the shape of the arguments is wrong. A well-formed
// command with invalid values comes back with invalid set.
func parse(args []string, stderr io.Writer) (command, bool) {
	switch {
	case len(args) == 2 && args[0] == "migrate" && (args[1] == "up" || args[1] == "down" || args[1] == "status"):
		return command{name: "migrate", arg: args[1]}, true
	case len(args) == 2 && args[0] == "routes" && args[1] == "list":
		return command{name: "routes list"}, true
	case len(args) == 3 && args[0] == "routes" && (args[1] == "enable" || args[1] == "disable"):
		id, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || id < 1 {
			return command{}, false
		}
		return command{name: "routes " + args[1], id: id}, true
	case len(args) >= 2 && args[0] == "routes" && args[1] == "add":
		return parseAdd(args[2:], stderr)
	}
	return command{}, false
}

func parseAdd(args []string, stderr io.Writer) (command, bool) {
	fs := flag.NewFlagSet("routes add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	alias := fs.String("alias", "", "")
	prov := fs.String("provider", "", "")
	model := fs.String("upstream-model", "", "")
	priority := fs.Int("priority", -1, "")
	inPrice := fs.String("input-price", "0", "")
	outPrice := fs.String("output-price", "0", "")
	defMax := fs.Int("default-max-tokens", 0, "")
	maxMax := fs.Int("max-max-tokens", 0, "")
	disabled := fs.Bool("disabled", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *alias == "" || *prov == "" || *model == "" || *priority < 0 {
		return command{}, false
	}

	rt := router.Route{Alias: *alias, Provider: *prov, UpstreamModel: *model, Priority: *priority, Enabled: !*disabled}
	var errs []error
	var err error
	if rt.InputPrice, err = money.ParseUSD(*inPrice); err != nil {
		errs = append(errs, fmt.Errorf("--input-price: %w", err))
	}
	if rt.OutputPrice, err = money.ParseUSD(*outPrice); err != nil {
		errs = append(errs, fmt.Errorf("--output-price: %w", err))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["default-max-tokens"] {
		rt.DefaultMaxTokens = defMax
	}
	if set["max-max-tokens"] {
		rt.MaxMaxTokens = maxMax
	}
	if len(errs) == 0 {
		if err := rt.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return command{name: "routes add", route: rt, invalid: errors.Join(errs...)}, true
}

func printRoutes(w io.Writer, routes []router.Route) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tALIAS\tPRIORITY\tPROVIDER\tUPSTREAM MODEL\tENABLED\tINPUT USD/1M\tOUTPUT USD/1M\tDEFAULT MAX\tMAX MAX")
	opt := func(p *int) string {
		if p == nil {
			return "-"
		}
		return strconv.Itoa(*p)
	}
	for _, r := range routes {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%t\t%s\t%s\t%s\t%s\n", r.ID, r.Alias, r.Priority, r.Provider,
			r.UpstreamModel, r.Enabled, r.InputPrice, r.OutputPrice, opt(r.DefaultMaxTokens), opt(r.MaxMaxTokens))
	}
	return tw.Flush()
}
