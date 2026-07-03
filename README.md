# inferenced

> A provider-agnostic local gateway that transparently meters your team's AI inference — tokens, models, and cost — without changing a line of application code.

`inferenced` sits between a developer's coding agent (Claude Code, Cursor, aider, an OpenAI-compatible client, …) and the upstream LLM API. It forwards traffic unchanged while **teeing** the streamed response to meter token usage and cost, then emits a structured telemetry event — all without buffering the response or adding perceivable latency.

> **Status:** early spike. The metering core and provider seam are working; the control plane, transparent interception, and cost enforcement are on the roadmap.

## How it works

```text
inbound HTTP ─► [detect provider] ─► [ReverseProxy: rewrite to upstream host, HTTPS]
                                          │
                                          ▼ (tee the response body)
                                   ┌──────┴───────────────────────────────┐
                            client (hot path,                      StreamParser
                            unbuffered, flushed)              (observation path)
                                                                     │
                                                          telemetry.Event ─► Sink
```

- **Generic core** — a stdlib `httputil.ReverseProxy` (`FlushInterval = -1`) whose `ModifyResponse` tees the body via `io.TeeReader` + `io.Pipe`: one copy streams to the client instantly, the other feeds a provider-specific parser.
- **Provider seam** — everything vendor-specific (upstream host, request/stream shapes, pricing) lives behind a `Provider` interface. Detection is Host-first (for future transparent interception), then falls back to path/header heuristics.
- **Structured telemetry** — each request produces a serializable `Event` handed to a `Sink`. Today that's stdout; the control-plane shipper is a drop-in.
- **Developer identity** — a stable, non-secret `sha256(credential)[:12]` is attached to every event for future per-person/team cost attribution. The raw credential is never logged or stored.

## Quickstart

Run the gateway:

```bash
go run ./cmd/gateway --port=8766
# or, from a release build:
inferenced --port=8766
```

Point a coding agent at it and use it normally:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8766"
claude "Write a hello world in Rust"
```

Or any OpenAI-compatible client:

```bash
export OPENAI_BASE_URL="http://127.0.0.1:8766/v1"
```

The gateway prints a lifecycle line per request:

```text
[REQUEST]  --> POST /v1/messages | Provider: anthropic | Model: claude-3-5-sonnet | Dev: 4b578105e8c7 | Auth: sk-ant...[Redacted]
[STREAM]   --> Processing stream chunks...
[RESPONSE] <-- Complete | Status: 200 | Provider: anthropic | Model: claude-3-5-sonnet | Tokens In: ~842 | Tokens Out: ~1240 | Approx Cost: $0.02106
```

## Provider support

| Provider | Status | Notes |
| --- | --- | --- |
| Anthropic | ✅ | Messages API, SSE + non-stream usage |
| OpenAI (and OpenAI-compatible) | ✅ | Chat Completions, SSE + `usage` |
| Google Gemini | 🔜 | planned |
| Ollama (local) | 🔜 | planned |

Adding a provider is a single file implementing the `Provider` interface plus one registry entry. Browser-based inference (e.g. chatgpt.com) is out of scope.

## Development

Requires **Go 1.26** and [Task](https://taskfile.dev). Tooling (Ginkgo, golangci-lint) is pinned via Go tool directives — no separate install needed.

```bash
task            # vet + lint + test
task test       # Ginkgo suites with the race detector
task lint       # golangci-lint
task build      # -> bin/inferenced
task run -- --port=8766
```

## Roadmap

- Telemetry control plane: ingest events, aggregate spend, push policy/config down to each gateway.
- Transparent interception: JAMF-pushed binary + local DNS map (no per-user `*_BASE_URL` env override).
- Cost management: per-person / per-team spend limits and enforcement.
- Dynamic model selection: policy-driven downgrades (e.g. Opus → Sonnet over a spend threshold).
- Productivity signals: correlate AI usage with commits / PRs.

## License

[MIT](LICENSE) © 2026 Fraser Isbester
