## wb2cpa ${GITHUB_REF_NAME}

CLIProxyAPI plugin for **Tencent CodeBuddy** (`copilot.tencent.com`) and **WorkBuddy Global** (`www.workbuddy.ai`). The runtime plugin ID remains `workbuddy`, so existing library names, credential `"type"`, and CPA config keys remain compatible.

### WorkBuddy 管理页与账户概览

The existing resource URL remains compatible:

```text
/v0/resource/plugins/workbuddy/api-key
```

It is now the **WorkBuddy 管理** page with:

- per-credential account cards (masked account identifier, realm, auth mode, enablement state)
- real OAuth plan type, package balance, billing cycle, and CN enterprise quota when upstream provides them
- dynamic model credit multipliers and image / reasoning / tool-call capabilities
- manual whole-account or single-account refresh
- a retained API Key creation tab

Account data is queried server-side only. API keys, OAuth access/refresh tokens, complete credential JSON, and raw billing responses are never returned to the browser or management API. API-key credentials state that balance queries are unsupported rather than showing a false zero balance.

### CN and Global OAuth

The management page provides explicit login buttons for both realms. OAuth state, token polling, account lookup, and refresh now stay on the realm that issued or owns the credential:

| Realm | OAuth / inference base | Billing base |
|---|---|---|
| CN | `https://copilot.tencent.com` | `https://www.codebuddy.cn` |
| Global | `https://www.workbuddy.ai` | `https://www.workbuddy.ai` |

The standard CPA OAuth card remains CN by default for backward compatibility.

### Quality and compatibility

- Dynamic model discovery still filters the CLI allowlist and preserves static fallback; cached management metadata exposes upstream `credits` without changing CPA registration behavior.
- API Key save errors no longer echo the credential JSON.
- CI now runs both `go vet ./...` and `go test ./...` before building release assets.

### Install

Add the repository's registry to CPA if you self-host the plugin:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/aiden-git/wb2cpa/main/registry.json"
  configs:
    workbuddy: { enabled: true, priority: 100 }
```

Release assets contain only `workbuddy.so` / `.dylib` / `.dll` at the zip root, plus `checksums.txt`.
