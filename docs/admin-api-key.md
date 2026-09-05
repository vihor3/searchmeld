# 管理员 API Key 接口文档

管理员 API Key 是 One Search Relay 提供给可信外部系统使用的超级凭据。它可以调用需要管理员权限的 `/api/admin/*` 接口，也可以调用需要普通 API Token 的 `/v1/*` 搜索与抽取接口。

> 代码依据：后端在 `backend/internal/api/auth.go` 中的 `requireAdmin` 和 `requireAPIToken` 都会识别管理员 API Key；路由注册见 `backend/internal/api/handlers.go`。

## 1. 基本概念

- 管理员 API Key 前缀：`oak_`
- 外部搜索 API Token 前缀：`osr_`
- 管理员密码会话：服务端生成的 HttpOnly Cookie `searchmeld_admin_session`，不是请求头 Token。
- 系统同时只保存一个管理员 API Key。每次重新生成都会让旧 Key 立即失效。
- 管理员 API Key 明文只在生成响应中显示一次，之后只能查看 `key_prefix`、`created_at`、`updated_at`。
- 管理员 API Key 拥有完整管理权限，并可调用 Extract 而无需单独声明 `extract` scope，请只保存在可信服务端环境中。

## 2. 生成和查看管理员 API Key

### 2.1 管理员登录

可以在管理台用现有管理员账号和密码登录，然后在“系统设置”中生成管理员 API Key。首次账号仍由现有启动配置创建，不需要新的初始化服务。

无浏览器的 HTTP 客户端也可以登录。下面的 Bash 示例使用权限受限的临时 Cookie jar，后续 2.2–2.4 在同一 shell 中执行。示例密码仅是占位符；真实凭据不要写入命令行参数、shell 历史或日志，可改用权限为 `600` 的 JSON 文件作为 `--data-binary @文件路径` 输入。公网将 `BASE_URL` 改为已配置的 HTTPS 源，部署要求见 [README](../README.md)。

```bash
umask 077
BASE_URL=http://localhost:5173
COOKIE_JAR=$(mktemp)
trap 'rm -f -- "$COOKIE_JAR"' EXIT

curl --fail-with-body -X POST "$BASE_URL/api/admin/login" \
  --cookie-jar "$COOKIE_JAR" \
  -H 'X-SearchMeld-Admin: 1' \
  -H 'Content-Type: application/json' \
  --data-binary @- <<'JSON'
{
  "username": "admin",
  "password": "your-admin-password"
}
JSON
```

响应：

```json
{
  "expires_at": "2026-06-12T00:00:00Z"
}
```

登录只通过 `Set-Cookie` 返回会话凭据，JSON 不包含 `token`。Cookie 为 host-only、`Path=/api/admin`、`HttpOnly`、`SameSite=Lax`，无 Max-Age/Expires；HTTPS 规范源下设置 Secure。服务端默认固定 24 小时到期，后端重启也会失效；浏览器的会话恢复不会延长服务端期限。不要展示、提交或上传 Cookie jar。

Cookie 登录在同一浏览器配置的独立标签页共享。所有 Cookie 管理调用（包括 GET）及密码登录都需要 `X-SearchMeld-Admin: 1`；浏览器 fetch 还需 `credentials: 'include'`。如果提供 Origin，必须匹配规范源或明确配置的可信源；originless CLI 可以凭证明头和 Cookie 使用这些接口。

### 2.2 生成或轮换管理员 API Key

```bash
curl --fail-with-body -X POST "$BASE_URL/api/admin/settings/admin-api-key" \
  --cookie "$COOKIE_JAR" \
  -H 'X-SearchMeld-Admin: 1'
```

也可以用已有管理员 API Key 自我轮换：

```bash
curl -X POST "$BASE_URL/api/admin/settings/admin-api-key" \
  -H "Authorization: Bearer oak_old_xxx"
```

响应示例：

```json
{
  "key": "oak_new_xxx",
  "key_prefix": "oak_new_",
  "created_at": "2026-06-11T10:00:00Z",
  "updated_at": "2026-06-11T10:00:00Z"
}
```

注意：

- `key` 只在这一次响应中出现。
- 轮换成功后旧的 `oak_` Key 立即不可用。
- 该操作会写入审计日志，actor 形如 `admin_api_key:<key_prefix>` 或 `admin`。

### 2.3 查看当前管理员 API Key 元信息

```bash
curl --fail-with-body "$BASE_URL/api/admin/settings/admin-api-key" \
  --cookie "$COOKIE_JAR" \
  -H 'X-SearchMeld-Admin: 1'
```

响应示例：

```json
{
  "key_prefix": "oak_abcd",
  "created_at": "2026-06-11T10:00:00Z",
  "updated_at": "2026-06-11T10:00:00Z"
}
```

如果尚未生成过管理员 API Key，响应为空对象：

```json
{}
```

### 2.4 注销密码会话

```bash
curl --fail-with-body -X POST "$BASE_URL/api/admin/logout" \
  --cookie "$COOKIE_JAR" \
  -H 'X-SearchMeld-Admin: 1'
rm -f -- "$COOKIE_JAR"
```

成功返回 `{"status":"ok"}`，只撤销本次鉴权的服务端会话，不发送清除 Cookie 的 `Set-Cookie`，避免延迟响应抹掉其他标签页的新登录。失效 Cookie 即使保留也无法鉴权。Cookie 删除、服务端到期/重启、成功退出后，下一次管理请求必须重新登录。403、网络错误或 5xx 不证明注销成功，应确认后重试。API Key 调用 logout 是不撤销 Key 的成功操作，也不注销请求中偶然携带的 Cookie 会话。

## 3. 鉴权调用方式

管理员 API Key 支持两种请求头。

推荐方式：

```http
Authorization: Bearer oak_xxx
```

兼容方式：

```http
X-API-Key: oak_xxx
```

示例：

```bash
export BASE_URL=http://localhost:5173
export ADMIN_API_KEY=oak_xxx

curl "$BASE_URL/api/admin/dashboard" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

等价写法：

```bash
curl "$BASE_URL/api/admin/dashboard" \
  -H "X-API-Key: $ADMIN_API_KEY"
```

请求一旦携带 Authorization 或 X-API-Key，就只按请求头选择规则验证管理员 Key；已选的空值、无效凭据、旧 `adm_` 或普通 `osr_` 管理请求返回 401，不回退到偶然携带的有效 Cookie。Bearer 优先于 X-API-Key；有效 `oak_` CLI 无需 Cookie 或 `X-SearchMeld-Admin`。提供了 Origin 的 Key 请求仍须通过严格的管理源校验。

**旧客户端迁移**：账号密码登录接口没有删除，但不再返回 JSON `token`，`adm_` 不再可作 Bearer/X-API-Key 管理凭据。已有脚本改用持久 `oak_`，首次生成使用上面的 Cookie jar 或设置页。升级前已打开的管理页面仍有期待 token JSON 的旧 JavaScript，先刷新旧页面加载新版，再登录一次；不要只在旧表单中重新输入密码。新版页面和新标签页共享 Cookie。已有账号、Key、Token 及权限不重建。网页 Cookie 不授予业务 API 或需要鉴权的 MCP 权限，不能用旧 localStorage/sessionStorage Token 绕过 Cookie 删除。管理接口的成功、失败及秘密查看响应均设置 `Cache-Control: no-store`、`Pragma: no-cache`。

## 4. 管理员 API Key 开放接口总览

除 `POST /api/admin/login` 是用户名密码登录入口外，管理员 API Key 可调用以下两类接口：

1. 所有受管理员权限保护的 `/api/admin/*` 接口。
2. 所有受 API Token 保护的 `/v1/*` 搜索与抽取接口。

`GET /healthz` 不需要鉴权。

### 4.1 管理接口 `/api/admin/*`

| 方法 | 路径 | 能否使用管理员 API Key | 说明 |
| --- | --- | --- | --- |
| `POST` | `/api/admin/logout` | 可以 | Cookie 调用撤销本会话且不清除 Cookie；Key 调用不吊销 Key，也不注销偶然携带的 Cookie。 |
| `GET` | `/api/admin/me` | 可以 | 返回 `{"username":"admin"}` 形式的身份信息，不返回凭据。 |
| `GET` | `/api/admin/dashboard` | 可以 | 获取用量、Provider、Provider 健康度、30 天账单摘要。 |
| `GET` | `/api/admin/providers` | 可以 | 获取 Provider 配置列表。 |
| `GET` | `/api/admin/providers/health` | 可以 | 获取 Provider 健康状态。 |
| `PATCH` | `/api/admin/providers/{name}` | 可以 | 更新 Provider 配置。`name` 为内置 Provider 名，例如 `exa`、`you`、`jina`、`tavily`、`firecrawl`、`serper`、`brave`。 |
| `GET` | `/api/admin/keys` | 可以 | 获取 Provider Key 列表，只返回脱敏信息。 |
| `POST` | `/api/admin/keys` | 可以 | 创建 Provider Key。 |
| `PATCH` | `/api/admin/keys/{id}` | 可以 | 更新 Provider Key。 |
| `POST` | `/api/admin/keys/{id}/test` | 可以 | 使用指定 Provider Key 发起测试搜索。 |
| `POST` | `/api/admin/keys/{id}/quota` | 可以 | 查询并保存该 Key 的官方额度/账单信息或本地估算额度。 |
| `DELETE` | `/api/admin/keys/{id}` | 可以 | 删除 Provider Key。 |
| `GET` | `/api/admin/tokens` | 可以 | 获取外部 API Token 列表，只返回前缀和配置。 |
| `POST` | `/api/admin/tokens` | 可以 | 创建外部 API Token，并在响应中返回 `raw_token`。 |
| `GET` | `/api/admin/tokens/{id}/secret` | 可以 | 解密并返回完整 Token，供管理台点击复制；响应禁止缓存并记录审计日志。 |
| `PATCH` | `/api/admin/tokens/{id}` | 可以 | 更新 Token 配置或状态。 |
| `DELETE` | `/api/admin/tokens/{id}` | 可以 | 删除外部 API Token。 |
| `GET` | `/api/admin/settings` | 可以 | 获取运行时设置。 |
| `PUT` | `/api/admin/settings` | 可以 | 覆盖更新运行时设置。 |
| `GET` | `/api/admin/settings/admin-api-key` | 可以 | 查看管理员 API Key 元信息。 |
| `POST` | `/api/admin/settings/admin-api-key` | 可以 | 生成/轮换管理员 API Key。 |
| `GET` | `/api/admin/logs` | 可以 | 获取请求日志列表（Search / Extract）。支持 `limit`。 |
| `GET` | `/api/admin/logs/{id}` | 可以 | 获取单条请求日志详情和 Provider 调用明细。 |
| `GET` | `/api/admin/usage/summary` | 可以 | 获取总体用量汇总。 |
| `GET` | `/api/admin/usage/billing` | 可以 | 获取账单/计量汇总。支持 `days`。 |
| `GET` | `/api/admin/metrics` | 可以 | 获取网关指标聚合。 |
| `GET` | `/api/admin/audit-logs` | 可以 | 获取审计日志列表。支持 `limit`。 |
| `POST` | `/api/admin/playground/search` | 可以 | 管理台搜索调试接口，不绑定外部 API Token ID。 |

### 4.2 搜索与抽取接口 `/v1/*`

管理员 API Key 也可用于搜索与抽取接口，效果类似超级 API Token：

| 方法 | 路径 | 能否使用管理员 API Key | 说明 |
| --- | --- | --- | --- |
| `POST` | `/v1/search` | 可以 | 原生搜索接口。 |
| `POST` | `/v1/extract` | 可以 | 原生 URL 内容抽取接口。 |
| `POST` | `/v1/compat/tavily/search` | 可以 | Tavily-like 兼容搜索接口。 |
| `POST` | `/v1/compat/tavily/extract` | 可以 | Tavily-like 兼容抽取接口。 |
| `POST` | `/v1/compat/serper/search` | 可以 | Serper-like 兼容搜索接口。 |
| `POST` | `/v1/compat/openai/responses-search` | 可以 | OpenAI-like 兼容搜索接口。 |
| `GET` | `/v1/providers` | 可以 | 获取 Provider 配置列表。 |
| `GET` | `/v1/usage/summary` | 可以 | 获取用量汇总。 |

与普通 `osr_` API Token 不同，管理员 API Key 不受 scopes 或 `allowed_providers` 限制，也不会作为 `api_token_id` 写入用量归属。普通 Token 调用 `/v1/search` 需要 `search` scope；调用 `/v1/extract`、`/v1/compat/tavily/extract` 或 MCP `extract` 必须显式包含 `extract` scope。

## 5. 管理接口调用示例

以下示例假设：

```bash
export BASE_URL=http://localhost:5173
export ADMIN_API_KEY=oak_xxx
```

### 5.1 查看 Dashboard

```bash
curl "$BASE_URL/api/admin/dashboard" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

响应包含：

- `usage`：总体请求数、成功/失败数、缓存命中、平均延迟。
- `providers`：Provider 配置。
- `provider_health`：Provider 健康度。
- `billing`：近 30 天计量/费用汇总。

### 5.2 查看 Provider 列表

```bash
curl "$BASE_URL/api/admin/providers" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

响应示例：

```json
{
  "providers": [
    {
      "id": 1,
      "name": "exa",
      "display_name": "Exa",
      "base_url": "https://api.exa.ai",
      "enabled": true,
      "priority": 10,
      "weight": 1,
      "timeout_ms": 12000,
      "settings": { "key_retry_count": 3, "max_concurrency": 0 },
      "available_keys": 1
    }
  ]
}
```

### 5.3 更新 Provider 配置

```bash
curl -X PATCH "$BASE_URL/api/admin/providers/exa" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "Exa",
    "base_url": "https://api.exa.ai",
    "enabled": true,
    "priority": 10,
    "weight": 1,
    "timeout_ms": 12000,
    "settings": {
      "key_retry_count": 3,
      "max_concurrency": 0,
      "request_result_limit": 10,
      "retry_error_types": ["auth", "quota_exhausted", "rate_limited"],
      "key_routing_strategy": "weighted_random"
    }
  }'
```

成功响应：

```json
{"status":"ok"}
```

说明：`PATCH /api/admin/providers/{name}` 会按请求体覆盖该 Provider 的主要配置字段，因此建议先 `GET /api/admin/providers`，在原对象基础上修改后再提交。`settings.max_concurrency` 是渠道级最大并发请求数，`0` 表示不限，正数表示该 Provider 同时在途请求上限。

### 5.4 创建 Provider Key

You.com 示例：

```bash
curl -X POST "$BASE_URL/api/admin/keys" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_name": "you",
    "alias": "you-main",
    "key": "you-provider-key",
    "weight": 1,
    "rpm_limit": 60,
    "daily_quota": 0,
    "monthly_quota": 0
  }'
```

Exa 示例：

```bash
curl -X POST "$BASE_URL/api/admin/keys" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_name": "exa",
    "alias": "exa-main",
    "key": "exa-search-api-key",
    "exa_api_key_id": "exa-api-key-id",
    "exa_service_key": "exa-team-management-x-api-key",
    "weight": 1,
    "rpm_limit": 60,
    "daily_quota": 0,
    "monthly_quota": 0
  }'
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `provider_name` | 内置 Provider 名：`exa`、`you`、`jina`、`tavily`、`firecrawl`、`serper`、`brave`。 |
| `alias` | Key 别名。同一 Provider 下唯一。 |
| `key` | 上游搜索 API Key，会加密存储。 |
| `exa_api_key_id` | Exa 官方 usage 查询使用的 API Key ID。Exa 可选但建议填写。 |
| `exa_service_key` | Exa Team Management `x-api-key`。创建 Exa Key 时当前后端要求必填。 |
| `weight` | 权重，默认 1。 |
| `rpm_limit` | 单 Key 每分钟限制，0 表示不限。 |
| `daily_quota` | 单 Key 日请求额度，0 表示不限。 |
| `monthly_quota` | 单 Key 月请求额度，0 表示不限。 |

### 5.5 更新 Provider Key

```bash
curl -X PATCH "$BASE_URL/api/admin/keys/1" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "alias": "you-main-updated",
    "status": "enabled",
    "weight": 2,
    "rpm_limit": 120,
    "daily_quota": 5000,
    "monthly_quota": 100000
  }'
```

可更新字段：

- `alias`
- `key`
- `exa_api_key_id`
- `exa_service_key`
- `status`：`enabled`、`disabled`、`cooling`、`exhausted`
- `weight`
- `rpm_limit`
- `daily_quota`
- `monthly_quota`

### 5.6 测试 Provider Key

```bash
curl -X POST "$BASE_URL/api/admin/keys/1/test" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "one search relay",
    "limit": 3
  }'
```

响应包含：

- `summary`：状态、错误类型、延迟、结果数等。
- `results`：归一化后的搜索结果。

### 5.7 查询额度/账单

```bash
curl -X POST "$BASE_URL/api/admin/keys/1/quota" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{}'
```

Exa 可传日期和分组参数：

```bash
curl -X POST "$BASE_URL/api/admin/keys/1/quota" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "start_date": "2026-06-01",
    "end_date": "2026-06-11",
    "group_by": "day"
  }'
```

支持情况：

| Provider | 查询方式 | 结果含义 |
| --- | --- | --- |
| `exa` | Team Management usage API | 指定周期用量/费用，不是剩余额度。 |
| `you` | Billing account balance API | 账户余额，单位 cents/USD。 |
| `jina` | `https://r.jina.ai/` 文本解析 | 解析 `Balance left`，单位 tokens。 |
| `tavily` | `GET https://api.tavily.com/usage` | 返回当前 Key usage/limit，按 credits 展示剩余额度。 |
| `firecrawl` | `GET https://api.firecrawl.dev/v2/team/credit-usage` | 返回团队 remainingCredits/planCredits 和账期。 |
| `serper` | 本地累计用量估算 | Serper 未公开独立余额接口；按注册赠送的 2500 credits 减本实例全生命周期累计 credits 估算剩余额度，不额外请求上游。 |
| `brave` | 正常搜索响应头 | 从正常搜索的 `X-RateLimit-*` 响应头被动保存额度快照；点击查询不会额外消耗请求，没有快照时显示本地累计请求数。 |

### 5.8 创建外部 API Token

```bash
curl -X POST "$BASE_URL/api/admin/tokens" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "client-a",
    "scopes": ["search", "extract"],
    "allowed_providers": ["exa", "jina", "tavily", "firecrawl"],
    "rate_limit_per_min": 60,
    "daily_quota": 1000,
    "monthly_quota": 30000
  }'
```

响应示例：

```json
{
  "token": {
    "id": 1,
    "name": "client-a",
    "token_prefix": "osr_abcd",
    "scopes": ["search", "extract"],
    "allowed_providers": ["exa", "jina", "tavily", "firecrawl"],
    "status": "enabled",
    "rate_limit_per_min": 60,
    "daily_quota": 1000,
    "monthly_quota": 30000,
    "usage_count": 0,
    "created_at": "2026-06-11T10:00:00Z",
    "updated_at": "2026-06-11T10:00:00Z"
  },
  "raw_token": "osr_xxx"
}
```

创建响应会直接返回 `raw_token`；之后管理员仍可调用 `GET /api/admin/tokens/{id}/secret` 重新读取，管理台也会在每次点击令牌时读取并复制。令牌列表本身仍只返回前缀，避免批量暴露完整凭据。`scopes` 省略或传空数组时默认保存为 `["search"]`。既有 Token 也保持原 scopes，升级不会自动增加 `extract`；需要正文抽取时必须显式加入 `extract`。

### 5.9 更新外部 API Token

更新 scopes、配额和允许 Provider：

```bash
curl -X PATCH "$BASE_URL/api/admin/tokens/1" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "client-a",
    "scopes": ["search", "extract"],
    "allowed_providers": ["exa", "you", "jina", "tavily", "firecrawl", "serper", "brave"],
    "rate_limit_per_min": 120,
    "daily_quota": 2000,
    "monthly_quota": 60000
  }'
```

更新 Token 时会保存请求中的完整 scopes 列表；省略或传空数组会回到默认 `["search"]`，从列表中移除 `extract` 会立即禁止该 Token 调用原生、Tavily 兼容和 MCP Extract。

启用/禁用 Token：

```bash
curl -X PATCH "$BASE_URL/api/admin/tokens/1" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"status":"disabled"}'
```

注意：当前后端逻辑中，如果请求体包含非空 `name`，则执行配置更新；如果 `name` 为空且 `status` 非空，则执行状态更新。

### 5.10 查看和更新运行时设置

查看：

```bash
curl "$BASE_URL/api/admin/settings" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

更新：

```bash
curl -X PUT "$BASE_URL/api/admin/settings" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "default_mode": "parallel",
    "default_providers": ["exa", "you", "jina", "tavily", "firecrawl", "serper", "brave"],
    "default_limit": 10,
    "default_dedupe": true,
    "request_timeout_ms": 20000,
    "cache_enabled": false,
    "cache_ttl_seconds": 3600,
    "cache_max_results": 20,
    "compat_tavily_enabled": true,
    "compat_serper_enabled": true,
	    "compat_openai_enabled": true,
	    "api_auth_required": true,
	    "allow_private_extract_targets": false,
	    "provider_health_window_minutes": 15,
    "provider_routing_strategy": "fixed",
    "log_retention_days": 3
  }'
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `default_mode` | 默认搜索模式：`parallel`、`fallback`、`single`。 |
| `default_providers` | 默认 Provider 列表；新库初始化和默认 fallback 为 `exa`、`you`、`jina`、`tavily`、`firecrawl`、`serper`、`brave`。 |
| `default_limit` | 默认返回结果数，搜索时最大限制为 50。 |
| `default_dedupe` | 是否默认去重。 |
| `request_timeout_ms` | 搜索请求总超时基线；Extract 会按 Provider 的单次超时、URL 批次和并发波次扩展必要预算。 |
| `cache_enabled` | 是否开启缓存。 |
| `cache_ttl_seconds` | 缓存 TTL；空结果会用更短 TTL（60s，且不超过该值）。 |
| `cache_max_results` | 单次响应最多缓存的结果条数，超出截断后写入；0 表示不截断。 |
| `compat_tavily_enabled` | 是否启用 Tavily-like 兼容接口。 |
| `compat_serper_enabled` | 是否启用 Serper-like 兼容接口。 |
| `compat_openai_enabled` | 是否启用 OpenAI-like 兼容接口。 |
| `api_auth_required` | `/v1/*` 是否需要 API Token 或管理员 API Key。 |
| `allow_private_extract_targets` | 是否允许 Extract 请求明显的私网、环回、链路本地或常见内网域名目标；默认 `false`，仅可信内网场景开启。 |
| `provider_health_window_minutes` | Provider 健康统计窗口。 |
| `provider_routing_strategy` | Provider 级路由策略：`fixed`、`priority`、`weighted`、`weighted_random`、`available_keys`、`random`。 |
| `log_retention_days` | 搜索日志和审计日志保留天数。 |

### 5.11 查看日志和审计日志

管理台的仪表盘、搜索日志和审计日志表格会限制在右侧内容区域内滚动，避免内容较多时触发浏览器全局滚动条。

搜索日志列表：

```bash
curl "$BASE_URL/api/admin/logs?limit=100" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

搜索日志详情：

```bash
curl "$BASE_URL/api/admin/logs/1" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

审计日志：

```bash
curl "$BASE_URL/api/admin/audit-logs?limit=100" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

### 5.12 搜索调试接口

```bash
curl -X POST "$BASE_URL/api/admin/playground/search" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "latest AI search API",
    "providers": ["exa", "you"],
    "mode": "parallel",
    "limit": 5,
    "cache": "bypass"
  }'
```

该接口与 `/v1/search` 使用相同的原生搜索请求格式，但作为管理台调试接口，不要求外部 API Token，也不会绑定 `api_token_id`。

## 6. 搜索与抽取接口调用示例

管理员 API Key 可直接调用搜索与抽取接口，不需要普通 Token 的 `search` / `extract` scopes。

### 6.1 原生搜索

```bash
curl -X POST "$BASE_URL/v1/search" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "latest web search APIs",
    "providers": ["exa", "you", "jina", "tavily", "firecrawl", "serper", "brave"],
    "mode": "parallel",
    "limit": 10,
    "cache": "default",
    "include_raw": false
  }'
```

### 6.2 原生 Extract

```bash
curl -X POST "$BASE_URL/v1/extract" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "urls": ["https://example.com/article"],
    "providers": ["exa", "jina", "tavily", "firecrawl"],
    "mode": "fallback",
    "format": "markdown"
  }'
```

完整字段和 Tavily Extract 兼容格式见 [extract.md](extract.md)。

### 6.3 Tavily-like Search

```bash
curl -X POST "$BASE_URL/v1/compat/tavily/search" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "latest web search APIs",
    "search_depth": "advanced",
    "max_results": 5,
    "include_raw_content": false,
    "providers": ["exa", "you"],
    "mode": "parallel",
    "cache": "default"
  }'
```

### 6.4 Tavily-like Extract

```bash
curl -X POST "$BASE_URL/v1/compat/tavily/extract" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "urls": "https://example.com/article",
    "query": "database migration",
    "chunks_per_source": 3,
    "extract_depth": "advanced",
    "timeout": 12.5,
    "include_usage": true
  }'
```

`timeout` 支持 1–60 秒及小数；`chunks_per_source` 使用时必须同时提供非空 `query`；`include_usage=true` 会请求响应中的 `usage.credits`。管理员 API Key 可直接调用，普通 Token 则必须拥有 `extract` scope。

### 6.5 Serper-like

```bash
curl -X POST "$BASE_URL/v1/compat/serper/search" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "q": "latest web search APIs",
    "num": 5,
    "gl": "us",
    "hl": "en",
    "providers": ["you"],
    "cache": "bypass"
  }'
```

### 6.6 OpenAI-like

```bash
curl -X POST "$BASE_URL/v1/compat/openai/responses-search" \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "input": "latest web search APIs",
    "limit": 5,
    "providers": ["exa", "jina"],
    "mode": "fallback"
  }'
```

### 6.7 查看 `/v1` Provider 和用量

```bash
curl "$BASE_URL/v1/providers" \
  -H "Authorization: Bearer $ADMIN_API_KEY"

curl "$BASE_URL/v1/usage/summary" \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

## 7. 状态码和错误格式

错误响应统一为：

```json
{
  "error": {
    "message": "admin login required",
    "status": 401
  }
}
```

常见状态码：

| 状态码 | 场景 |
| --- | --- |
| `400` | JSON 格式错误、路径参数错误、请求体非法。 |
| `401` | 缺少或使用了无效管理员 Session / 管理员 API Key / API Token。 |
| `403` | 普通 API Token 缺少 scope/Provider 权限（管理员 Key 不受这两项限制）；管理接口还会拒绝不可信 Origin 或 Cookie/登录调用缺少、错误的证明头。 |
| `404` | 兼容接口被禁用时返回；或反向代理未命中路径。 |
| `415` | 密码登录请求不是 JSON 媒体类型。 |
| `429` | 管理员登录失败次数过多，或普通 API Token 触发 RPM 限制。 |
| `502` | Provider Key 测试或官方额度查询时上游失败。 |
| `500` | 数据库、配置、加解密或内部服务错误。 |

## 8. 审计行为

使用管理员 API Key 调用管理接口时，部分写操作会写入 `audit_logs`。actor 形如：

```text
admin_api_key:oak_abcd
```

会记录的典型动作包括：

- `provider.update`
- `provider_key.create`
- `provider_key.update`
- `provider_key.test`
- `provider_key.quota`
- `provider_key.delete`
- `api_token.create`
- `api_token.update`
- `api_token.status`
- `api_token.delete`
- `settings.update`
- `settings.admin_api_key.rotate`
- `admin.logout`

## 9. 使用建议

- 管理员 API Key 适合 CI/CD、内部运维平台、自动化配置同步、可信后端服务调用。
- 不建议把管理员 API Key 放入浏览器前端、移动端或第三方不可控环境。
- 如只需要搜索能力，应创建仅含 `search` scope 的 `osr_` 外部 API Token，并用 `allowed_providers`、RPM、日/月额度做限制。
- Extract 默认校验目标 IP 和域名解析结果，但仍不要向不可信方授予 `extract` scope；自托管 Jina Reader、Firecrawl 或其它抓取服务时，应配置出口 ACL、连接时地址复核或目标白名单，防止 DNS 重绑定、重定向访问内网、云元数据和管理端点。
- 轮换管理员 API Key 前确认所有依赖方都能同步更新；轮换后旧 Key 会立即失效。
- 建议定期查看 `/api/admin/audit-logs`，确认管理操作来源符合预期。
