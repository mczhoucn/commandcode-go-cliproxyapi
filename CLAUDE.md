# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repository is

A **native dynamic plugin for CLIProxyAPI (CPA)**, not a standalone server. It exposes the CommandCode Provider API (`https://api.commandcode.ai/provider/v1`) as one CPA provider, `commandcode`, so OpenAI, Anthropic Messages, and OpenAI Responses clients can all use the same upstream accounts. Build output is a C-shared library (`.so`/`.dylib`/`.dll`) that CPA loads; `main.go` is the only file importing `"C"`.

Supported plans are GOAT / Pro / Max. The plugin was renamed from `commandcode-go-cliproxyapi`; the old `go` was inherited naming from `opencode-go-cliproxyapi`, never a Go-plan support claim. **The plugin ID is part of the host-facing contract**, so a rename touches more than the package name: `pluginName` and `githubRepoURL` in `internal/plugin/plugin.go`, the `go.mod` module path, `PLUGIN_ID` in `.github/workflows/build.yml`, the route strings embedded in `resources/quota_page.html` and `oauth_page.html`, and the C-shared output filename. Auth files need no migration — their `type` is the provider key `commandcode`, which does not change. Historical CHANGELOG/RELEASE_NOTES entries keep the old name on purpose; rewriting them would falsify the release record.

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
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/<os>/<arch>/commandcode-or-or-cliproxyapi.<ext> .

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
- Shutdown waits (`shutdownDrainTimeout`, 15s) for all host-callback goroutines so none outlives the host API. (v8.0.67 closes the instance's host streams and HTTP operations before calling shutdown and never dlcloses, which is what unblocks parked emits; older loaders may unload as soon as the export returns.) Any new callback path must join `HostBridge.inFlight` through `invoke`/`callWithTimeoutResult`. `inFlight` is a counter with a timed wait, safe to `Add` after a timed-out wait — do not swap it back to a `sync.WaitGroup`.
- `executor.execute` is always unary: a `stream: true` body is stripped and sent non-stream. Only `executor.execute_stream` streams. The pump goroutine has its own `recover()`; a panic in a goroutine of a c-shared library kills the whole host process, so any new background goroutine needs one too.
- `host_callback_id` from the executor request is forwarded on `host.http.do` / `do_stream` so the host parents the upstream call on the client's request context (client disconnect cancels upstream). Omit it when absent for older hosts.
- Stream IDs are direction-specific: IDs from `DoStream` are **upstream**; `executorRequest.StreamID` is the **downstream** ID. Never interchange them.
- Emit and log callbacks have their own deadlines (`emitTimeout` 60s, `logTimeout` 5s, deliberately decoupled) so a wedged host consumer surfaces as an error instead of parking a producer forever. The host's emit queue holds 16 chunks and drains at client read speed, so a short emit deadline would abort slow-but-healthy clients.

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

### Downstream protocol coverage (DeepSeek-documented surface)

The plugin speaks the full surface described by DeepSeek's public guides, because CommandCode forwards to those models. Four details are easy to regress:

- **Historical thinking must be replayed.** A request carrying `tools` is rejected upstream if an earlier assistant turn's thinking was dropped. Claude `thinking` blocks round-trip as `reasoning_content`; a Responses `reasoning` item's plaintext `content` merges into the adjacent assistant message. `redacted_thinking` and `summary`/`encrypted_content` stay omitted — they have no plaintext to replay. The Claude decoder folds the wire `thinking` field into `ClaudeBlock.Text`; reading only `text` empties every block.
- **Effort placement differs per protocol.** Anthropic uses `output_config.effort`, Chat Completions uses `reasoning_effort`, Responses uses `reasoning.effort`. DeepSeek ignores `thinking.budget_tokens`, so an explicit effort always wins over the budget-derived level, and `none`/`auto` are switches rather than rankable strengths.
- **Image `detail`** (`low`/`high`/`original`/`auto`) is accepted both as `image_url.detail` and as a sibling `detail` on Responses `input_image`; `shared.OpenAIImageDetail` normalizes and rejects anything else.
- **Responses reasoning events are emitted under two families** — `response.reasoning_summary_text.*` (OpenAI spec) and `response.reasoning_text.*` (DeepSeek docs). They are distinct event types rather than aliases, so both must be sent for either client to work. `ReasoningSummaryDelta`/`ReasoningSummaryDone` return `[][]byte`.

Upstream remains CommandCode's OpenAI-compatible Provider API (`provider/v1`), matching both reference implementations. The vendor CLI's private `/alpha/generate` protocol (a `config`/`permissionMode`/`threadId` envelope with `tool-call`/`tool-result`/`reasoning` message blocks) is deliberately **not** implemented: it is undocumented, and using it would imply CommandCode Go plan support, which this plugin does not claim.

### Management UI

`resources/quota_page.html` and `oauth_page.html` are embedded (`resources/embed.go`) and served from `HandleManagement`. They are plain HTML/JS that authenticate by reading the host's `cli-proxy-auth` localStorage entry, decoding both `enc::v1::` (`location.host` + user-agent) and `enc::v2::` (`location.host` only) XOR formats, matching Management Center's `src/utils/encryption.ts`. If the host changes that storage format again, both pages need updating. Browser tests in `tests/*.cjs` exercise these pages against local mocks.

### Quota surfaces

Two surfaces read the same upstream account API through `fetchQuota` (`quota.go`):

- **Plugin page** (`quota-usage`): account list, key entry, per-card refresh; uses used/cap percentages.
- **Standard `QuotaProvider`** (`quota_provider.go`): registration advertises `quota_provider`, and `quota.identifier` / `quota.describe` / `quota.fetch` serve Management Center's auth-file cards. The host-supplied shape is **remainingFraction in [0,1]** — the inverse of the page's percentage — and `window` values (`five_hour`, `weekly`, `monthly`) must stay in the host's known set or the UI loses its ordering/period labels. `QuotaBucket` has no display-name field, so the row label is packed into `description`. State flags beat the arithmetic: `unlimited` renders full and `exceeded` renders empty regardless of used/cap. `quota.reset` answers unsupported because CommandCode has no reset endpoint.

Changing either surface means keeping both consistent; the plugin page never fetches through the host's quota route.

**Window hierarchy.** The three windows nest (`month` > `weekly` > `five_hour`): the shorter is carved out of the longer, so a spent longer window forces the shorter ones to zero even when upstream reports headroom (`applyWindowHierarchy` in `quota.go`, applied inside `fetchQuota` so both surfaces agree). Propagation is downward only. `exhausted()` treats `unlimited` and zero-cap windows as never exhausted, and counts `used >= cap` as spent even without an upstream flag.

**An exhausted window is never a fetch error.** The host renders any `quota.fetch` error as "failed to fetch quota" and drops the whole card, which hides the very window data that explains the problem. So `quotaResponseFromUsage` returns the full snapshot — buckets already folded to 100% — plus one `Summary` metric whose label names the spent window (`quotaExhaustedLabel`). `quota.fetch` therefore only fails for genuine retrieval problems.

That error path could not have affected routing anyway: `FetchCredentialQuota` is a read-only management endpoint. CPA stops routing to a credential through executor cooldown reporting (see below), not through the quota surface. The plugin API exposes no RPC to set a cooldown or mark a credential unavailable; only `scheduler.pick` could influence selection, and that is a global scheduler, too invasive for this provider.

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
| `RATE_LIMITED`, any `error.rateLimit`, or usage-window wording (5-hour / weekly / monthly) | `credential_quota`, `CredentialScoped`, HTTP **429**, `RetryAfterSeconds` from `rateLimit.reset` or the message's "resets at / resets in" text | Cool the whole credential until upstream reset |
| `USAGE_EXCEEDED` / credit / spend-cap exhaustion (upstream 402/403 included) | `credential_quota`, `CredentialScoped`, HTTP **429** | Cool the whole credential |
| Plain or codeless 429 | `transient_rate_limit` | Cool only the model, no quota record |
| `model_not_in_plan` / not on endpoint | `model_not_found` | Cool only that model |
| Request defect (prompt too long) | `request_scoped` | No cooldown, stop rotation |
| In-stream classified error (quota, auth, model) | forwarded as-is via `error_detail` on stream close; `Retryable=false` once bytes reached the client | Same as the non-stream row |
| Post-first-byte truncation, or upstream EOF with no terminal event | `stream_truncated` (via `error_detail` on stream close) | No cooldown, no replay |
| Client disconnect on emit | `connection_lifecycle` | No cooldown |
| Dial/TLS/DNS failure | `transient_transport` | Retryable, no cooldown |

**The host's credential-wide cooldown is 429-gated.** `MarkResult` (`sdk/cliproxy/auth/conductor_cooldown.go`) only applies the `credential_quota` wrap inside `case 429`; a 401/402/403 cools just the reporting model for a flat 30 minutes. That is why every credential-scoped outcome is rewritten to 429 even when upstream sent 402/403 — clients see 429 for spend-cap errors, by design. On a 429 the host uses `RetryAfterSeconds` verbatim (10s floor); without it, it falls back to exponential backoff from 1s capped at 30m, so a missing reset hint means the credential is retried quickly.

Non-JSON upstream bodies fall back to status-only classification. Both `rateLimit.reset` (seconds, milliseconds when > 1e11, numeric string, or RFC3339) and `Retry-After` (delta-seconds or HTTP-date) are **clamped** to `maxRetryAfterSeconds` (32 days, covering weekly windows and a monthly billing period), never rejected.

Verified end-to-end against a real v8.0.67 host: a window-exhausted 429 produced `scope: credential, reason: credential_quota, remaining_seconds: 3541` (matching the mock's reset instant), while a plain 429 produced `scope: model, reason: transient_rate_limit`.

## Versioning and release

- `pluginVersion` in `internal/plugin/plugin.go` is reported to the host and asserted exactly in `internal/plugin/plugin_test.go`; bump both together.
- A release also updates `README.md` / `README.zh-CN.md`, `CHANGELOG.md` (user-facing, bilingual), and `RELEASE_NOTES.md` (used as the GitHub release body).
- Pushing a `v*` tag runs `.github/workflows/build.yml` (also triggered on `pull_request` and manually via `workflow_dispatch`). The test job runs `go test ./...` then `go vet ./...`; the build matrix covers linux amd64/arm64, darwin amd64/arm64, windows amd64, windows arm64 (cross-cgo), and freebsd amd64 (`continue-on-error`, so its failure does not block a release). Each zip must contain exactly one library at its root, plus a release-wide `checksums.txt`.

**The `release` job is gated on `refs/tags/v*`**, so a `workflow_dispatch` run builds artifacts but publishes nothing. Two consequences observed in practice:

- A tag push sometimes does not surface in `gh run list` immediately, and run/job `status` can lag behind reality — an artifact endpoint can show finished zips while the run still reads `queued`. Check `gh api .../runs/<id>/artifacts` before concluding a build is stuck.
- If the dispatched run does reach its matrix, the artifacts are still usable: `gh release create` can publish them manually, but the release job (when it does run) overwrites `checksums.txt` with the full platform set.

Cross-compiling `-buildmode=c-shared` requires CGO and a matching C toolchain, so a Windows-only machine cannot produce the Linux library; CI is the supported path.

## Conventions in this codebase

- Comments cite spec identifiers (`FR-###`, `arch §`, `AC §`, `F1`–`F5`) whose source documents are **not** in this repository; treat them as stable rationale labels, not links.
- `docs/` holds the in-repo design records: `model-capabilities.md` (per-model endpoints, reasoning-effort probes, why there is no capability API; linked from the README) and `oauth-api-key-design.md` (Chinese design doc for the OAuth-guide + manual-key entry points). Read them before changing routing defaults or the auth flow.
- Code comments explain invariants and rejected alternatives heavily (especially around failure modes and race conditions). Match that density when changing concurrency, credentials, or protocol translation.
- User-facing strings in the management API and pages are Chinese; code comments and docs are English (with bilingual README/CHANGELOG/RELEASE_NOTES).
- Commits follow Conventional Commits with scopes such as `fix(adapter)`, `feat(auth)`, `feat(quota)`, `fix(plugin)`, `chore(release)`.
