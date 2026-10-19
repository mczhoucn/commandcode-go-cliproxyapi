# CommandCode GOAT CLIProxyAPI Plugin

**English** | [简体中文](README.zh-CN.md)

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes a **CommandCode GOAT** plan as a single provider (`commandcode`; published model ids carry the `commandcode/` prefix).

The plugin owns model discovery, protocol translation, execution, and the CommandCode accounts and quota page. API keys come only from CLIProxyAPI auth files, so those accounts serve OpenAI, Anthropic, and Responses clients.

## What the upstream actually serves

CommandCode GOAT is reached through its OpenAI-compatible provider surface:

| Upstream route | Status |
|---|---|
| `POST {base-url}/chat/completions` | Serves the OSS models (`deepseek/*`, `z-ai/*`, `Qwen/*`, `MiniMaxAI/*`, `moonshotai/*`, …) |
| `GET {base-url}/models` | Model catalog |
| `POST {base-url}/messages` | Claude models only — OSS ids are rejected with *"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | Not a registered route |

The default upstream base URL is `https://api.commandcode.ai/provider/v1`.

Because there is exactly one usable route for the catalog, **every model discovered from `/models` is routed to chat-completions by default**, whatever its family or vendor prefix. `route-overrides` exist for upstreams that really do serve another protocol on another endpoint.

## The Problem

Without this plugin, using a CommandCode GOAT plan in CLIProxyAPI requires hand-written provider blocks per protocol family, and the pieces that CLI clients depend on are missing or wrong:

- **Duplicated configuration & keys**: the same keys must be configured in several provider blocks.
- **Fragmented scheduling**: rotation, rate limits, and cooldowns cannot be shared, and the Management Center cannot show per-key usage.
- **Client protocol burden**: clients must know which upstream endpoint a model requires.
- **Lost reasoning**: CommandCode returns thinking text under `reasoning` / `reasoning_details[].text` (never the standard `reasoning_content`), so a plain OpenAI-compatible provider silently drops every chain-of-thought for Anthropic and Responses clients.
- **Rejected reasoning effort**: CommandCode's `/models` carries no thinking metadata, so a provider that validates `reasoning_effort` locally rejects `xhigh`/`max` even though the upstream accepts them.

## The Solution

- **Auth files are the only key source**: accounts are CLIProxyAPI auth files, not plugin `api-keys`. In Management Center → **CommandCode** → **CommandCode accounts and quota**, paste an API key and save it. You do not need to write JSON yourself; uploading an auth file still works. Disable and delete stay on the host auth-file page. Rotation, retries, cooldowns, session affinity, and per-account statistics stay with the host scheduler.
- **Transparent translation & routing**: clients ask for `commandcode/<upstream-id>` and never need to know the upstream protocol.
- **Reasoning fidelity**: upstream thinking text is preserved for all three client formats, and `reasoning_effort` is forwarded verbatim when the model declares no capability.
- **One model catalog**: everything published by `{base-url}/models` is discoverable under the `commandcode/` prefix in `/v1/models`.

## Features

- **Single Provider Namespace**: models appear as `commandcode/deepseek/deepseek-v4.1-flash`, `commandcode/z-ai/glm-5.3-flash`, … (prefix configurable, can be disabled for bare upstream ids).
- **Multi-Protocol Client Translation**: OpenAI Chat Completions, Anthropic Messages, and OpenAI Responses requests all become upstream chat-completions calls, and responses are converted back — including streaming.
- **Reasoning Preservation**: upstream `reasoning`, `reasoning_details[].text`, and `reasoning_content` are normalized across targets:
  - Claude clients get a leading `thinking` block with `thinking_delta` events;
  - Responses clients get a leading `reasoning` output item with `response.reasoning_summary_*` events;
  - OpenAI clients get `reasoning_content` backfilled onto each chunk while the vendor fields stay intact.
- **Capability-aware Reasoning Controls**: an explicit `route-overrides` declaration (or catalog thinking metadata) re-enables eager validation and clamping; with nothing declared, the client's `reasoning_effort` is forwarded verbatim (`auto`/`none` omit the field) and the upstream is the authority.
- **Dynamic Catalog Discovery**: remote catalog with local fallback, deduplication, and diagnostics for what was excluded. Discovery runs only when at least one account is enabled.
- **CommandCode accounts and quota**: Management Center menu **CommandCode**, page **CommandCode accounts and quota** (CommandCode账户与额度). Paste an API key and save it as an auth file, then see each account's email, plan, remaining plan credits, and the rolling 5-hour/weekly windows (used, cap, reset time). Each card refreshes on demand. The plugin can start with no accounts.
- **Host auth scheduling**: enabled auth-file accounts are shared across protocols by CLIProxyAPI's scheduler. Plugin config does not hold a key pool, and a retired `api-keys` block is not imported into one.

## Requirements

- **CLIProxyAPI**: `v7.2.138+`
- **Go Toolchain**: Go 1.26+ (CGO enabled for `-buildmode=c-shared`)

## v0.1.2 upgrade and release notes

- Existing auth files remain usable. Plugin `api-keys` config is retired and is not imported. Add accounts in **CommandCode → CommandCode accounts and quota**, or upload auth files; disable and delete remain on the host auth-file page.
- For a manual upgrade, replace the library for the target OS/architecture, restart CLIProxyAPI, then refresh Management Center. Updating this repository's version does not update an already running installation.
- For [CPA plugin-store publication](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements), use the GitHub release tag `v0.1.2`. Each supported platform needs `commandcode-go-cliproxyapi_0.1.2_<goos>_<goarch>.zip`, plus the release-wide `checksums.txt` in SHA-256 format. The existing build workflow generates these assets when a version tag is pushed.
- Each ZIP must contain exactly one matching library at its root: `commandcode-go-cliproxyapi.so`, `commandcode-go-cliproxyapi.dylib`, or `commandcode-go-cliproxyapi.dll`. Keep the plugin ID and repository URL unchanged; the store reads the version from the latest GitHub release.

## Build

```bash
# Linux (AMD64)
go build -buildmode=c-shared -o plugins/linux/amd64/commandcode-go-cliproxyapi.so .

# macOS (ARM64)
go build -buildmode=c-shared -o plugins/darwin/arm64/commandcode-go-cliproxyapi.dylib .

# Windows (AMD64)
go build -buildmode=c-shared -o plugins/windows/amd64/commandcode-go-cliproxyapi.dll .
```

Place the artifact into the host's plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Configuration

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  configs:
    commandcode-go-cliproxyapi:
      # Upstream base URL (default: "https://api.commandcode.ai/provider/v1")
      base-url: "https://api.commandcode.ai/provider/v1"

      # Optional catalog override (default: "{base-url}/models")
      # catalog-url: "https://api.commandcode.ai/provider/v1/models"

      # Client-facing model id prefix
      model-prefix:
        enabled: true              # true -> "commandcode/<model>" (default: true)
        value: "commandcode"

      # Accounts are CLIProxyAPI auth files, not keys in this file.
      # Management Center → CommandCode → CommandCode accounts and quota.

      # Catalog discovery
      catalog:
        refresh-interval: "15m"           # minimum "1m"
        stale-while-unavailable: true

      # Protocol switches: disabling one removes every model routed to it
      protocols:
        chat-completions: true
        messages: true
        responses: true

      # Per-model route pins; only needed when the upstream serves another
      # endpoint (CommandCode itself serves OSS models on chat-completions)
      route-overrides:
        "claude-sonnet-5":
          protocol: "messages"            # chat-completions | messages | responses
          endpoint: "/v1/messages"        # required, must start with "/"

      request-timeout: "5m"
      max-response-bytes: 67108864        # 64 MiB
      allow-http: false                   # http:// base-url for local testing
```

### Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | — | ignored | Retired. Not a key source. See [Accounts](#accounts). |
| `base-url` | `string` | `https://api.commandcode.ai/provider/v1` | Upstream provider base URL. Valid HTTPS (or HTTP with `allow-http: true`), no query, fragment, or userinfo. |
| `catalog-url` | `string` | `{base-url}/models` | Catalog discovery URL. |
| `model-prefix.enabled` | `bool` | `true` | Client-facing ids use `<prefix>/<model>`; `false` publishes bare upstream ids. |
| `model-prefix.value` | `string` | `commandcode` | Provider prefix. |
| `catalog.refresh-interval` | `duration` | `15m` | Catalog polling cadence (minimum `1m`). |
| `catalog.stale-while-unavailable` | `bool` | `true` | Keep serving the last good snapshot when a refresh fails. |
| `protocols.*` | `bool` | `true` | Route kill switches. A disabled protocol excludes its models with a diagnostic. |
| `route-overrides` | `map` | `{}` | `{ model: { protocol, endpoint } }` pins a model onto another upstream route. `endpoint` is required. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP timeout (also bounds account/quota calls to 30s). |
| `max-response-bytes` | `int64` | `67108864` | Maximum non-streaming response body size. |
| `allow-http` | `bool` | `false` | Permit `http://` upstreams for local testing. |

### Accounts

API keys are not configured in this YAML. CLIProxyAPI auth files are the only source.

- **Add**: Management Center → **CommandCode** → **CommandCode accounts and quota** (CommandCode账户与额度). Paste an API key and save. You do not need to upload a hand-written JSON file; uploading an auth file yourself still works.
- **Disable / delete**: still on the host auth-file page, not on this page.
- **No accounts**: the plugin can start. Catalog discovery runs only when at least one account is enabled, so this provider publishes nothing until then.
- **Old `api-keys`**: an existing install may keep the block. The accounts page warns that it is ignored. It is not imported, not saved, and not used for routing, catalog discovery, or quota, and it cannot bring back an account you deleted. After you confirm the auth files are the accounts you want, remove the block yourself.

### Quota page

The **CommandCode accounts and quota** page (Management Center → **CommandCode**) reads the account surface on the same authority as `base-url`:

| Endpoint | Purpose |
|---|---|
| `GET {authority}/alpha/billing/credits` | Remaining plan credits and the 5-hour/weekly windows |
| `GET {authority}/alpha/billing/subscriptions` | Plan id/status |
| `GET {authority}/alpha/whoami?limits=1` | Account email for the card label |

`{authority}` is derived from `base-url` by trimming its provider path (`/provider/v1`). Cards are host auth files, not `api-keys` entries. Each card is refreshed manually and independently; the page never polls, and quota values never influence routing.

**Known email-sync limitation**: after discovering an email, the plugin attempts to update the original auth file so both pages can display it. It checks for prior deletion or changes, but the final check and file replacement are not atomic with host edits. Concurrent deletion, disabling, or metadata edits can be overwritten, and a deleted file can be recreated. Keeping email on both pages retains this risk. Wait for quota refreshes to finish before editing auth files, then reload the list to confirm the result.

### Reasoning effort

CommandCode publishes **no capability API**: `{base-url}/models` returns only `id`, `object`, `created`, `owned_by`, `name` and `context_length`, and its `/alpha/*` account surface has no models or capabilities route. The per-model effort lists that exist live inside the vendor's own clients (the `command-code` CLI and the web app both ship a static table), and the upstream gateway itself accepts every value in the union `low | medium | high | xhigh | max` regardless of the per-model list.

Consequences for this plugin:

- with nothing declared in the catalog, `reasoning_effort` is forwarded verbatim (`auto`/`none` omit the field) so `xhigh` and `max` work;
- if a catalog entry ever carries a `thinking` object (`levels`, `min`, `max`, `zero_allowed`, `dynamic_allowed`), that declaration takes over and eager validation plus budget clamping are re-enabled;
- the vendor's per-model table, the probes behind these statements and the evidence for the missing capability API are recorded in [`docs/model-capabilities.md`](docs/model-capabilities.md).

## Testing

```bash
go test ./...        # unit + mocked end-to-end tests
go test ./... -cover # with coverage
go vet ./...         # vetting
```

The optional browser regression test covers adding accounts, keeping API keys out of browser storage, deleted/stale responses, disabled state, and mobile/dark layouts. It uses local mock endpoints only:

```bash
npx --yes playwright install chromium
npx --yes -p playwright -c 'NODE_PATH=$(dirname $(dirname $(command -v playwright))) node tests/accounts_page.cjs'
```

Alternatively, set `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to an existing Chrome executable and skip the browser download.

## Provenance

This plugin was built with reference to two prior CommandCode plugins:

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) (v0.1.7) is the direct ancestor: the adapter kernel, catalog, config, auth, and quota scaffolding come from there, and the CommandCode-specific behaviour — reasoning preservation, unattributed-capability effort passthrough, the chat-completions-only route default, and the account API used by the quota page — was adapted on top.
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode) (ahoo) is an independent earlier CommandCode plugin whose write-up established the vendor behaviours this plugin had to reproduce: thinking text arriving under `reasoning` / `reasoning_details[].text` and never `reasoning_content`, the resulting need to backfill `reasoning_content` before CLIProxyAPI's openai→claude translator sees it, the SSE `data: ` prefix that streaming `/v1/messages` requires, and the fact that only fully-qualified vendor names (`deepseek/deepseek-v4.1-flash`) are accepted upstream.

Repository: <https://github.com/mczhoucn/commandcode-go-cliproxyapi>

## License

[MIT](LICENSE)
