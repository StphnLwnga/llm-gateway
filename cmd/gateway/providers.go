package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/ollama"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
	"github.com/StphnLwnga/llm-gateway/internal/router"
)

// providers returns every configured provider, by name: the ones with a platform key, and Ollama when its URL is set.
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
	if cfg.OllamaBaseURL == "" {
		log.InfoContext(ctx, "provider disabled", "provider", "ollama", "reason", "OLLAMA_BASE_URL is not set")
	} else {
		out["ollama"] = ollama.New("ollama", cfg.OllamaBaseURL, client)
		log.InfoContext(ctx, "provider enabled", "provider", "ollama", "base_url", cfg.OllamaBaseURL)
	}
	return out
}

// routedModels lists the router's aliases in the HTTP API's model shape.
type routedModels struct {
	routes *router.Router
}

func (m routedModels) Models() []httpapi.Model {
	var out []httpapi.Model
	for _, rm := range m.routes.Models() {
		out = append(out, httpapi.Model{ID: rm.Alias, Created: rm.Created})
	}
	return out
}
