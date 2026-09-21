## workbuddy ${GITHUB_REF_NAME}

CLIProxyAPI plugin for **Tencent CodeBuddy** (`copilot.tencent.com`) and **WorkBuddy Global** (`www.workbuddy.ai`).

### 本次更新

#### 1. 国际版（Global realm）支持

按凭据 `domain` 字段自动路由到对应接入点，无需额外配置：

| `domain` | Realm | 聊天端点 |
|---|---|---|
| `copilot.tencent.com`（默认） | CN | `https://copilot.tencent.com/v2/chat/completions` |
| `www.workbuddy.ai` / `*.workbuddy.ai` | 全局 | `https://www.workbuddy.ai/v2/chat/completions` |

登录 / 刷新端点固定走 CN；聊天与模型列表按 realm 分叉。全局账号自动使用 `X-No-Enterprise-Id` + `X-Domain: www.workbuddy.ai`，并切换 `Origin` / `Referer`。

#### 2. 动态模型发现

模型列表不再写死，改为从上游拉取（每 realm 独立缓存：成功 1 小时 / 失败 5 分钟）：

- 仅保留 CLI Agent 白名单内的模型
- 过滤 embedding / 代码补全 / 图像生成类（`nes-` / `completion-` / `codewise-` 前缀，`maxOutputTokens ≤ 256`，`text-to-image` 标签）
- 忽略上游标记 `disabled` 的模型
- 拉取失败或返回空时降级到内置静态列表（`glm-5.2`、`hy3`、`deepseek-v4-pro` 等）

#### 3. 请求改写增强（`sanitizeBlockedTemplates`）

扩展为 6 条规则，并加快速路径（无触发词时不做任何字符串操作）：

| 触发串 | 改写 |
|---|---|
| `You are Claude Code, Anthropic's official CLI for Claude.` | `CLI` → `CLI tool` |
| `You are Codex, Anthropic's official CLI for Claude.` | 同上 |
| `Main branch (you will usually use this for PRs)` | → `Default branch` |
| `Use the feedback tool to give feedback to Anthropic.` | 整句删除 |
| 任意 `11128` | → `11-128`（反探测） |
| `x-anthropic-billing-header:value` 段 | 整段删除 |
| `cc_xxx=...;` KV 对 | 整对删除 |

`rewriteSystemForUpstream` 同时新增：`developer` 角色 → `system`；`max_completion_tokens` → `max_tokens`（已有 `max_tokens` 时保留原值）。

#### 4. 工具调用配对清理

- `repackToolResultBlocks` — 将穿插在 tool-result 块中的非 tool 消息移至块尾，保证 tool 结果紧跟 assistant
- `cleanupOrphanToolCalls` — 对称剪枝孤立的 `tool_call` / `tool` 消息；`tool_calls` 数组清空时移除该键

修复部分客户端（Claude Code 等）因消息顺序不规范触发的上游报错。

#### 5. 其他修复

- **`X-Refresh-Token` 不再随聊天请求发送** —— 该头仅属于刷新流程，此前误加到每个 chat 请求上
- 新增 22 条单元测试（sanitize 全部规则、改写逻辑、工具调用清理、realm 判定、模型过滤），`go test ./...` 全绿

### Install (plugin store)

1. Ensure CLIProxyAPI v7.2+ with plugins enabled.
2. Add this repo as a store source in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/aiden-git/workbuddy-cli-proxy/main/registry.json"
  configs:
    workbuddy: { enabled: true, priority: 100 }
```

3. Refresh the plugin store in the management UI, then install **WorkBuddy (CodeBuddy)** — or:

```http
POST /v0/management/plugin-store/workbuddy/install
```

> Alternatively, download the zip for your platform, extract `workbuddy.so` / `.dylib` / `.dll` and drop it into CPA's `plugins/` directory.

### Credentials

- **OAuth / QR login** via CPA management UI (workbuddy provider).
- **Manual API key**: upload an auth file, e.g.

```json
{
  "type": "workbuddy",
  "auth_type": "api_key",
  "api_key": "YOUR_CODEBUDDY_API_KEY",
  "user_id": "anonymous",
  "domain": "copilot.tencent.com"
}
```

For a Global account, set `"domain": "www.workbuddy.ai"` instead.

### Assets

Zip root contains only `workbuddy.so` / `.dylib` / `.dll` per platform:

- `workbuddy_0.3.3_linux_amd64.zip`
- `workbuddy_0.3.3_linux_arm64.zip`
- `workbuddy_0.3.3_darwin_amd64.zip`
- `workbuddy_0.3.3_darwin_arm64.zip`
- `workbuddy_0.3.3_windows_amd64.zip`
- `workbuddy_0.3.3_windows_arm64.zip`
- `checksums.txt` (sha256sum format)