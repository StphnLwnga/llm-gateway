package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/ollama"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
	"github.com/StphnLwnga/llm-gateway/internal/router"
)

// providers returns every configured provider, by name: the ones with a platform key, and Ollama when its URL is set.
// Each one has liveness deadlines, and GenAI telemetry outside them, so a passed deadline ends its span.
func providers(ctx context.Context, cfg config.Config, client *http.Client, tp trace.TracerProvider, mp metric.MeterProvider,
	log *slog.Logger) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	var errs []error
	add := func(name, baseURL string, p provider.Provider) {
		w := &provider.Watchdog{Name: name, Inner: p, FirstEvent: cfg.ProviderFirstTokenTimeout, Stall: cfg.StreamStallTimeout}
		g, err := provider.NewGenAI(w, name, baseURL, tp, mp)
		errs = append(errs, err)
		out[name] = g
		log.InfoContext(ctx, "provider enabled", "provider", name, "base_url", baseURL)
	}
	for _, p := range []struct {
		name, keyName, baseURL string
		key                    config.Secret
	}{
		{"groq", "GROQ_API_KEY", cfg.GroqBaseURL, cfg.GroqAPIKey},
		{"openai", "OPENAI_API_KEY", cfg.OpenAIBaseURL, cfg.OpenAIAPIKey},
	} {
		if p.key == "" {
			log.InfoContext(ctx, "provider disabled", "provider", p.name, "reason", p.keyName+" is not set")
			continue
		}
		add(p.name, p.baseURL, openaicompat.New(p.name, p.baseURL, p.key, client))
	}
	if cfg.OllamaBaseURL == "" {
		log.InfoContext(ctx, "provider disabled", "provider", "ollama", "reason", "OLLAMA_BASE_URL is not set")
	} else {
		add("ollama", cfg.OllamaBaseURL, ollama.New("ollama", cfg.OllamaBaseURL, client))
	}
	return out, errors.Join(errs...)
}

// routing serves the router's snapshot in the HTTP API's types.
type routing struct {
	routes *router.Router
}

func (r routing) Models() []httpapi.Model {
	var out []httpapi.Model
	for _, m := range r.routes.Models() {
		out = append(out, httpapi.Model{ID: m.Alias, Created: m.Created})
	}
	return out
}

func (r routing) Resolve(alias string) []httpapi.Target {
	var out []httpapi.Target
	for _, t := range r.routes.Resolve(alias) {
		out = append(out, httpapi.Target{
			ProviderName: t.Route.Provider, Provider: t.Provider, UpstreamModel: t.Route.UpstreamModel,
			DefaultMaxTokens: t.Route.DefaultMaxTokens, MaxMaxTokens: t.Route.MaxMaxTokens,
		})
	}
	return out
}
