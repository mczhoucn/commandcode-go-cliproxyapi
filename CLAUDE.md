# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repository is

A **native dynamic plugin for CLIProxyAPI (CPA)**, not a standalone server. It exposes the CommandCode Provider API (`https://api.commandcode.ai/provider/v1`) as one CPA provider, `commandcode`, so OpenAI, Anthropic Messages, and OpenAI Responses clients can all use the same upstream accounts. Build output is a C-shared library (`.so`/`.dylib`/`.dll`) that CPA loads; `main.go` is the only file importing `"C"`.

Supported plans are GOAT / Pro / Max. The `go` in `commandcode-go-cliproxyapi` is inherited naming and does **not** mean CommandCode Go support.

## Commands

Go 1.26+ and CGO are required for the artifact; unit tests do not need CGO.

```bash
# Tests (unit + mocked end-to-end; internal/plugin takes ~40s)
go test ./...
go test ./... -cover
go test ./internal/plugin -run TestRegisterSuccessPublishesModels -v   # single test
go test ./internal/plugin -run TestQuota -v                            # quota surfaces (page + provider)
go vet ./...

# Shared-library build (requires CGO and a C compiler; the README lists per-OS outputs)
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/<os>/<arch>/commandcode-go-cliproxyapi.<ext> .

# Release packaging used by CI (zip contains exactly one library at its root, plus checksums)
go run ./.github/scripts/package-release.go -library <lib> -archive <zip> -checksum <sha256-file>
```

Gotchas:
- `go build .` and `go vet .` fail with "build constraints exclude all Go files" when CGO is disabled (the root `main.go` imports `"C"`). `go build ./...`, `go test ./...`, and `go vet ./...` succeed because they skip the root package when it has no buildable files. With CGO enabled but no C compiler, `-buildmode=c-shared` fails with `cgo: C compiler "gcc" not found`.
- `tests/` is excluded from normal builds (`//go:build live`), so `go list ./tests` reports "build constraints exclude all Go files". Compile-check it with `go vet -tags live ./tests`.
- Debug tracing: build with `-tags debug` to enable `internal/plugin/debug_dev.go`, which appends secret-free control-flow records to `commandcode-debug.log` next to the host executable. Release builds use the no-op stub in `debug_release.go`.
- If `proxy.golang.org` is unreachable, prefix commands with `GOPROXY=https://goproxy.cn,direct`; the module cache under `~/go/pkg/mod` is then populated normally.

Optional tests (local mocks only; see README for full invocation):

```bash
# Live CPA smoke tests, require a running host
CPA_HOST=http://localhost:8317 CPA_KEY=... go test -tags live ./tests -run TestLiveSmokeSuite -v

# Native host integration (temporary config/mock upstream/auth dir)
CPA_HOST_BINARY=/abs/cli-proxy-api CPA_PLUGIN_LIBRARY=/abs/plugin.<ext> node tests/oauth_host.cjs

# Browser regression tests (Playwright)
npx --yes -p playwright -c 'NODE_PATH=... node tests/accounts_page.cjs'
npx --yes -p playwright -c 'NODE_PATH=... node tests/oauth_page.cjs'
```

## Architecture

Layering, from the host inward:

```
CPA host process
  │  C ABI: cliproxy_plugin_init / cliproxyPluginCall / cliproxyPluginFree / cliproxyPluginShutdown
  ▼
main.go                      CGO glue only; forwards method+JSON to Manager.HandleCall
  ▼
internal/plugin/plugin.go    Manager: method dispatch, register/reconfigure/shutdown,
                             catalog refresh loop, model publication
  ├── hostbridge.go          HostBridge: the ONLY outbound network path (host callbacks)
  ├── executor.go            execute / execute_stream: resolve, translate, call, pump
  ├── quota_provider.go      standard QuotaProvider surface (quota.* methods)
  ├── auth.go, credential.go, oauth.go, account_email.go, quota.go
  ▼
internal/catalog             catalog fetch, normalization, route assignment, snapshot
internal/adapter/chatcompletions|messages|responses
                             request, non-stream response, and SSE stream translation
internal/adapter/shared      shared wire kernels (model rewrite, content parts,
                             reasoning normalization, SSE framers/emitters, usage)
internal/thinking            budget <-> effort mapping with capability clamping
internal/errclass            error classes, upstream parsing, cooldown scope, redaction
internal/config              YAML load/validate/defaults
resources                    embedded quota_page.html / oauth_page.html
```

All outbound HTTP must go through `HostBridge` (`host.http.do`, `host.http.do_stream`, `host.http.stream_read`, `host.http.stream_close`, `host.stream.emit`, `host.stream.close`, `host.log`, `host.auth.*`). There is no `http.Client` anywhere in the plugin. The only production filesystem access is `account_email.go`, which rewrites the selected auth file to persist a discovered email (best effort, non-atomic with host edits); the other `os.*` calls are behind the `debug` build tag.

### Execution flow

1. CPA selects an auth file and invokes `executor.execute` or `executor.execute_stream` with `SourceFormat` (`openai` | `claude` | `openai-response`), the original client body, and the selected credential.
2. `resolveExecution` rejects a non-`commandcode` provider, a missing API key, or a model absent from the routable snapshot.
3. `resolveCommandCodeSessionID` derives the `x-commandcode-session` header: `canonical_session_id` metadata, then a fixed header precedence list, then a SHA-256 of the first user turn's model-visible content.
4. `buildUpstreamRequest` picks the adapter for the record's route and rewrites the model to the bare upstream ID.
5. The request goes through `HostBridge.Do` (non-stream) or `DoStream` (stream). Stream execution returns immediately after 200 OK; `pumpStream` runs in a background goroutine, translating chunks and emitting them downstream until a terminal event or failure, and joins the bridge's in-flight accounting.
6. Responses are translated back to the client's format; errors become classified envelopes.

### Routing

- Every catalog model defaults to `chat-completions` (`catalog.DefaultRoute`). Messages/Responses are opt-in per model via `route-overrides`; there is no capability-based auto-routing.
- `catalog.JoinUpstreamURL` drops a leading `/v1` from the endpoint because `base-url` already carries the version segment. `endpointEscapesBase` rejects override endpoints that would leave the configured authority.
- `catalog.Manager.Lookup` resolves both the prefixed public ID and the bare upstream ID.
- Protocol kill switches (`protocols.*`) are applied when the snapshot is built, so they take effect at the next successful refresh.

### Lifecycle and concurrency invariants

- `Manager.lifeMu` serializes whole register / reconfigure / shutdown sequences and all login/claim mutation; `Manager.mu` guards the config/catalog snapshot. Never wait on a tick loop while holding `m.mu`.
- Reconfigure stops the old ticker and waits for exit, then adopts the new manager unconditionally. If the refresh failed and `catalog.stale-while-unavailable` is on, the new manager re-seeds records from the old snapshot's raw entries rebuilt under the new config.
- Registration does one bounded refresh (`registerRefreshTimeout`, 10s) so a slow catalog cannot block host startup. Discovery needs at least one enabled `commandcode` auth file; with none, the provider publishes no models, and `handleModelsForAuth` retries on demand when a key is added.
- Shutdown waits (`shutdownDrainTimeout`, 15s) for all host-callback goroutines because Unix loaders free the host API and dlclose the plugin as soon as the export returns. Any new callback path must join `HostBridge.inFlight` through `invoke`/`callWithTimeoutResult`.
- Stream IDs are direction-specific: IDs from `DoStream` are **upstream**; `executorRequest.StreamID` is the **downstream** ID. Never interchange them.
- Emit and log callbacks have their own deadlines (`emitTimeout`/`logTimeout`) so a wedged host consumer surfaces as an error instead of parking a producer forever.

### Auth model

- Auth files are the only credential source. The old `api-keys` config block is detected only to warn that it is ignored; it is never imported or used.
- Manual entry (`POST .../accounts`) and the OAuth guide both funnel through `prepareCredentialLocked`: reject whitespace/control chars, reject existing keys (by key digest or file name), then verify `alpha/whoami` and `alpha/billing/subscriptions` (plan must be `individual-goat`, `individual-pro`, `individual-pro-v1`, or `individual-max`, `active`, and unexpired).
- Files are named `commandcode-key-<sha256>.json`; the digest alone derives both the runtime ID and file name. Unresolved saves are tracked as in-memory digest claims, not atomic guarantees.
- The "OAuth" flow is a browser-authorization **guide** that ends with a pasted API key; there are no refresh tokens or automatic renewal. Login sessions last 10 minutes and only CPA performs the save. `oauth.go` and the `resources/oauth_page.html` state machine encode this; changes risk duplicating auth files or claiming success before a file exists.

### Reasoning, adapters, and errors

- Upstream sends thinking text under `reasoning` / `reasoning_details[].text`, never `reasoning_content`. `shared.BackfillReasoningContent` mirrors it for OpenAI clients; Claude targets get `thinking` blocks; Responses targets get reasoning summary items.
- `internal/thinking` maps budget↔effort using tables copied from the host SDK (`internal/thinking/convert.go`). They were taken at v7.2.138 and are unchanged in v8.0.67; re-check on any further SDK bump. When a model declares no thinking capability (all CommandCode models today), client efforts are forwarded verbatim rather than locally rejected.
- Adapters share kernels in `shared` deliberately: identically shaped requests must produce identical policies on all routes (e.g. empty content dropped, tool results text-only, `fc_`-prefixed Responses item IDs, `total_tokens = input + output`). `responses_parity_test.go` and the many `F5`/parity comments pin these cross-route invariants.
- `errclass` classes are the envelope error codes; all messages pass through `Redact` (bearer, `x-api-key`, query-string keys), and post-first-byte network failures are never retryable. Do not add a code path that forwards an upstream body without redaction.

### Management UI

`resources/quota_page.html` and `oauth_page.html` are embedded (`resources/embed.go`) and served from `HandleManagement`. They are plain HTML/JS that authenticate by reading the host's `cli-proxy-auth` localStorage entry, decoding both `enc::v1::` (origin + user-agent) and `enc::v2::` (origin only) XOR formats. If the host changes that storage format again, both pages need updating. Browser tests in `tests/*.cjs` exercise these pages against local mocks.

### Quota surfaces

Two surfaces read the same upstream account API through `fetchQuota` (`quota.go`):

- **Plugin page** (`quota-usage`): account list, key entry, per-card refresh; uses used/cap percentages.
- **Standard `QuotaProvider`** (`quota_provider.go`): registration advertises `quota_provider`, and `quota.identifier` / `quota.describe` / `quota.fetch` serve Management Center's auth-file cards. The host-supplied shape is **remainingFraction in [0,1]** — the inverse of the page's percentage — and `window` values (`five_hour`, `weekly`) must stay in the host's known set or the UI loses its ordering/period labels. `QuotaBucket` has no display-name field, so the row label is packed into `description`. State flags beat the arithmetic: `unlimited` renders full and `exceeded` renders empty regardless of used/cap. `quota.reset` answers unsupported because CommandCode has no reset endpoint.

Changing either surface means keeping both consistent; the plugin page never fetches through the host's quota route.

### Host SDK compatibility floor

The plugin pins `declaredSchemaVersion = 6` (`internal/plugin/plugin.go`) instead of echoing `pluginabi.SchemaVersion` (10 in v8.0.67). Hosts reject a plugin whose schema **exceeds** their own, so declaring the newest SDK value would reject every host older than v8.0.50 for features this plugin never calls. Schema 6 is what the plugin actually uses: raw management JSON bodies, plus the cooldown error fields.

| Host version | Behavior |
|---|---|
| `< v8.0.43` | No cooldown error fields / no quota provider; unsupported |
| `v8.0.43+` | Full quota surface and cooldown reporting |

Verify against the SDK before changing: check that `RetryAfterSeconds`, `CredentialScoped`, and the `ErrorCode*` constants still exist, and that the declared schema does not exceed what older hosts accept. The SDK module path is now `github.com/router-for-me/CLIProxyAPI/v8`.

### Cooldown semantics (why error classification matters)

CPA decides cooldown scope and duration from four envelope fields, so the plugin's job is to report the vendor's error faithfully — never to invent cooling policy. `errclass/upstream.go` encodes this; the shapes were recovered from the vendor CLI (`command-code 1.88.5`, `dist/cli.mjs`: `parseWindowLimitError`, `classifyKind`, `parseSpendCapError`), not guessed.

| Upstream condition | Reported as | CPA effect |
|---|---|---|
| `RATE_LIMITED` + `error.rateLimit.window` | `credential_quota`, `CredentialScoped`, `RetryAfterSeconds` from `rateLimit.reset` | Cool the whole credential until upstream reset |
| `USAGE_EXCEEDED` / credit exhaustion | `credential_quota`, `CredentialScoped` | Cool the whole credential |
| Plain 429 | `transient_rate_limit` | Cool only the model, no quota record |
| `model_not_in_plan` / not on endpoint | `model_not_found` | Cool only that model |
| Request defect (prompt too long) | `request_scoped` | No cooldown, stop rotation |
| Post-first-byte truncation | `stream_truncated` (via `error_detail` on stream close) | No cooldown, no replay |
| Client disconnect on emit | `connection_lifecycle` | No cooldown |
| Dial/TLS/DNS failure | `transient_transport` | Retryable, no cooldown |

Non-JSON upstream bodies fall back to the historical status-only classification, so proxy errors and plain-text failures keep their previous wording. `Retry-After` (delta-seconds or HTTP-date) is admitted only within `(0, 24h]`.

Verified end-to-end against a real v8.0.67 host: a window-exhausted 429 produced `scope: credential, reason: credential_quota, remaining_seconds: 3541` (matching the mock's reset instant), while a plain 429 produced `scope: model, reason: transient_rate_limit`.

## Versioning and release

- `pluginVersion` in `internal/plugin/plugin.go` is reported to the host and asserted exactly in `internal/plugin/plugin_test.go`; bump both together.
- A release also updates `README.md` / `README.zh-CN.md`, `CHANGELOG.md` (user-facing, bilingual), and `RELEASE_NOTES.md` (used as GitHub release body).
- Pushing a `v*` tag runs `.github/workflows/build.yml`: tests and vet first, then a build matrix (linux amd64/arm64, darwin amd64/arm64, windows amd64, windows arm64 via cross-cgo, freebsd amd64 non-blocking), then publishes zips plus `checksums.txt`. Each zip must contain exactly one library at its root; the plugin ID and repository URL must stay unchanged for plugin-store compatibility.

## Conventions in this codebase

- Comments cite spec identifiers (`FR-###`, `arch §`, `AC §`, `F1`–`F5`) whose source documents are **not** in this repository; treat them as stable rationale labels, not links.
- Code comments explain invariants and rejected alternatives heavily (especially around failure modes and race conditions). Match that density when changing concurrency, credentials, or protocol translation.
- User-facing strings in the management API and pages are Chinese; code comments and docs are English (with bilingual README/CHANGELOG/RELEASE_NOTES).
- Commits follow Conventional Commits with scopes such as `fix(adapter)`, `feat(auth)`, `feat(quota)`, `fix(plugin)`, `chore(release)`.
