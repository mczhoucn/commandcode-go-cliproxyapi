# ChangeLog

本文件记录面向使用者的更新摘要。详细版本历史见 [RELEASE_NOTES.md](RELEASE_NOTES.md)，操作步骤见 [中文说明](README.zh-CN.md) / [English README](README.md)。

## v0.2.0 — 2026-11-12

- **插件改名为 `commandcode-or-or-cliproxyapi`**（原 `commandcode-go-cliproxyapi`）。新名字不再含 `go`，避免与 CommandCode Go 套餐混淆。
- **这是不兼容变更**，因为插件 ID 是宿主接口的一部分：
  - 配置里的 `plugins.configs.commandcode-go-cliproxyapi` 需要改成新 ID，否则配置不会生效；
  - 插件库文件名改为 `commandcode-or-or-cliproxyapi.<ext>`，旧的同名库需要删除；
  - 管理接口路由变为 `/v0/resource/plugins/commandcode-or-or-cliproxyapi/*` 与 `/v0/management/plugins/commandcode-or-or-cliproxyapi/*`；
  - 已保存的认证文件（`commandcode-key-<hash>.json`）内容不受影响，其中的 `type` 仍是 `commandcode`；provider 键没有变，账户和额度会继续显示。
- 功能与 v0.1.5 相同，没有其他行为变化。
- 仍支持 **CommandCode GOAT / Pro / Max，不支持 Go**。

### v0.2.0 English highlights — 2026-11-12

- **Renamed to `commandcode-or-or-cliproxyapi`** (was `commandcode-go-cliproxyapi`). The name no longer carries a `go` that could be mistaken for CommandCode Go plan support.
- **Breaking**, because the plugin ID is part of the host contract:
  - the `plugins.configs.commandcode-go-cliproxyapi` entry must move to the new ID or the configuration will not apply;
  - the library is now `commandcode-or-or-cliproxyapi.<ext>`; remove the old library;
  - management routes move to `/v0/resource/plugins/commandcode-or-or-cliproxyapi/*` and `/v0/management/plugins/commandcode-or-or-cliproxyapi/*`;
  - existing auth files (`commandcode-key-<hash>.json`) need no change: their `type` is still `commandcode`, so accounts and quota keep working.
- Behaviour is identical to v0.1.5; nothing else changed.
- Still **CommandCode GOAT / Pro / Max only; Go is not supported**.

## v0.1.5 — 2026-11-12

- **补齐 DeepSeek 文档所定义的下游能力**（三种客户端协议均已覆盖）：
  - **思考内容历史回传**：此前 Claude 和 Responses 客户端的历史思考块被直接丢弃。带 `tools` 的请求在后续轮次必须完整回传思考内容，否则上游拒绝。现在 Claude 的 `thinking` 块回传为 `reasoning_content`，Responses 的 `reasoning` item 明文 `content` 合并进相邻 assistant 消息。
  - **Anthropic `output_config.effort`**：DeepSeek 的 Anthropic 兼容面读取 `output_config.effort` 并忽略 `budget_tokens`。三种客户端声明的思考强度现在都会以该字段发出；显式 `output_config.effort` 优先于预算换算，`none`/`auto` 作为开关处理而不当作强度。
  - **图片 `detail`**：`low`/`high`/`original`/`auto` 在 Chat Completions 与 Responses 两种书写位置都会被识别并保留，非法值丢弃。
  - **Responses 思考事件双发**：同一段思维链同时以 OpenAI 标准的 `response.reasoning_summary_text.*` 和 DeepSeek 文档的 `response.reasoning_text.*` 两族事件发出，任一族客户端都能读到。
- 修复 Claude 请求解码器未读取 `thinking` 字段导致所有历史思考块文本为空的问题。
- 上游协议不变：仍使用 CommandCode 的 OpenAI 兼容 Provider API（`provider/v1`），与两个参考实现一致；不实现 CLI 私有的 `/alpha/generate`。
- 插件现在注册 CPA 的标准配额提供者能力（`QuotaProvider`）：CommandCode 认证文件在管理后台的认证文件列表里可以直接看到套餐与剩余额度进度条，和内置 provider 一致，不再只存在于插件自己的页面。
- 标准卡片显示 5 小时、每周和每月三个窗口的剩余比例、已用/上限与重置时间；CommandCode 只上报剩余月度额度，套餐未知时不显示月度窗口，避免出现没有分母的假进度条。
- CommandCode 没有重置额度的接口，因此不声明支持重置。
- **模型与凭据冷却改为完全跟随 CPA 配置**：插件现在按 CommandCode 真实的错误形态上报冷却范围与时长，由 CPA 的冷却配置（`routing.cooldown.*`、`disable-cooling`、`transient-error-cooldown-seconds` 等）决定实际行为。
  - 用量窗口耗尽（`RATE_LIMITED` + `rateLimit.window`）→ 整条凭据冷却，时长取上游给的 reset 时间；
  - 套餐额度/credit 耗尽 → 整条凭据冷却；
  - 普通 429 → 只冷却当前模型，不记额度耗尽；
  - 模型不在套餐内 → 只冷却该模型；
  - 请求本身的缺陷（提示词过长等）→ 不冷却，也不轮换凭据；
  - 首字节后流中断 / 客户端断连 → 不冷却、不重放；
  - 连接、TLS、DNS 失败 → 可重试但不冷却凭据。
- 上游 `Retry-After` 响应头（秒数或 HTTP 日期）会被透传为冷却时长，异常值（0、负数、超过 24 小时）一律丢弃。
- 错误分类依据是从官方 CLI `command-code 1.88.5` 中提取的真实处理逻辑，不是推测。
- 最低支持的 CLIProxyAPI 版本提高到 `v8.0.43`（首个同时具备配额提供者与冷却错误字段的版本），插件依赖的 SDK 升级到 `v8.0.67`。插件只声明 schema 6（自身实际用到的契约），因此更老的宿主不会因为 SDK 新特性而被误拒。
- 不改变套餐支持范围、认证文件或配置：仍支持 **CommandCode GOAT / Pro / Max，不支持 Go**。已有安装需替换插件库并重启 CPA。

### v0.1.5 English highlights — 2026-11-12

- **DeepSeek-documented client capabilities are now complete on all three protocols**:
  - **Historical thinking is replayed.** Claude and Responses clients used to have their earlier thinking discarded. A tool-carrying request must replay it on every later turn or the upstream rejects the request, so Claude `thinking` blocks now round-trip as `reasoning_content` and a Responses `reasoning` item's plaintext `content` merges into the adjacent assistant message.
  - **Anthropic `output_config.effort`.** DeepSeek's Anthropic surface reads `output_config.effort` and ignores `budget_tokens`. All three client formats now emit it; an explicit effort wins over a budget-derived level, and `none`/`auto` are treated as switches rather than strengths.
  - **Image `detail`.** `low`/`high`/`original`/`auto` is recognized in both documented placements and preserved; unknown values are dropped.
  - **Dual Responses reasoning events.** One chain of thought is emitted under both the OpenAI-standard `response.reasoning_summary_text.*` family and DeepSeek's documented `response.reasoning_text.*` family, so a client listening for either receives it.
- Fix the Claude request decoder not reading the `thinking` field, which emptied every replayed thinking block.
- The upstream protocol is unchanged: CommandCode's OpenAI-compatible Provider API (`provider/v1`), matching both reference implementations. The CLI-private `/alpha/generate` is not implemented.
- The plugin now registers CPA's standard `QuotaProvider` capability, so CommandCode credentials render their plan and remaining usage directly on the host's auth-file cards instead of only on the plugin's own page.
- The standard card shows the 5-hour, weekly, and monthly windows with remaining ratio, used/cap, and reset time. CommandCode reports only remaining monthly credits, so the monthly window is omitted when the plan is unknown rather than drawing a meter with no denominator.
- Reset is not advertised: CommandCode exposes no quota reset endpoint.
- **Model and credential cooldowns now follow CPA's configuration entirely.** The plugin reports the real CommandCode error shape, and CPA's own cooling settings (`routing.cooldown.*`, `disable-cooling`, `transient-error-cooldown-seconds`) decide what actually happens:
  - usage-window exhaustion (`RATE_LIMITED` + `rateLimit.window`) → cool the whole credential for the upstream reset time;
  - plan credit exhaustion → cool the whole credential;
  - a plain 429 → cool only the reporting model, recording no quota exhaustion;
  - model outside the plan → cool only that model;
  - request defects (prompt too long, etc.) → no cooldown and no credential rotation;
  - post-first-byte stream truncation or client disconnect → no cooldown, never replay;
  - dial/TLS/DNS failures → retryable without cooling the credential.
- An upstream `Retry-After` header (delta-seconds or HTTP-date) is forwarded as the cooldown duration; bogus values (0, negative, over 24h) are discarded.
- Classification is derived from the vendor CLI's own logic (`command-code 1.88.5`), not inference.
- Minimum CLIProxyAPI is now `v8.0.43`, the first release with both the quota provider and these cooldown error fields; the SDK dependency moves to `v8.0.67`. The plugin deliberately declares schema 6 — the contract it actually uses — so older hosts are not rejected for unrelated newer SDK features.
- No plan-support, auth-file or configuration changes: **CommandCode GOAT / Pro / Max only; Go is not supported**. Replace the plugin library and restart CPA.

## v0.1.4 — 2026-11-04

- 修复管理后台启用“记住密码”后，CommandCode 页面仍提示“Enable Remember password”的兼容性问题：管理后台新的 `enc::v2::` 认证存储格式现在可以被插件正确读取。
- 配额页和 OAuth 页同时兼容 `enc::v1::` 与 `enc::v2::`；v2 按当前管理后台地址解码，不再错误混入浏览器 User-Agent。
- 不改变套餐支持范围、认证文件或配置：仍支持 **CommandCode GOAT / Pro / Max，不支持 Go**。已有安装需更新插件并重启 CPA，发布新版本不会自动替换运行中的插件。

### v0.1.4 English highlights — 2026-11-04

- Fix the compatibility issue where the CommandCode pages still showed “Enable Remember password” even when Management Center had Remember password enabled: the plugin now reads the Management Center's newer `enc::v2::` auth-storage format.
- Make both the quota and OAuth pages accept `enc::v1::` and `enc::v2::`; v2 is decoded using the current Management Center origin without incorrectly including the browser User-Agent.
- No plan-support, auth-file or configuration changes: **CommandCode GOAT / Pro / Max only; Go is not supported**. Update the plugin and restart CPA; publishing does not automatically replace an installed plugin.

## v0.1.3 — 2026-10-20

- 修复通过 Responses 接口接收流式工具调用时，回复已经结束、客户端却仍将工具视为未完成而不执行的问题；覆盖上游 Chat Completions 和 Messages 两条转换路径。
- 在 `response.completed` 前，按原输出序号发送 `response.function_call_arguments.done` 和 `response.output_item.done`；开始、参数增量、结束事件与最终输出复用同一个 `fc_` 工具项编号，并保留原始 `call_id`。
- 不改变套餐支持范围、认证文件或配置：仍支持 **CommandCode GOAT / Pro / Max，不支持 Go**。已有安装需更新插件并重启 CPA，发布新版本不会自动替换运行中的插件。

### v0.1.3 English highlights — 2026-10-20

- Fix streamed Responses tool calls remaining unfinished in clients after the response ends, for both Chat Completions and Messages upstream conversion paths.
- Emit `response.function_call_arguments.done` and `response.output_item.done` before `response.completed`, preserving the announced output index and reusing the same `fc_` item ID across events and final output while retaining the original `call_id`.
- No plan-support, auth-file or configuration changes: **CommandCode GOAT / Pro / Max only; Go is not supported**. Update the plugin and restart CPA; publishing does not automatically replace an installed plugin.

## v0.1.2 — 2026-10-20

### 支持范围与名称

- 支持 **CommandCode GOAT / Pro / Max**，**不支持 CommandCode Go 套餐**。
- Go 走官方 CLI 专用的私有接口与协议，不是本插件接入的 OpenAI 兼容 Provider API；Go 也没有 Provider API 权限。CPA 当前的 CommandCode 接入未实现这套私有协议，改用 OAuth 登录不会改变套餐支持范围。
- 真实插件名为 `commandcode-go-cliproxyapi`，继承自 `opencode-go-cliproxyapi`。名称中的 `go` 是历史命名，不表示支持 Go 套餐；插件 ID 和仓库地址不变。

### 优化账户添加的易用性

- 在插件添加的独立 **CommandCode → CommandCode账户与额度** 页面直接输入已有 API key。
- 插件联网核验身份、套餐与有效期后，自动将凭据保存为 CPA 认证文件；不再需要到配置文件中手工添加 API key，也不用手写或上传认证 JSON。
- 旧插件配置 `api-keys` 已停用，不自动导入；已有认证文件继续使用。认证文件的禁用、删除仍在 CPA 认证文件页完成。

### 增加 CommandCode OAuth 登录

- 从 **CPA → OAuth 登录 → CommandCode → 新增** 进入引导页，再打开官方链接登录、授权。
- **不需要事先到 CommandCode 官网手动创建 API key。** 官网在授权流程中生成凭据，用户从官方备用页复制，粘回 CPA 引导页完成。
- 官网通过 HTTP POST（JSON 或表单）返回认证凭据，**不把认证 Token / API key 放进 Callback 的 HTTP GET URL 或地址栏**，所以不能通过复制回调网址完成认证。
- 当前方案利用 Chrome 默认禁止访问 `http://localhost:6000/callback` 的行为，故意让回调传递失败，进入官方备用流程；不需要监听端口或端口转发，也不要解除该端口限制。
- **授权通过后，可能短暂出现“回调网址 / 认证传递错误”提示，再自动进入凭据复制备用页，这是设计中的正常过渡。** 不要仅因这一提示重新授权。若迟迟没有进入备用页、无法复制凭据，或 CPA 核验/保存失败，则应按真实异常处理。
- 在同一地址的 CPA 管理页登录并勾选“记住密码”。当前只验证了 Chrome；其他浏览器尚未验证。
- 这是“浏览器授权、复制凭据完成”，不是自动回调、独立 access/refresh token 或自动续期。只有 CPA 确认并核对到认证文件后，才显示保存成功。

### 两种认证方式并行共存，使用能力相同

- **已有 API key** 和 **OAuth 浏览器授权** 最终都生成同类 `type: commandcode`、`api_key` 认证文件，共用身份/订阅核验、模型调用、调度和额度查询能力。
- 唯一的来源区别是 `credential_source` 标记；OAuth 新增不会获得额外的模型或额度功能。
- 相同 key 不重复添加、不主动覆盖已有禁用状态或备注。相同账户的不同 key 可以共存，但共享原订阅额度，不会变成多份独立额度。
- 额度仍在插件独立页面展示；尚未接入 CPA 认证文件卡片的标准额度能力（`QuotaProvider`）。

### 保留 OpenAI Chat Completions 转换

- 创建本插件时，CommandCode 官方还没有 OpenAI Responses 接口；目前官方已提供，且多数 OpenAI 与开放模型已经支持。
- 截至本次发布准备时（2026-10-20），官方公开模型目录中的 **GLM-5.3 Flash**（`z-ai/glm-5.3-flash`）、**Tencent Hy4 Preview**（`tencent/hy4-preview`）、**DeepSeek V4 Flash Fast**（`deepseek/deepseek-v4-flash-fast`）等少数模型仍只声明 `/chat/completions`，没有声明 `/responses`。
- 因此保留把 OpenAI Responses 等客户端请求转换为上游 OpenAI Chat Completions 的默认路径。不是“先尝试 Responses 再自动回退”，也不会自动按 `supported_endpoints` 改变路由；可通过已存在的 `route-overrides` 显式选择已核实支持的端点。
- 这是官方目录声明的时间快照，不是永久限制或逐模型推理实测。最新状态以[官方 Provider API 文档](https://commandcode.ai/docs/provider)和[实时模型目录](https://api.commandcode.ai/provider/v1/models)为准。

### 登录兼容性与边界

- 修复旧版 CPA 因重发相同配置而中断登录会话的问题。
- 修复 CPA 管理 JSON 对官方链接进行 HTML 转义后丢失正确 state 参数的问题，并补充最终显示链接的浏览器回归。
- 已交付给 CPA 的保存不保证能通过取消撤回；保存结果不明时不自动重写。宿主并发文件操作、邮箱回写与 HTTP 重定向的既有已接受限制仍在，见 README。

## English summary

### v0.1.2 English highlights — 2026-10-20

- Support **CommandCode GOAT / Pro / Max**, not **Go**. Go uses the official CLI's private protocol and has no Provider API access; CPA's current CommandCode integration does not implement it. The plugin name `commandcode-go-cliproxyapi` is inherited from `opencode-go-cliproxyapi`, not a Go-plan support claim.
- Improve usability: paste an existing API key on the independent **CommandCode accounts and quota** page; verification and auth-file creation replace manual key configuration and hand-written JSON. Old `api-keys` config is not imported.
- Add **CommandCode OAuth login** without manually pre-creating a key. Authorize on the official site, copy the generated credential from its fallback page, and paste it into CPA.
- Credentials return by HTTP POST, not in a callback HTTP GET URL. The Chrome-blocked localhost:6000 callback intentionally triggers the official failed-delivery fallback. A brief callback/delivery error before the credential-copy page is expected; missing fallback, unavailable credentials or failed CPA validation/save is not.
- Both methods coexist and produce equivalent auth files with the same model, scheduling and quota capabilities. Only the source label differs. Identical keys are deduplicated; different keys do not multiply subscription quota. Quota remains on the plugin page, not CPA auth-file cards.
- Retain default **OpenAI Chat Completions translation**: Responses was unavailable at creation and now supports most OpenAI/open models, but GLM-5.3 Flash, Tencent Hy4 Preview and DeepSeek V4 Flash Fast still declare only Chat Completions in the 2026-10-20 catalog snapshot. Routes do not switch automatically.
- Fix same-configuration reloads interrupting login and HTML-escaped official URLs losing state. Browser regression checks the final visible authorization link. Existing save/cancel, host-file, email-sync and redirect limitations remain documented in the README.

本版本的目标发布页为 [GitHub Release v0.1.2](https://github.com/mczhoucn/commandcode-go-cliproxyapi/releases/tag/v0.1.2)；安装包和校验文件仅以发布成功后的实际附件为准。发布不会自动更新已有 CPA 安装；请按 README 更新插件并重启 CPA。
