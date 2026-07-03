# inferenced

> A provider-agnostic local gateway that transparently meters your team's AI inference — tokens, models, and cost — without changing a line of application code.

`inferenced` sits between a developer's coding agent (Claude Code, Cursor, aider, an OpenAI-compatible client, …) and the upstream LLM API. It forwards traffic unchanged while **teeing** the streamed response to meter token usage and cost, then emits a structured telemetry event — all without buffering the response or adding perceivable latency.

> **Status:** early spike. The metering core, provider seam, layered pricing (live + custom sheets), and local spend enforcement are working; the control plane and transparent interception are on the roadmap.

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

## Install

Homebrew (tap):

```bash
brew install Fraser-Isbester/tap/inferenced
```

Or the curl installer (downloads the latest release for your OS/arch, verifies the sha256, installs to `~/.local/bin`):

```bash
curl -fsSL https://raw.githubusercontent.com/Fraser-Isbester/inferenced/main/install.sh | sh
```

Then optionally register the background gateway service:

```bash
inferenced install
```

## CLI

`inferenced` is a single binary with a small command tree:

| Command | What it does |
| --- | --- |
| `serve` | Run the local inference gateway (`--port` overrides the config listen address). |
| `install` / `uninstall` | Install or remove the background gateway service (launchd on macOS, systemd user unit on Linux). |
| `config init` | Write a starter config to the resolved path (`--force` to overwrite). |
| `config validate` | Load the config and validate its enforcement limits. |
| `config path` | Print the config path in effect. |
| `config show` | Print the effective config as YAML. |
| `spend` | Summarize recorded spend from the local ledger (`--period day\|month\|rolling`, `--window`, `--since`). |
| `ui` | Local web dashboard (coming soon). |
| `version` | Print the build version. |

## Quickstart

Run the gateway from a release build:

```bash
inferenced serve --port=8766
# or from source:
go run ./cmd/inferenced serve --port=8766
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
| OpenAI | ✅ | Chat Completions, Responses, Embeddings — SSE + `usage` |
| OpenAI-compatible (config) | ✅ | Azure OpenAI, OpenRouter, Groq, Together, Fireworks, vLLM, LM Studio, … — one config line each |
| Google Gemini | ✅ | `generateContent`/`streamGenerateContent`, `usageMetadata` |
| Ollama (local) | ✅ | `/api/chat`, `/api/generate` (NDJSON), `$0` cost |
| AWS Bedrock, GCP Vertex | 🔜 | need bespoke request signing (SigV4 / OAuth) |

Built-in providers are a single file implementing the `Provider` interface plus a registry entry. Any endpoint that speaks the **OpenAI wire format** needs no code at all — add it under `providers.openai_compatible` in the config (see below). Browser-based inference (e.g. chatgpt.com) is out of scope.

## Configuration

A config file is optional — with none present, inferenced listens on `127.0.0.1:8766` with all built-in providers enabled. Point at one with `--config`, or drop it at `./inferenced.yaml` or `$XDG_CONFIG_HOME/inferenced/config.yaml`. See [`inferenced.example.yaml`](inferenced.example.yaml) for the full schema. Highlights:

```yaml
developer:
  id: "alice@example.com"   # overrides the anonymized credential hash for attribution
  team: "platform"
providers:
  openai: { enabled: false }         # disable a built-in
  ollama: { enabled: true, upstream: "localhost:11434" }
  openai_compatible:                 # capture any OpenAI-wire endpoint, no code
    - { name: openrouter, host: openrouter.ai, base_path: /api/v1 }
    - { name: vllm-local, host: "localhost:8000", scheme: http }
```

## Pricing

Cost is resolved in layers, highest priority first:

1. a **custom price sheet** (`pricing.sheet`) — authoritative when set,
2. **live prices** fetched on boot from [models.dev](https://models.dev) and cached to disk with a TTL,
3. inferenced's **embedded family defaults** (bundled in the binary),
4. each provider's **built-in price** (final fallback).

There is no first-party per-provider pricing API, so "live" prices come from the community-maintained **models.dev** aggregate (`https://models.dev/api.json`). The fetch happens once at startup behind a 24h disk cache and **degrades gracefully** — a network failure falls back to the last cached copy, then to the embedded defaults, and never blocks the gateway from starting. Setting a custom sheet is treated as authoritative and skips the live fetch (the embedded defaults still backfill any model the sheet omits).

A custom sheet is YAML or JSON with the same shape as a provider's inline `pricing` block — `provider → (model substring | "default") → per-1M price`:

```yaml
# prices.yaml
anthropic:
  default: { input_per_1m: 3.0,  output_per_1m: 15.0 }
  opus:    { input_per_1m: 15.0, output_per_1m: 75.0 }
openai:
  gpt-4o-mini: { input_per_1m: 0.15, output_per_1m: 0.60 }
```

```yaml
pricing:
  sheet: "/etc/inferenced/prices.yaml"   # optional; skips the live fetch
  remote:
    enabled: true                        # default; false = embedded defaults only
    ttl: "24h"
```

Model matching is case-insensitive and picks the **longest** substring match (so `gpt-4o-mini` beats `gpt-4o`), falling back to the provider's `default` entry.

## Enforcement & local spend ledger

Every metered request is persisted to a local, pure-Go **SQLite** ledger (no CGO). The ledger records the per-request cost, model, developer, and team, and is the source of truth for spend enforcement. It lives at `$XDG_DATA_HOME/inferenced/inferenced.db` (falling back to `~/.local/share/inferenced/inferenced.db`) unless you set `storage.path`. Persistence is on by default; set `storage.enabled: false` to turn it off.

On top of the ledger, inferenced can **enforce spend limits** defined entirely in local config — no backend required. Limits are keyed on an abstract team/group concept plus the developer identity, so you can cap an individual, a whole team, or everyone at once.

```yaml
storage:
  enabled: true          # default; persist every request to the local ledger

enforcement:
  enabled: true          # evaluate the limits below on every request
  action: block          # "block" enforces thresholds; "warn" is a dry run (log only)
  block_status: 402      # HTTP status when blocking (default 402 Payment Required)
  downgrade:             # model down-scoping map (first substring match wins)
    - { match: opus,   to: claude-sonnet-4-5 }
    - { match: sonnet, to: claude-haiku-4-5 }
  limits:
    # team-a: down-scope models at 80% of budget, hard-block at 120%.
    - { scope: team, id: team-a, period: day,   limit_usd: 100, downgrade_at: 0.8, block_at: 1.2 }
    # team-b: never downgraded, simply blocked at 100%.
    - { scope: team, id: team-b, period: month, limit_usd: 500 }
    - { scope: developer, id: "*", period: day, limit_usd: 5 }
```

- **Scope** is `developer` or `team`; `id: "*"` matches everyone in that scope, otherwise the limit applies only to the named developer/team.
- **Period** is `day`, `month`, or `rolling`. `day` and `month` reset at the local calendar boundary; `rolling` sums spend over a trailing `window` (any Go duration, e.g. `168h`) and is **required** for rolling limits.
- `limit_usd` is the reference budget. **Thresholds are fractions of it and fully per-limit**, so the policy is rule-based: `downgrade_at` (e.g. `0.8`) is where the model is down-scoped, `block_at` (e.g. `1.2`, default `1.0`) is where the request is blocked. Omitting `downgrade_at` means the limit only ever blocks.

**Downgrade** (model failover): when a limit crosses `downgrade_at` (but is still under `block_at`), inferenced rewrites the request's model in-flight to the target of the first matching `downgrade` rule — the JSON `model` field for Anthropic/OpenAI/Ollama, or the URL path for Gemini — and forwards it. The downgraded request is metered under the cheaper model. Block always wins over downgrade, and a rewrite that can't be applied (no matching rule, parse error) forwards the request unchanged rather than dropping it.

**Block:** when a limit reaches `block_at` and `action: block`, the request is denied (never forwarded) with a **provider-shaped error envelope** so the calling client surfaces a native, non-retriable error. The status defaults to **402 Payment Required** (`block_status`), which coding agents read as a quota/billing stop — deliberately **not** 401/403 (Claude Code maps those to "run `/login`") or 429 (treated as retryable rate-limiting). The error type is mapped per provider for a 402 budget block (Anthropic `invalid_request_error`, OpenAI `insufficient_quota`, Gemini `FAILED_PRECONDITION`).

With `action: warn`, nothing is enforced — every block/downgrade intent is logged (`[LIMIT] warn …`) and the request forwarded unchanged, so you can dry-run a policy. Enabling enforcement automatically starts the ledger even if `storage.enabled` is false, since enforcement reads from it. Spend-query failures **fail open** (the request is allowed) so a ledger hiccup never blocks a developer.

## Development

Requires **Go 1.26** and [Task](https://taskfile.dev). Tooling (Ginkgo, golangci-lint) is pinned via Go tool directives — no separate install needed.

```bash
task            # vet + lint + test
task test       # Ginkgo suites with the race detector
task lint       # golangci-lint
task build      # -> bin/inferenced
task run -- serve --port=8766
```

## Roadmap

- Telemetry control plane: ingest events, aggregate spend, push policy/config down to each gateway.
- Transparent interception: JAMF-pushed binary + local DNS map (no per-user `*_BASE_URL` env override).
- Productivity signals: correlate AI usage with commits / PRs.
- Local web dashboard (`inferenced ui`): realtime telemetry, spend, and editable config.

## License

[MIT](LICENSE) © 2026 Fraser Isbester
