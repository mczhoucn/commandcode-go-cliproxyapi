# CommandCode GOAT CLIProxyAPI 插件

[English](README.md) | **简体中文**

一个 [CLIProxyAPI](https://help.router-for.me/plugin/development) 的原生动态 Go 插件，把 **CommandCode GOAT** 套餐作为单一 provider（`commandcode`；对外发布的模型 id 带 `commandcode/` 前缀）对外提供。

插件负责模型发现、协议翻译、请求执行，以及 CommandCode 账户与额度页面。API key 只来自 CLIProxyAPI 认证文件，因此同一批账户可以同时服务 OpenAI、Anthropic 和 Responses 三种客户端。

## 上游实际提供了什么

CommandCode GOAT 通过它的 OpenAI 兼容入口访问：

| 上游路由 | 状态 |
|---|---|
| `POST {base-url}/chat/completions` | 服务 OSS 模型（`deepseek/*`、`z-ai/*`、`Qwen/*`、`MiniMaxAI/*`、`moonshotai/*` …） |
| `GET {base-url}/models` | 模型目录 |
| `POST {base-url}/messages` | 只接受 Claude 模型 —— OSS 模型 id 会被拒绝：*"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | 未注册的路由 |

默认上游地址是 `https://api.commandcode.ai/provider/v1`。

因为目录里的模型实际上只有一条可用路由，**从 `/models` 发现的所有模型默认都走 chat-completions**，不区分厂商前缀。`route-overrides` 是给"上游确实在别的端点提供其他协议"的部署准备的例外通道。

## 为什么需要这个插件

不用插件时，把 CommandCode GOAT 接入 CLIProxyAPI 需要手写多个 provider 段，而且几个客户端强依赖的能力要么缺失、要么是错的：

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
- **多协议客户端翻译**：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 三种请求都会转成上游 chat-completions 调用，响应（含流式）再转回来。
- **思考内容保真**：把上游的 `reasoning`、`reasoning_details[].text`、`reasoning_content` 归一化到各目标格式：
  - Claude 客户端：前置一个 `thinking` 块 + `thinking_delta` 事件；
  - Responses 客户端：前置一个 `reasoning` output item + `response.reasoning_summary_*` 事件；
  - OpenAI 客户端：每个 chunk 回填 `reasoning_content`，厂商原始字段保持不变。
- **能力感知的思考控制**：`route-overrides` 或模型目录里的 thinking 元数据一旦声明能力，就恢复严格校验与预算夹取；没有任何声明时，客户端的 `reasoning_effort` 原样转发（`auto`/`none` 省略该字段），以上游为准。
- **动态目录发现**：远程目录 + 本地兜底、去重，并对被排除的模型给出诊断。至少有一个启用的账户时才会发现目录。
- **CommandCode账户与额度**：管理后台菜单是 **CommandCode**，页面是 **CommandCode账户与额度**。粘贴 API key 并保存为认证文件后，可查看每个账户的邮箱、套餐、剩余套餐额度，以及滚动 5 小时/每周窗口（已用、上限、重置时间）；每张卡片按需独立刷新。没有账户也可以启动。
- **宿主认证调度**：启用的认证文件账户经 CLIProxyAPI 调度器跨协议共享。插件配置里没有密钥池，旧的 `api-keys` 也不会被导入进去。

## 环境要求

- **CLIProxyAPI**：`v7.2.138+`
- **Go**：1.26+（`-buildmode=c-shared` 需要启用 CGO）

## v0.1.2 升级与发布说明

- 已有认证文件继续使用。插件配置中的 `api-keys` 已停用，不会自动导入。新增账户请进入 **CommandCode → CommandCode账户与额度**，或自行上传认证文件；禁用和删除仍在宿主认证文件页。
- 手动升级时，替换与目标操作系统、处理器架构对应的插件库，重启 CLIProxyAPI，再刷新管理后台。只修改本仓库的版本号，不会更新已经运行的插件。
- 向 [CPA 插件仓库](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)提交前，需要发布 GitHub 标签 `v0.1.2`。每个受支持平台应提供 `commandcode-go-cliproxyapi_0.1.2_<goos>_<goarch>.zip`，并提供汇总的 SHA-256 校验文件 `checksums.txt`。推送版本标签后，现有构建流程会生成这些文件。
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
      # （CommandCode 自身的 OSS 模型都在 chat-completions 上）
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
| `catalog-url` | `string` | `{base-url}/models` | 目录发现地址。 |
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

- **添加**：管理后台 → **CommandCode** → **CommandCode账户与额度**。粘贴 API key 并保存。不必上传自制 JSON；仍然可以自行上传认证文件。
- **禁用 / 删除**：仍在宿主认证文件页，不在这个页面。
- **没有账户**：插件可以启动。目录发现只在至少一个账户启用时进行，在此之前该 provider 不会对外发布模型。
- **旧 `api-keys`**：已有安装可以保留这段配置。账户页会提示它已被忽略。它不会被导入、不会被保存，也不参与路由、目录发现或额度，也不能把已删除的账户带回来。确认认证文件就是想保留的账户之后，请自行删掉这段。

### 配额页面

管理后台 → **CommandCode** 里的 **CommandCode账户与额度** 页面读取与 `base-url` 同源的账号接口：

| 接口 | 用途 |
|---|---|
| `GET {authority}/alpha/billing/credits` | 剩余套餐额度和 5 小时/每周窗口 |
| `GET {authority}/alpha/billing/subscriptions` | 套餐 id/状态 |
| `GET {authority}/alpha/whoami?limits=1` | 卡片标题用的账号邮箱 |

`{authority}` 由 `base-url` 去掉 provider 路径（`/provider/v1`）得到。卡片对应宿主认证文件，而不是 `api-keys` 条目。每张卡片手动独立刷新，页面从不轮询，配额数值也不参与路由决策。

**邮箱同步的已知限制**：查到邮箱后，插件会尽量写回原认证文件，让插件页和宿主认证文件页都能显示邮箱。写回前会检查文件是否已删除或更改，但最后一次检查与替换文件之间仍存在竞争窗口；同时删除、禁用或修改备注，可能被旧内容覆盖，删除的文件也可能被重新创建。当前选择保留两处显示邮箱的行为，未消除这一风险。建议等额度刷新结束后再修改认证文件，操作后刷新列表确认结果。

### reasoning effort（思考强度）

CommandCode **没有能力查询接口**：`{base-url}/models` 只返回 `id`、`object`、`created`、`owned_by`、`name`、`context_length`，`/alpha/*` 账号面里也没有 models 或 capabilities 路由。真正的"每个模型支持哪些 effort"只存在于厂商自己的客户端里（`command-code` CLI 和网页端各自内置一份静态表），而上游网关本身只要值属于并集 `low | medium | high | xhigh | max` 就接受，并不按模型校验。

因此本插件的行为是：

- 目录里没有声明能力时，`reasoning_effort` 原样转发（`auto`/`none` 省略该字段），所以 `xhigh` 和 `max` 都能用；
- 一旦目录条目带了 `thinking` 对象（`levels`、`min`、`max`、`zero_allowed`、`dynamic_allowed`），就以该声明为准，恢复严格校验与预算夹取；
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

也可通过 `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` 指定本机已有的 Chrome 可执行文件，跳过下载浏览器。

## 来源

本插件在编写时参考了另外两个 CommandCode 插件：

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi)（v0.1.7）是直接的前身：adapter 内核、catalog、config、auth 和配额页骨架都来自那里；CommandCode 相关的改动——思考内容保真、无能力声明时的 effort 透传、chat-completions 单一默认路由、配额页改用真实账号接口——在其之上适配。
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode)（ahoo）是更早的一个独立 CommandCode 插件，它的文档最先记录了本插件必须复现的厂商行为：思考文本出现在 `reasoning` / `reasoning_details[].text` 而从不使用标准的 `reasoning_content`；因此必须在 CLIProxyAPI 的 openai→claude 翻译器读取之前回填 `reasoning_content`；流式 `/v1/messages` 要求每个分块带 SSE `data: ` 前缀；上游只接受完整厂商名（`deepseek/deepseek-v4.1-flash`）。

本仓库：<https://github.com/mczhoucn/commandcode-go-cliproxyapi>

## 许可证

[MIT](LICENSE)
