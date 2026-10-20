# Model capabilities (Provider API, endpoints and reasoning effort)

## Current release scope

The current CPA CommandCode Provider integration supports **GOAT, Pro and Max**
through the official Provider API. **Go is not supported**: it uses an official
CLI-specific private interface, not an OpenAI-compatible API, and this plugin
does not implement that private protocol. Independently, Go has **no Provider
API access**. Whether an account has an API key is therefore not a support test.
The name `commandcode-go-cliproxyapi` is inherited from `opencode-go-cliproxyapi`;
its `go` component does not indicate Go-plan support.

The [official Provider documentation][provider] says every plan except Go has
API access, explicitly including GOAT, Pro, Max and Team; it documents Go's
rejection as `403 upgrade_required`. That broader upstream entitlement list is
not a claim that this release has verified every plan. The Go CLI private
protocol boundary above comes from the maintainer's release context (2026-10-20),
not a new wire-level protocol test.

## Current Responses support

When this plugin was created, the upstream did not offer Responses (release
history supplied by the maintainer). The [current official documentation][provider] now
lists `POST https://api.commandcode.ai/provider/v1/responses`: OpenAI and open
models use Chat Completions, and **most**, not all, also support Responses.
Claude models use `/messages` only. The official recommendation is to check each
model's `supported_endpoints` in the [public models catalog][models].

The maintainer's compatibility record for this release (2026-10-20) identifies
these Responses exceptions. An unauthenticated catalog read on **2026-10-20 UTC**
confirmed that each lists only `/chat/completions`, with no `/responses`:

| Model | Exact upstream id | Catalog `supported_endpoints` |
|---|---|---|
| GLM-5.3 Flash | `z-ai/glm-5.3-flash` | `["/chat/completions"]` |
| Tencent Hy4 Preview | `tencent/hy4-preview` | `["/chat/completions"]` |
| DeepSeek V4 Flash Fast | `deepseek/deepseek-v4-flash-fast` | `["/chat/completions"]` |

These are release compatibility records corroborated by the [catalog][models],
not paid inference tests, an exhaustive list of unsupported models, or a promise
of permanent non-support. Do not confuse Flash with
`z-ai/glm-5.3-flashx`: the latter lists both `/chat/completions` and `/responses`
in the same read. Recheck the exact id before changing a route.

The current catalog includes `supported_endpoints` in addition to the six fields
seen in the historical snapshot below. This field describes **serving endpoints**,
not `thinking`, supported reasoning efforts, budgets, or whether an effort is
honoured. No thinking/effort declaration was present in the catalog read for
this update; endpoint support must not be used to infer one.

Evidence checked on **2026-10-20 UTC**: the official Provider page (direct read
and Context7), the public catalog (unauthenticated GET), and the local source
files cited below. No real key, authenticated account request, or inference
request was used. The 2026-09-17 probes and vendor effort table are retained
historical records and **were not rerun or re-extracted** for this update.

## Historical capability investigation (2026-09-17)

The following evidence was gathered against `api.commandcode.ai` on
**2026-09-17**. In particular, the six-field catalog row is a historical
observation, not the current schema or a permanent claim that no capability
metadata can exist:

| Probe | Result |
|---|---|
| `GET https://api.commandcode.ai/provider/v1/models` (no auth needed) | 69 models, each with exactly `id`, `object`, `created`, `owned_by`, `name`, `context_length` — no capability, thinking or effort field |
| `/alpha/*` route inventory from the vendor's own CLI bundle and web bundle | `billing/credits`, `billing/subscriptions`, `usage/summary`, `whoami`, `agent/generate`, `generate`, `learn`, `namespaces`, `sandbox/*`, `share/*`, `taste/*`, `web-fetch`, `web-search`, `fingerprint/record`, `lifecycle-events`, `devrel-thread/*`, `consent/set`, `conversions/track` — **no models/capabilities route** |
| Vendor CLI (`command-code` v1.54.2, `dist/cli.mjs`) | `getSupportedEfforts(model)` reads a static `new Map([...])` of 40 models; a model outside it yields `null` → the CLI reports "Reasoning effort not supported for X" and sends no effort field |
| Vendor web app (`commandcode.ai`, `assets/constants-*.js`) | ships a static model registry with `reasoning: true` and `reasoningEfforts: [...]` per model — the effort selector in the UI reads that bundled table, not an API |

At that time, the examined vendor clients bundled their per-model effort data
rather than fetching thinking declarations from an API. This observation does
not establish a permanent absence of capability metadata or endpoint support.

## Historical effort probes (2026-09-17)

The retained live-probe record for `/provider/v1/chat/completions`, made with a
real provider key on **2026-09-17**, reported acceptance of the union
`low | medium | high | xhigh | max` for the sampled efforts below, including
values outside each model's vendor-declared list:

| Model | Vendor-declared list | Probed efforts | Upstream answer |
|---|---|---|---|
| `deepseek/deepseek-v4.1-flash` | low, high, max | low, high, max, **medium**, **xhigh**, *omitted* | 200 for all |
| `z-ai/glm-5.3-flash` | low, high, max | low, high, max, **medium** | 200 for all |
| `Qwen/Qwen3.8-Max` | low, medium, xhigh | high, **max** | 200 for both |

In that historical record, `reasoning_effort: "none"` (observed in real client
traffic) was rejected with
`Invalid option: expected one of "low"|"medium"|"high"|"xhigh"|"max"`.

**Acceptance is not the same as effect.** Whether the upstream *honours* an
effort outside a model's declared list, or silently falls back to a default, is
not verified here; that would need comparing
`usage.completion_tokens_details.reasoning_tokens` across efforts.

## Historical vendor-declared effort table (2026-09-17)

Source: the web app's model registry (63 models, of which those below carry an
explicit effort list). Extracted 2026-09-17; `command-code` CLI 1.54.2 agrees on
every model the two share.

| Model id | Reasoning efforts |
|---|---|
| `claude-sonnet-5`, `claude-sonnet-4-6`, `claude-fable-5-1`, `claude-fable-5`, `claude-opus-5`, `claude-opus-4-8`, `claude-opus-4-7` | low, medium, high, xhigh, max |
| `gpt-6-astra`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna` | low, medium, high, xhigh, max |
| `gpt-5.5`, `gpt-5.4`, `gpt-5.3-codex` | low, medium, high, xhigh |
| `gpt-5.4-mini`, `MiniMaxAI/MiniMax-M3`, `minimax/minimax-m3-free`, `tencent/hy4-preview`, `google/gemini-3.8-flash`, `google/gemini-3.7-flash`, `google/gemini-3.6-flash`, `google/gemini-3.5-flash`, `google/gemini-3.5-flash-lite`, `google/gemini-3.1-flash-lite`, `xai/grok-4.5` | low, medium, high |
| `deepseek/deepseek-v4.1-flash`, `deepseek/deepseek-v4-flash-fast`, `z-ai/glm-5.3-flash`, `zai-org/GLM-5.3`, `moonshotai/Kimi-K3` | low, high, max |
| `deepseek/deepseek-v4-pro`, `deepseek/deepseek-v4-flash`, `deepseek/deepseek-v4-flash-vision-exp`, `zai-org/GLM-5.2`, `sakana/fugu-ultra` | high, max |
| `Qwen/Qwen3.8-Max`, `Qwen/Qwen3.8-Max-0902`, `Qwen/Qwen3.8-27B`, `Qwen/Qwen3.8-Flash` | low, medium, xhigh |
| `meta/muse-spark-1.1`, `meta/muse-spark-1.2`, `meta/muse-spark-1.2-contributor`, `meta/muse-spark-1.3-contributor`, `xai/grok-4.6` | low, medium, high, xhigh |
| `meta/muse-spark-1.3` | low, medium, high, xhigh, max |
| `moonshotai/Kimi-K2.7-Code`, `moonshotai/Kimi-K2.7-Code-Highspeed`, `Qwen/Qwen3.7-Max`, `Qwen/Qwen3.7-Plus`, `Qwen/Qwen3.7-Flash`, `Qwen/Qwen3.6-Max-Preview`, `Qwen/Qwen3.6-Plus`, `meituan/LongCat-2.0:free`, `stepfun/Step-3.7-Flash`, `stepfun/Step-3.5-Flash`, `tencent/Hy3`, `tencent/hy3-paid`, `nvidia/nemotron-3-ultra-550b-a55b`, `thinkingmachines/inkling`, `thinkingmachines/inkling-small`, `poolside/laguna-s-2.1-free`, `inclusionai/ling-3.0-flash-free`, `inclusionai/ling-3.0-flash-sante:free` | *(none declared — the vendor offers no effort selector)* |

## How the plugin behaves

Source inspection for this update:
[local catalog implementation](../internal/catalog/catalog.go),
[configuration](../internal/config/config.go) and
[executor](../internal/plugin/executor.go).

- `catalog.DefaultRoute` remains **`chat-completions`**. `Manager.resolve`
  applies a per-model `route-overrides` entry first; otherwise it uses that
  default, even when the upstream now supports Responses.
- `rawModel` does **not** decode `supported_endpoints`. The plugin does not
  automatically select an endpoint from that field, switch by model family, or
  retry another endpoint after a protocol failure. The executor returns upstream
  errors; any host-level account/provider retry is a separate concern.
- Enabling `protocols.responses` permits a Responses route; it does not select
  it. A caller's Responses request can still be translated to Chat Completions.
  An explicit `route-overrides` entry, keyed by the exact upstream model id, is
  what changes the default route. Check the catalog before choosing
  `responses` with `/v1/responses`, or `messages` with `/v1/messages` for
  Claude. An override cannot grant Go API access or add upstream model support.

For reasoning, [`internal/thinking/thinking.go`](../internal/thinking/thinking.go)
treats a model's catalog `thinking` object (`levels`, `min`, `max`,
`zero_allowed`, `dynamic_allowed` — decoded by `rawThinking` in the catalog
implementation) as the capability declaration and uses it when present.
The current catalog read has no such declaration; `supported_endpoints` is
not a replacement for it. With undeclared thinking capabilities:

- on the default Chat Completions route, supplied effort choices are forwarded
  subject to the conversion branch. Native Chat Completions input and
  Responses-to-Chat conversion retain supplied `auto` and `none`; those values
  do not guarantee field omission. Omit the input effort setting when no effort
  field should be sent. Upstream acceptance and effect remain authoritative;
- a model whose catalog entry *does* declare valid levels or bounds provides
  capability checks and budget bounds to the relevant conversion paths. A
  `route-overrides` entry alone does not declare these capabilities.

Embedding the historical vendor table as per-model declarations would restore
local validation and let a numeric-budget target (Anthropic Messages) map effort
to a budget, but it is deliberately not done. The historical samples accepted
efforts beyond the vendor lists, so embedding those lists could cause false
rejections. They are undocumented vendor-client snapshots that may change;
neither that table nor HTTP 200 proves the present per-model effect of an effort.

[provider]: https://commandcode.ai/docs/provider
[models]: https://api.commandcode.ai/provider/v1/models
