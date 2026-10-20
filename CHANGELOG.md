# ChangeLog

本文件记录面向使用者的更新摘要。详细版本历史见 [RELEASE_NOTES.md](RELEASE_NOTES.md)，操作步骤见 [中文说明](README.zh-CN.md) / [English README](README.md)。

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
