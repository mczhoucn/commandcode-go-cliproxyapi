# Release Notes

## v0.1.2 — Auth-file account management

Upgrade notice: auth files are now the only API key source. The retired plugin `api-keys` block is not imported; preserve the auth files you want to keep.

- The Management Center menu is **CommandCode**. **CommandCode accounts and quota** (CommandCode账户与额度) can paste an API key and save it as a host auth file. Hand-written JSON is unnecessary; uploading an auth file still works.
- Disable and delete stay on the host auth-file page.
- A leftover `api-keys` block in plugin config is not a key source. It is not parsed, expanded, stored, imported, or used for routing, catalog discovery, or quota, and it cannot bring back an account you deleted. The accounts page warns when the block is still present. An existing install may keep it; remove it yourself after the auth files are the accounts you want.
- The plugin can start with no accounts. Catalog discovery runs only when at least one account is enabled.
- Plugin config with no keys is valid.
- Known limitation: email sync is retained for both pages. Its final file check and replacement are not atomic with host deletion/disabling/metadata edits; concurrent changes may be overwritten or a deleted file recreated. This risk is accepted, not fixed. See the quota-page section in the README.

**中文**

- 管理后台菜单为 **CommandCode**。「CommandCode账户与额度」可以粘贴 API key 并保存为宿主认证文件。不必上传自制 JSON，仍可自行上传认证文件。
- 禁用和删除仍在宿主认证文件页。
- 插件配置里的旧 `api-keys` 不再是密钥来源：不解析、不展开、不保存、不导入，也不参与路由、目录发现或额度，不能复活已删除的账户。账户页会提示这段仍在。已有安装可以留着它，确认认证文件无误后自行移除。
- 没有账户也可以启动。目录发现需要至少一个启用的账户。
- 不含密钥的插件配置可以加载。
- 已知限制：保留两处显示邮箱的同步行为。最后的文件检查与替换不能和宿主删除、禁用、备注修改保持原子性；同时操作可能丢失修改或重新创建被删文件。这项风险已接受，未修复。详见 README 配额页面说明。

## v0.1.1 — Accept array function_call_output results

Responses `function_call_output.output` may be a string or an array of text
parts (`input_text`, `output_text`, or `text`). The array shape is flattened
into the string Chat Completions and Messages already send upstream. Non-text
parts are still rejected.

**Upgrade notes**

- Replace the plugin binary and restart CLIProxyAPI.

## v0.1.0 — CommandCode GOAT provider

First release of the plugin as a CommandCode GOAT provider, derived from
opencode-go-cliproxyapi v0.1.7.

**Adaptations**

- **Reasoning preservation**: upstream `reasoning`, `reasoning_details[].text`
  and `reasoning_content` are read together (mirrored values counted once) and
  emitted as a leading Claude `thinking` block, a leading Responses `reasoning`
  item with summary events, or a backfilled `reasoning_content` on the OpenAI
  passthrough — streaming and non-streaming.
- **Chat-completions-only routing**: every catalog model defaults to
  `{base-url}/chat/completions`, because CommandCode's `/messages` endpoint
  accepts Claude models only and `/responses` is not registered. The family
  prefix table was removed; `route-overrides` remain for other upstreams.
- **Effort passthrough**: models without thinking metadata (all CommandCode
  models) are no longer validated or clamped locally — `xhigh`/`max` reach the
  upstream verbatim, `auto`/`none` omit the field. A model that declares levels
  or bounds opts back into eager validation.
- **Quota page against the real account API**: cards show account email, plan,
  remaining plan credits, and the rolling 5-hour/weekly windows from
  `/alpha/billing/credits`, `/alpha/billing/subscriptions` and `/alpha/whoami`.
- Defaults renamed (`commandcode` prefix, `api.commandcode.ai/provider/v1`
  base URL, `commandcode-go-cliproxyapi` plugin id).

**Upgrade notes**

- The provider namespace changed, so client model ids must be updated
  (for example `commandcode/deepseek/deepseek-v4.1-flash`).
- Replace the plugin binary, restart CLIProxyAPI, and hard-refresh the
  Management Center.
