package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
)

// providers returns every provider that has a platform key, by name.
func providers(ctx context.Context, cfg config.Config, client *http.Client, log *slog.Logger) map[string]provider.Provider {
	out := map[string]provider.Provider{}
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
		out[p.name] = openaicompat.New(p.name, p.baseURL, p.key, client)
		log.InfoContext(ctx, "provider enabled", "provider", p.name, "base_url", p.baseURL)
	}
	return out
}
