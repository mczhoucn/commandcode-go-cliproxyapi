# CommandCode GOAT / Pro / Max CLIProxyAPI Plugin

**English** | [简体中文](README.zh-CN.md)

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes a **CommandCode GOAT / Pro / Max** plan as a single provider (`commandcode`; published model ids carry the `commandcode/` prefix).

The plugin owns model discovery, protocol translation, execution, and the CommandCode accounts and quota page. API keys come only from CLIProxyAPI auth files, so those accounts serve OpenAI, Anthropic, and Responses clients.

> **Supports CommandCode GOAT / Pro / Max, not the CommandCode Go plan.** Go uses the official CLI's private, non-OpenAI-compatible interface and protocol rather than the Provider API used by this plugin, and has no Provider API access. CPA's current CommandCode integration does not implement that private protocol; choosing a different login method does not enable Go.
>
> **The `go` in the name does not mean Go-plan support.** The actual plugin name is `commandcode-go-cliproxyapi`, inherited from [`opencode-go-cliproxyapi`](https://github.com/massiveits/opencode-go-cliproxyapi). The plugin ID is unchanged.

See the [ChangeLog](CHANGELOG.md) for this update and [release notes](RELEASE_NOTES.md) for detailed history.

## What the upstream actually serves

CommandCode GOAT / Pro / Max is reached through its OpenAI-compatible provider surface:

| Upstream route | Status |
|---|---|
| `POST {base-url}/chat/completions` | Serves the OSS models (`deepseek/*`, `z-ai/*`, `Qwen/*`, `MiniMaxAI/*`, `moonshotai/*`, …) |
| `GET {base-url}/models` | Model catalog |
| `POST {base-url}/messages` | Claude models only — OSS ids are rejected with *"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | Now available for most OpenAI and open models, but not every model; check each model's `supported_endpoints` |

The default upstream base URL is `https://api.commandcode.ai/provider/v1`.

When this plugin was created, CommandCode did not yet provide OpenAI Responses. The official API now supports it for most OpenAI and open models. However, the public catalog still lists only Chat Completions for the following models at this release-preparation snapshot (2026-10-20):

| Model | Upstream model ID | Currently declared endpoint |
|---|---|---|
| GLM-5.3 Flash | `z-ai/glm-5.3-flash` | `/chat/completions` |
| Tencent Hy4 Preview | `tencent/hy4-preview` | `/chat/completions` |
| DeepSeek V4 Flash Fast | `deepseek/deepseek-v4-flash-fast` | `/chat/completions` |

The plugin therefore **retains translation to OpenAI Chat Completions as its default path**, allowing Responses clients to use models that lack native Responses support. This is not a Responses-first retry/fallback mechanism, and the plugin does not automatically switch routes based on `supported_endpoints`. Use explicit `route-overrides` only after confirming that a model supports the chosen Messages or Responses endpoint.

This is a release-preparation catalog snapshot, not a permanent restriction or a per-model inference test. Check the [official Provider API documentation](https://commandcode.ai/docs/provider) and the live [model catalog](https://api.commandcode.ai/provider/v1/models) for current `supported_endpoints`.

## The Problem

Without this plugin, a manual CommandCode GOAT / Pro / Max integration must manage provider configuration, authentication and endpoint choices. Splitting it into provider blocks per protocol can duplicate configuration and fragment scheduling; vendor-specific reasoning fields and capability declarations also need adaptation:

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
- **Multi-Protocol Client Translation**: by default, OpenAI Chat Completions, Anthropic Messages, and OpenAI Responses requests are translated into upstream OpenAI Chat Completions calls, with responses converted back, including streaming. This path remains for models without native Responses support, despite broader official Responses availability. Explicit `route-overrides` can select another verified upstream endpoint.
- **Reasoning Preservation**: upstream `reasoning`, `reasoning_details[].text`, and `reasoning_content` are normalized across targets:
  - Claude clients get a leading `thinking` block with `thinking_delta` events;
  - Responses clients get a leading `reasoning` output item with `response.reasoning_summary_*` events;
  - OpenAI clients get `reasoning_content` backfilled onto each chunk while the vendor fields stay intact.
- **Capability-aware Reasoning Controls**: valid catalog `thinking` metadata supplies capability checks and budget bounds to the relevant conversion paths. `route-overrides` only selects a protocol and endpoint; it cannot declare thinking capabilities. Without a declaration, upstream acceptance is authoritative. `auto`/`none` are not universally omitted; see Reasoning effort below.
- **Dynamic Catalog Discovery**: remote catalog with local fallback, deduplication, and diagnostics for what was excluded. Discovery runs only when at least one account is enabled.
- **CommandCode accounts and quota**: Management Center menu **CommandCode**, page **CommandCode accounts and quota** (CommandCode账户与额度). Paste an API key and save it as an auth file, then see each account's email, plan, remaining plan credits, and the rolling 5-hour/weekly windows (used, cap, reset time). Each card refreshes on demand. The plugin can start with no accounts.
- **Host auth scheduling**: enabled auth-file accounts are shared across protocols by CLIProxyAPI's scheduler. Plugin config does not hold a key pool, and a retired `api-keys` block is not imported into one.

## Requirements

- **CLIProxyAPI**: `v7.2.138+`
- **Go Toolchain**: Go 1.26+ (CGO enabled for `-buildmode=c-shared`)

## v0.1.3 upgrade and release notes

- This patch fixes missing completion events for streamed Responses tool calls, preventing clients from treating tools as unfinished after the response ends; both Chat Completions and Messages upstream conversion paths are covered.
- Existing auth files remain usable. Plugin `api-keys` config is retired and is not imported. Add an existing key in **CommandCode → CommandCode accounts and quota**, or use **OAuth login → CommandCode** without manually adding keys to configuration. Auth-file uploads remain available; disable and delete stay on the host auth-file page.
- For a manual upgrade, replace the library for the target OS/architecture, restart CLIProxyAPI, then refresh Management Center. Updating this repository's version does not update an already running installation.
- For [CPA plugin-store publication](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements), use the GitHub release tag `v0.1.3`. Each supported platform needs `commandcode-go-cliproxyapi_0.1.3_<goos>_<goarch>.zip`, plus the release-wide `checksums.txt` in SHA-256 format. The existing build workflow generates these assets when a version tag is pushed.
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
      # Receives your API key: use a trusted URL, preferably the same scheme/host/port as base-url.
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
      # endpoint (chat-completions remains the default; verify before overriding)
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
| `catalog-url` | `string` | `{base-url}/models` | Catalog URL; receives the enabled account's bearer API key even for public catalogs. Use only a trusted destination, preferably the same scheme/host/port as `base-url`; same-origin is not enforced by configuration validation. |
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

- **Manual API key**: Management Center → **CommandCode** → **CommandCode accounts and quota**. Paste an existing key; the plugin verifies identity and active subscription before saving. Host JSON uploads remain available and are not covered by these admission checks.
- **Browser authorization**: CPA → **OAuth login** → **CommandCode** → Add. Open the official link from the plugin guide, sign in and authorize, copy the generated key from the official fallback page, and paste it into the guide. No manually pre-created key is required. This is manual credential completion, not a callback-URL copy or automatic callback.
- **Both methods coexist with the same capabilities**: they produce the same kind of `type: commandcode` / `api_key` auth file and share identity/subscription checks, model execution, and quota queries. OAuth-added accounts have no extra capabilities; only the `credential_source` label differs. Identical keys are still deduplicated; different keys may coexist.
- Both entry points require successful identity verification and an active, unexpired GOAT / Pro / Max subscription: exact plan IDs `individual-goat`, `individual-pro`, `individual-pro-v1`, or `individual-max`, with status `active`. Go, unknown plans/statuses, invalid credentials and unverifiable responses do not create files. Existing files are not migrated or deleted.
- Identical keys, including custom-named files, are rejected without intentionally changing disabled state or notes. Different keys for one account may coexist, but do not create additional subscription quota. Source labels identify only the entry point, not an officially verified binding to that browser authorization.
- **Disable / delete**: still on the host auth-file page, not on this page.
- **No accounts**: the plugin can start. Catalog discovery runs only when at least one account is enabled, so this provider publishes nothing until then.
- **Old `api-keys`**: an existing install may keep the block. The accounts page warns that it is ignored. It is not imported, not saved, and not used for routing, catalog discovery, or quota, and it cannot bring back an account you deleted. After you confirm the auth files are the accounts you want, remove the block yourself.

### Browser authorization requirements and limitations

- Sign in to the **same CPA origin** with **Remember password** enabled. The guide reuses existing management authentication; it does not put the management password in URLs or create another password store.
- The user verified the official fallback in Chrome using `http://localhost:6000/callback`, with no `mode=redirect`. Chrome blocks that port by default, triggering the fallback. Do not override the port restriction. Other browsers are unverified. No listener or port forwarding is needed.
- **A brief callback error is an expected transition**: the official site returns credentials through HTTP POST (JSON or form), not a token/API key in a callback HTTP GET URL or address bar. The chosen Chrome-blocked callback intentionally triggers the official failed-delivery fallback. After authorization, a callback-URL or credential-delivery error may briefly appear before the official credential-copy page opens. This is by design; do not authorize again solely because of that brief message. **If no fallback page appears, the credential cannot be copied, or CPA validation/save fails, treat that as a real failure rather than ignoring it.**
- The official flow may create a key. That API key remains the underlying credential: no independent access/refresh tokens or automatic renewal are provided. The official site's temporary storage and clipboard may contain it; the plugin adds no browser key storage, key-bearing URLs, or key logs.
- Login sessions last 10 minutes. Completion validates and stages; CPA polling owns the only save. The guide reports success only after CPA confirms and a matching auth file is found. The guide can keep polling after the original OAuth window closes.
- **Cancellation before delivery stops it; after delivery, cancellation cannot guarantee that CPA will not save.** Failed or uncertain saves are not automatically redelivered. Check auth files first. Unresolved saves retain an in-memory digest claim across these two entry points, not an atomic guarantee across host uploads/edits, restart, or multiple instances.
- **Accepted network limitation**: validation uses CPA's existing proxy/TLS/redirect policy and trusted plugin configuration, never a browser-supplied target. The host offers no plugin redirect opt-out; some redirects, such as a subdomain or another port on the same host, may retain the authorization header. This is accepted, not fixed; strict cross-origin prevention is not claimed.

### Quota page

Quota is displayed on the plugin's own page. The standard quota capability used by CPA auth-file cards (`QuotaProvider`) is not registered; adding an account through OAuth does not automatically add quota information to those cards.
The **CommandCode accounts and quota** page (Management Center → **CommandCode**) reads the account surface on the same authority as `base-url`:

| Endpoint | Purpose |
|---|---|
| `GET {authority}/alpha/billing/credits` | Remaining plan credits and the 5-hour/weekly windows |
| `GET {authority}/alpha/billing/subscriptions` | Plan id/status |
| `GET {authority}/alpha/whoami?limits=1` | Account email for the card label |

`{authority}` is derived from `base-url` by trimming its provider path (`/provider/v1`). Cards are host auth files, not `api-keys` entries. Loading the list may automatically query accounts missing an email to fill that information; otherwise refresh is manual and per card. There is no periodic quota polling, and quota values never influence routing.

**Known email-sync limitation**: after discovering an email, the plugin attempts to update the original auth file so both pages can display it. It checks for prior deletion or changes, but the final check and file replacement are not atomic with host edits. Concurrent deletion, disabling, or metadata edits can be overwritten, and a deleted file can be recreated. Keeping email on both pages retains this risk. Wait for quota refreshes to finish before editing auth files, then reload the list to confirm the result.

### Reasoning effort

No unified thinking/effort capability API has been confirmed for this plugin. The historical catalog contained only `id`, `object`, `created`, `owned_by`, `name`, and `context_length`; the public catalog checked on 2026-10-20 also includes `supported_endpoints`. That field declares API routes, not reasoning-effort levels. The bundled vendor effort tables and gateway acceptance of `low | medium | high | xhigh | max` were checked on 2026-09-17; they are historical evidence, not guarantees of current acceptance or effect.

Consequences for this plugin:

- without declared capabilities, the default Chat Completions route forwards supplied effort choices rather than locally rejecting `xhigh` / `max` merely for missing metadata. Native Chat Completions input and Responses-to-Chat conversion retain supplied `auto` / `none`; these values do not guarantee field omission. Omit the input effort setting if no effort field should be sent. Upstream acceptance and effect remain authoritative;
- if a catalog entry carries a valid `thinking` object (`levels`, `min`, `max`, `zero_allowed`, `dynamic_allowed`), the relevant conversion paths use it for capability checks and budget bounds. A route override does not supply that metadata;
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

OAuth guide fixtures (also local-only):

```bash
npx --yes -p playwright -c 'NODE_PATH=$(dirname $(dirname $(command -v playwright))) node tests/oauth_page.cjs'
```

Native host integration creates its own temporary config, mock upstream and auth directory. It needs local native binaries, not a running CPA:

```bash
CPA_HOST_BINARY=/absolute/path/to/cli-proxy-api \
CPA_PLUGIN_LIBRARY=/absolute/path/to/commandcode-go-cliproxyapi.dylib \
node tests/oauth_host.cjs
```

Set `CPA_BROWSER=1` and provide Playwright as above to drive the real guide too. Use `.so` on Linux or `.dylib` on macOS.

Alternatively, set `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to an existing Chrome executable and skip the browser download.

## Provenance

This plugin was built with reference to two prior CommandCode plugins:

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) (v0.1.7) is the direct ancestor: the adapter kernel, catalog, config, auth, and quota scaffolding come from there, and the CommandCode-specific behaviour — reasoning preservation, unattributed-capability effort passthrough, the chat-completions-only route default, and the account API used by the quota page — was adapted on top.
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode) (ahoo) is an independent earlier CommandCode plugin whose write-up established the vendor behaviours this plugin had to reproduce: thinking text arriving under `reasoning` / `reasoning_details[].text` and never `reasoning_content`, the resulting need to backfill `reasoning_content` before CLIProxyAPI's openai→claude translator sees it, the SSE `data: ` prefix that streaming `/v1/messages` requires, and the fact that only fully-qualified vendor names (`deepseek/deepseek-v4.1-flash`) are accepted upstream.

Repository: <https://github.com/mczhoucn/commandcode-go-cliproxyapi>

## License

[MIT](LICENSE)
