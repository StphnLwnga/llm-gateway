# Ollama fixtures

NDJSON has no comment syntax, so the provenance of each file is recorded here.

| File | Source |
|---|---|
| `ollama_stream_recorded.ndjson` | Recorded from Ollama 0.40.3, model `llama3.2:3b`, on 2026-10-11. `created_at` is normalised. |
| `ollama_chat_recorded.json` | Recorded from Ollama 0.40.3, model `llama3.2:3b`, on 2026-10-11. `created_at` is normalised. |

Other cases in the tests (thinking, error objects, missing counts) are constructed from Ollama's API documentation, not recorded, and are written inline in `ollama_test.go`.
