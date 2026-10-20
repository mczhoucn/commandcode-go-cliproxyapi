# CommandCode GOAT / Pro / Max CLIProxyAPI 插件

[English](README.md) | **简体中文**

一个 [CLIProxyAPI](https://help.router-for.me/plugin/development) 的原生动态 Go 插件，把 **CommandCode GOAT / Pro / Max** 套餐作为单一 provider（`commandcode`；对外发布的模型 id 带 `commandcode/` 前缀）对外提供。

插件负责模型发现、协议翻译、请求执行，以及 CommandCode 账户与额度页面。API key 只来自 CLIProxyAPI 认证文件，因此同一批账户可以同时服务 OpenAI、Anthropic 和 Responses 三种客户端。

> **支持 CommandCode GOAT / Pro / Max，不支持 CommandCode Go 套餐。** Go 使用官方 CLI 专用的私有接口与协议，不是本插件接入的 OpenAI 兼容 Provider API；Go 也没有 Provider API 权限。CPA 当前的 CommandCode 接入未实现这套私有协议，因此不能通过换一种登录方式来使用 Go 套餐。
>
> **名称里的 `go` 不代表 Go 套餐。** 当前插件名为 `commandcode-go-cliproxyapi`，沿用自前身 [`opencode-go-cliproxyapi`](https://github.com/massiveits/opencode-go-cliproxyapi) 的命名；不因套餐支持范围而更改插件 ID。

更新摘要见 [ChangeLog](CHANGELOG.md)，详细历史见 [发布说明](RELEASE_NOTES.md)。

## 上游实际提供了什么

CommandCode GOAT / Pro / Max 通过它的 OpenAI 兼容入口访问：

| 上游路由 | 状态 |
|---|---|
| `POST {base-url}/chat/completions` | 服务 OSS 模型（`deepseek/*`、`z-ai/*`、`Qwen/*`、`MiniMaxAI/*`、`moonshotai/*` …） |
| `GET {base-url}/models` | 模型目录 |
| `POST {base-url}/messages` | 只接受 Claude 模型 —— OSS 模型 id 会被拒绝：*"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | 官方现已提供，多数 OpenAI 与开放模型支持；仍需按模型核对 `supported_endpoints`，并非所有模型都可用 |

默认上游地址是 `https://api.commandcode.ai/provider/v1`。

插件创建之初，CommandCode 官方尚未提供 OpenAI Responses 接口；现在官方已经支持，大部分 OpenAI 与开放模型也已接入。但截至本次发布准备时（2026-10-20），官方公开模型目录中的以下模型仍只声明 Chat Completions：

| 模型 | 上游模型 ID | 当前声明的接口 |
|---|---|---|
| GLM-5.3 Flash | `z-ai/glm-5.3-flash` | `/chat/completions` |
| Tencent Hy4 Preview | `tencent/hy4-preview` | `/chat/completions` |
| DeepSeek V4 Flash Fast | `deepseek/deepseek-v4-flash-fast` | `/chat/completions` |

因此本插件**仍保留将客户端请求转换为 OpenAI Chat Completions 的默认路径**，让 Responses 客户端也能使用这些模型。它不是“先尝试 Responses，失败后自动回退”；插件也不会因目录出现 `supported_endpoints` 就自动切换路由。`route-overrides` 可在确认某模型支持目标端点后显式改用 Messages 或 Responses。

以上是本次发布准备时的目录快照，不是永久限制，也不代表逐模型推理实测。最新状态以[官方 Provider API 文档](https://commandcode.ai/docs/provider)及[实时模型目录](https://api.commandcode.ai/provider/v1/models)的 `supported_endpoints` 为准。

## 为什么需要这个插件

不用插件时，需要自行维护 CommandCode GOAT / Pro / Max 的 provider、认证及协议选择；按协议拆成多个 provider 配置时，还会遇到重复配置和调度分散的问题。此外，厂商特有的思考字段和能力声明也需要适配：

- **配置与密钥重复**：同一批 key 要在多个 provider 段里各配一遍。
- **调度被割裂**：轮换、限流、冷却无法共享，管理后台也看不到每个 key 的用量。
- **客户端得懂协议**：客户端必须事先知道某个模型对应哪个上游端点。
- **思考内容丢失**：CommandCode 的思考文本出现在 `reasoning` / `reasoning_details[].text` 里（从不使用标准的 `reasoning_content`），普通 OpenAI 兼容 provider 会让 Anthropic 和 Responses 客户端**整段丢失思维链**。
- **effort 被本地拒绝**：CommandCode 的 `/models` 不提供任何 thinking 元数据，于是本地校验 `reasoning_effort` 的实现会把上游明明接受的 `xhigh`/`max` 直接拒掉。

## 解决方式

- **认证文件是唯一密钥来源**：账户是 CLIProxyAPI 认证文件，不是插件配置里的 `api-keys`。在管理后台 → **CommandCode** → **CommandCode账户与额度** 粘贴 API key 并保存。不必手写 JSON；仍然可以自行上传认证文件。禁用和删除仍在宿主认证文件页。轮换、重试、冷却、会话粘性和每个账户的统计仍由宿主调度器负责。
- **透明翻译与路由**：客户端只写 `commandcode/<上游 id>`，不需要知道上游协议。
- **思考保真**：三种客户端格式都能拿到上游的思考文本；模型没有能力声明时，`reasoning_effort` 原样转发。
- **单一模型目录**：`{base-url}/models` 里的模型全部以 `commandcode/` 前缀出现在 `/v1/models`。

## 功能

- **单一 Provider 命名空间**：模型形如 `commandcode/deepseek/deepseek-v4.1-flash`、`commandcode/z-ai/glm-5.3-flash`（前缀可配置，也可关闭而直接使用上游裸 id）。
- **多协议客户端翻译**：默认把 OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 请求转换成上游 OpenAI Chat Completions 调用，再转换响应（含流式）。官方 Responses 已覆盖多数模型，但保留该路径以兼容尚未支持 Responses 的模型；显式 `route-overrides` 可选择其他已核实可用的上游接口。
- **思考内容保真**：把上游的 `reasoning`、`reasoning_details[].text`、`reasoning_content` 归一化到各目标格式：
  - Claude 客户端：前置一个 `thinking` 块 + `thinking_delta` 事件；
  - Responses 客户端：前置一个 `reasoning` output item + `response.reasoning_summary_*` 事件；
  - OpenAI 客户端：每个 chunk 回填 `reasoning_content`，厂商原始字段保持不变。
- **能力感知的思考控制**：有效的目录 `thinking` 元数据为相关转换分支提供能力校验和预算边界。`route-overrides` 只选择协议与端点，不能声明思考能力；缺少声明时以上游是否接受为准。`auto`/`none` 并非所有路径都会自动省略，详见下方思考强度说明。
- **动态目录发现**：远程目录 + 本地兜底、去重，并对被排除的模型给出诊断。至少有一个启用的账户时才会发现目录。
- **CommandCode账户与额度**：管理后台菜单是 **CommandCode**，页面是 **CommandCode账户与额度**。粘贴 API key 并保存为认证文件后，可查看每个账户的邮箱、套餐、剩余套餐额度，以及滚动 5 小时/每周窗口（已用、上限、重置时间）；每张卡片按需独立刷新。没有账户也可以启动。
- **宿主认证调度**：启用的认证文件账户经 CLIProxyAPI 调度器跨协议共享。插件配置里没有密钥池，旧的 `api-keys` 也不会被导入进去。

## 环境要求

- **CLIProxyAPI**：`v7.2.138+`
- **Go**：1.26+（`-buildmode=c-shared` 需要启用 CGO）

## v0.1.3 升级与发布说明

- 本次修复 Responses 流式工具调用缺少结束事件的问题，避免客户端在回复结束后仍将工具视为未完成而不执行；覆盖 Chat Completions 和 Messages 上游转换路径。
- 已有认证文件继续使用。插件配置中的 `api-keys` 已停用，不会自动导入。新增账户可在 **CommandCode → CommandCode账户与额度** 输入已有 API key，或通过 **OAuth 登录 → CommandCode** 授权添加，无需再手工向配置文件添加 key；仍可自行上传认证文件，禁用和删除仍在宿主认证文件页。
- 手动升级时，替换与目标操作系统、处理器架构对应的插件库，重启 CLIProxyAPI，再刷新管理后台。只修改本仓库的版本号，不会更新已经运行的插件。
- 向 [CPA 插件仓库](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)提交前，需要发布 GitHub 标签 `v0.1.3`。每个受支持平台应提供 `commandcode-go-cliproxyapi_0.1.3_<goos>_<goarch>.zip`，并提供汇总的 SHA-256 校验文件 `checksums.txt`。推送版本标签后，现有构建流程会生成这些文件。
- 每个 ZIP 的根目录只能包含一个对应的插件库：`commandcode-go-cliproxyapi.so`、`commandcode-go-cliproxyapi.dylib` 或 `commandcode-go-cliproxyapi.dll`。插件 ID 和仓库地址保持不变；插件仓库通过最新 GitHub Release 获取版本。

## 构建

```bash
# Linux (AMD64)
go build -buildmode=c-shared -o plugins/linux/amd64/commandcode-go-cliproxyapi.so .

# macOS (ARM64)
go build -buildmode=c-shared -o plugins/darwin/arm64/commandcode-go-cliproxyapi.dylib .

# Windows (AMD64)
go build -buildmode=c-shared -o plugins/windows/amd64/commandcode-go-cliproxyapi.dll .
```

把产物放到宿主的插件目录（例如 `<cliproxyapi_root>/plugins/<os>/<arch>/`）。

## 配置

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  configs:
    commandcode-go-cliproxyapi:
      # 上游地址（默认 "https://api.commandcode.ai/provider/v1"）
      base-url: "https://api.commandcode.ai/provider/v1"

      # 可选：目录地址覆盖（默认 "{base-url}/models"）
      # 此地址会收到 API key：只应使用可信服务，推荐与 base-url 的协议、主机、端口一致。
      # catalog-url: "https://api.commandcode.ai/provider/v1/models"

      # 客户端可见的模型 id 前缀
      model-prefix:
        enabled: true              # true -> "commandcode/<model>"（默认 true）
        value: "commandcode"

      # 账户只来自 CLIProxyAPI 认证文件，不写在这里。
      # 管理后台 → CommandCode → CommandCode账户与额度。

      # 目录发现
      catalog:
        refresh-interval: "15m"           # 最小 "1m"
        stale-while-unavailable: true

      # 协议开关：关掉某个协议会移除路由到它的全部模型
      protocols:
        chat-completions: true
        messages: true
        responses: true

      # 按模型钉路由；只有上游在别的端点提供该模型时才需要
      # （默认仍走 chat-completions；核实模型支持后可显式改走 messages / responses）
      route-overrides:
        "claude-sonnet-5":
          protocol: "messages"            # chat-completions | messages | responses
          endpoint: "/v1/messages"        # 必填，必须以 "/" 开头

      request-timeout: "5m"
      max-response-bytes: 67108864        # 64 MiB
      allow-http: false                   # 本地测试允许 http://
```

### 配置项

| 配置 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `api-keys` | — | 忽略 | 已停用，不再作为密钥来源。见下方「账户」。 |
| `base-url` | `string` | `https://api.commandcode.ai/provider/v1` | 上游 provider 地址。必须是合法 HTTPS（`allow-http: true` 时可用 HTTP），不能带 query、fragment 或 userinfo。 |
| `catalog-url` | `string` | `{base-url}/models` | 目录地址；即使目录公开，也会收到启用账户的 bearer API key。只应设置为可信服务，推荐与 `base-url` 的协议、主机、端口一致；当前配置校验不会强制同源。 |
| `model-prefix.enabled` | `bool` | `true` | `true` 时客户端用 `<prefix>/<model>`，`false` 时直接暴露上游裸 id。 |
| `model-prefix.value` | `string` | `commandcode` | provider 前缀。 |
| `catalog.refresh-interval` | `duration` | `15m` | 目录轮询周期（最小 `1m`）。 |
| `catalog.stale-while-unavailable` | `bool` | `true` | 刷新失败时继续提供上一份有效目录。 |
| `protocols.*` | `bool` | `true` | 路由总开关；关闭的协议会带着诊断信息排除其模型。 |
| `route-overrides` | `map` | `{}` | `{ 模型: { protocol, endpoint } }`，把某个模型钉到别的上游路由；`endpoint` 必填。 |
| `request-timeout` | `duration` | `5m` | 上游 HTTP 超时（配额/账号请求另按 30s 上限）。 |
| `max-response-bytes` | `int64` | `67108864` | 非流式响应体上限。 |
| `allow-http` | `bool` | `false` | 允许 `http://` 上游，仅供本地测试。 |

### 账户

API key 不写在这份 YAML 里。CLIProxyAPI 认证文件是唯一来源。

- **手动添加**：管理后台 → **CommandCode** → **CommandCode账户与额度**。粘贴已有 API key，联网核验身份及有效订阅后保存。无需上传自制 JSON；宿主仍允许自行上传认证文件，上传入口不受本插件的新增核验约束。
- **浏览器授权添加**：CPA → **OAuth 登录** → **CommandCode** → 新增。打开插件引导页的官方授权网址，在官网登录并授权，从官方备用页复制自动生成的 key，粘回引导页。无需事先在官网手动创建 key；不是复制回调网址，也不是自动回调。
- **两种方式可并行共存，使用能力相同**：最终都生成同类 `type: commandcode`、`api_key` 认证文件，使用相同的身份/订阅核验、模型调用和额度查询逻辑，不存在 OAuth 账户专属功能；只有 `credential_source` 来源标记不同。相同 key 仍会去重，不同 key 可以共存。
- 两个新增入口只接受身份核验成功且套餐为 GOAT / Pro / Max 的有效订阅。当前仅识别 `individual-goat`、`individual-pro`、`individual-pro-v1`、`individual-max`，状态必须为 `active` 且有效期未结束。Go、未知套餐、无法确认、网络失败不保存。旧文件不自动迁移或删除。
- 相同 key（包括自定义文件名中的 key）不重复添加，不主动修改旧认证的禁用状态或备注。同一账户的不同 key 可以分别添加，但不是多份独立订阅额度。来源标签仅表示新增入口，不证明 key 与本次浏览器授权绑定。
- **禁用 / 删除**：仍在宿主认证文件页，不在这个页面。
- **没有账户**：插件可以启动。目录发现只在至少一个账户启用时进行，在此之前该 provider 不会对外发布模型。
- **旧 `api-keys`**：已有安装可以保留这段配置。账户页会提示它已被忽略。它不会被导入、不会被保存，也不参与路由、目录发现或额度，也不能把已删除的账户带回来。确认认证文件就是想保留的账户之后，请自行删掉这段。

### 浏览器授权的使用条件与限制

- 在**同一地址**的 CPA 管理页面登录并勾选“记住密码”，引导页才能复用已有管理登录；不通过网址传递管理密码，不新建密码存储。
- 当前备用流程已由用户在 Chrome 中实际确认可进入；使用 `http://localhost:6000/callback`，不带 `mode=redirect`。Chrome 默认禁止连接此端口，官网请求失败后进入备用页。不要解除该端口限制；其他浏览器未验证，不承诺可用。插件不开监听服务，不需要端口转发。
- **短暂的回调错误提示是预期过渡步骤**：官网通过 HTTP POST（JSON 或表单）发送凭据，而不是将认证 Token / API key 放进 Callback 的 HTTP GET URL 或地址栏。本插件刻意使用上述会被 Chrome 拦截的回调，使官网进入认证传递失败的备用流程。官方授权通过后，可能先短暂出现回调 URL / 认证传递错误提示，随后跳到提供凭据复制按钮的官方备用页；这是当前设计，不必仅因这段提示重复授权。**若没有进入备用页、无法复制凭据，或 CPA 最终核验/保存失败，则是真正需要处理的异常，不能忽略。**
- 官网可能新建一个 API key，授权后底层仍使用它；没有独立的 OAuth access/refresh token 或自动续期。官方临时存储和剪贴板可能含 key，插件不把新增 key 写入浏览器存储、网址或日志。
- 会话有效期 10 分钟。完成提交只验证、暂存；CPA 轮询取得结果后保存。只有宿主确认完成并核对到匹配认证文件时，引导页才显示成功。关闭原 OAuth 窗口后，引导页仍可继续确认。
- **交付前可取消，交给 CPA 后不能保证取消撤回保存。** 保存失败、超时或结果不明确时不自动再次交付；先查看认证文件。未确认的保存会保留内存中的 key 哈希占用，避免两个入口重复写入。重启、多实例及宿主上传/编辑不受插件私有锁的原子保证。
- **网络取舍**：核验沿用 CPA 的代理、TLS 和 HTTP 跳转规则；受信任配置决定核验地址，页面不能指定。现有宿主无法由插件禁止跳转，部分跳转（子域名或同主机其他端口）可能继续携带认证头。这项限制已接受，未修复；不能宣称严格禁止跨源转发。

### 配额页面

额度显示在插件自己的页面。当前尚未接入 CPA 认证文件卡片使用的标准额度查询能力（`QuotaProvider`），通过 OAuth 新增也不会自动让该卡片显示额度。
管理后台 → **CommandCode** 里的 **CommandCode账户与额度** 页面读取与 `base-url` 同源的账号接口：

| 接口 | 用途 |
|---|---|
| `GET {authority}/alpha/billing/credits` | 剩余套餐额度和 5 小时/每周窗口 |
| `GET {authority}/alpha/billing/subscriptions` | 套餐 id/状态 |
| `GET {authority}/alpha/whoami?limits=1` | 卡片标题用的账号邮箱 |

`{authority}` 由 `base-url` 去掉 provider 路径（`/provider/v1`）得到。卡片对应宿主认证文件，而不是 `api-keys` 条目。加载列表时，缺少邮箱的账户可能会自动查询一次以补全信息；其余由用户按卡片手动刷新。页面没有定时额度轮询，配额数值也不参与路由决策。

**邮箱同步的已知限制**：查到邮箱后，插件会尽量写回原认证文件，让插件页和宿主认证文件页都能显示邮箱。写回前会检查文件是否已删除或更改，但最后一次检查与替换文件之间仍存在竞争窗口；同时删除、禁用或修改备注，可能被旧内容覆盖，删除的文件也可能被重新创建。当前选择保留两处显示邮箱的行为，未消除这一风险。建议等额度刷新结束后再修改认证文件，操作后刷新列表确认结果。

### reasoning effort（思考强度）

目前没有已确认可供本插件读取的统一 thinking / effort 能力接口。旧探测时，`{base-url}/models` 只有 `id`、`object`、`created`、`owned_by`、`name`、`context_length`；2026-10-20 的公开目录已包含 `supported_endpoints`，它表示模型可用的请求接口，不等于思考强度声明。厂商客户端内置 effort 表和网关对 `low | medium | high | xhigh | max` 的接受情况来自 2026-09-17 的历史检查，不保证当前仍接受这些值，也不证明它们实际生效。

因此本插件的行为是：

- 目录未声明能力时，默认 Chat Completions 路径会传递调用者设置的 effort，不会仅因缺少声明而在本地拒绝 `xhigh` / `max`。原生 Chat Completions 输入及 Responses 转 Chat Completions 会保留已设置的 `auto` / `none`，不能靠它们保证省略字段；如果不希望发送 effort，应省略输入中的这个设置。上游是否接受及实际效果以上游为准；
- 目录条目提供有效的 `thinking` 对象（`levels`、`min`、`max`、`zero_allowed`、`dynamic_allowed`）时，相关转换分支以它作为能力校验和预算边界。路由覆盖不会补充这份能力声明；
- 厂商的按模型对照表、上述结论的探测过程和"没有能力接口"的证据都记录在 [`docs/model-capabilities.md`](docs/model-capabilities.md)。

## 测试

```bash
go test ./...        # 单元测试 + 带 mock 的端到端测试
go test ./... -cover # 带覆盖率
go vet ./...         # 静态检查
```

可选的浏览器回归测试覆盖新增表单、密钥不进入浏览器存储、删除后的旧响应、禁用状态及窄屏/深色主题。它只使用本地模拟接口，不访问真实账户：

```bash
npx --yes playwright install chromium
npx --yes -p playwright -c 'NODE_PATH=$(dirname $(dirname $(command -v playwright))) node tests/accounts_page.cjs'
```

新增 OAuth 页面模拟测试（同样不访问真实账户）：

```bash
npx --yes -p playwright -c 'NODE_PATH=$(dirname $(dirname $(command -v playwright))) node tests/oauth_page.cjs'
```

原生宿主端到端测试需要提供本机宿主程序和已经构建的插件；测试自行创建临时配置、模拟上游和认证目录，不读取运行中的 CPA：

```bash
CPA_HOST_BINARY=/绝对路径/cli-proxy-api \
CPA_PLUGIN_LIBRARY=/绝对路径/commandcode-go-cliproxyapi.dylib \
node tests/oauth_host.cjs
```

设置 `CPA_BROWSER=1` 可同时驱动真实引导页，需以相同方式提供 Playwright。Linux 使用对应 `.so`，macOS 使用 `.dylib`。
也可通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` 指定本机已有的 Chrome 可执行文件，跳过下载浏览器。

## 来源

本插件在编写时参考了另外两个 CommandCode 插件：

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi)（v0.1.7）是直接的前身：adapter 内核、catalog、config、auth 和配额页骨架都来自那里；CommandCode 相关的改动——思考内容保真、无能力声明时的 effort 透传、chat-completions 单一默认路由、配额页改用真实账号接口——在其之上适配。
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode)（ahoo）是更早的一个独立 CommandCode 插件，它的文档最先记录了本插件必须复现的厂商行为：思考文本出现在 `reasoning` / `reasoning_details[].text` 而从不使用标准的 `reasoning_content`；因此必须在 CLIProxyAPI 的 openai→claude 翻译器读取之前回填 `reasoning_content`；流式 `/v1/messages` 要求每个分块带 SSE `data: ` 前缀；上游只接受完整厂商名（`deepseek/deepseek-v4.1-flash`）。

本仓库：<https://github.com/mczhoucn/commandcode-go-cliproxyapi>

## 许可证

[MIT](LICENSE)
