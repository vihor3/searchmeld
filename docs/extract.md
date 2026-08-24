# Extract 接口

One Search Relay 的 Extract 是“给定 URL，返回页面正文”的独立能力，不会先把 URL 当关键词执行搜索。它复用现有的 API Token、Provider Key 池、换 Key 重试、超时、代理、调用日志和用量统计。

## 支持的 Provider

| Provider | 上游能力 | 说明 |
| --- | --- | --- |
| `exa` | Contents | 支持一次提交多个 URL；可用 `query` 聚焦相关片段。 |
| `jina` | Reader | 网关按 URL 调用 Reader；默认读取基址为 `https://r.jina.ai`。 |
| `tavily` | Extract | 支持批量 URL、`extract_depth`、图片和 favicon。 |
| `firecrawl` | Scrape | 网关按 URL 调用 `/v2/scrape`，支持 Markdown、HTML 和 raw HTML。 |

`you`、`serper`、`brave` 当前只支持 Search，不能显式用于 Extract。Jina 如需使用自定义 Reader 代理，可在管理台“平台管理 → Jina → 高级”的“Reader 地址”中设置；底层配置键为 `settings.extract_base_url`，搜索仍继续使用 Provider 的 `base_url`。

## 原生接口

```text
POST /v1/extract
Authorization: Bearer osr_xxx
Content-Type: application/json
```

也可以使用 `X-API-Key: osr_xxx` 或管理员 API Key `oak_xxx`。

普通 `osr_` API Token 必须在 `scopes` 中显式包含 `extract`，否则返回 `403`。创建 Token 时不填写 scopes、只使用默认值，或从旧版本升级而来的既有 Token，都只有 `search`，不会因为升级自动获得正文抽取权限。可在管理台“API 令牌”中编辑 Token 并勾选“正文抽取”。管理员 API Key 不受普通 Token scope 限制，可以直接调用 Extract。

### 请求

```json
{
  "urls": [
    "https://example.com/article-a",
    "https://example.com/article-b"
  ],
  "providers": ["exa", "jina", "tavily", "firecrawl"],
  "mode": "fallback",
  "query": "只保留与数据库迁移有关的内容",
  "format": "markdown",
  "extract_depth": "advanced",
  "chunks_per_source": 3,
  "include_images": true,
  "include_favicon": true,
  "include_raw": false
}
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `urls` | string[] | 是 | - | 1–20 个绝对 `http` / `https` URL；不允许 URL 内嵌用户名或密码；重复 URL 会去重；默认拒绝明显的私网或保留地址。 |
| `providers` | string[] | 否 | 系统默认渠道中的 Extract 渠道 | 可选 `exa`、`jina`、`tavily`、`firecrawl`。 |
| `mode` | string | 否 | `fallback` | `fallback`、`parallel` 或 `single`。 |
| `query` | string | 否 | 空 | 聚焦内容的提示；仅在上游支持时生效。 |
| `format` | string | 否 | `markdown` | `markdown`、`text`、`html`、`raw_html`；上游不支持时会返回它能提供的最接近格式，并在结果的 `format` 标明实际格式。 |
| `extract_depth` | string | 否 | 上游默认 | `basic` 或 `advanced`；主要供 Tavily 使用。 |
| `chunks_per_source` | integer | 否 | 上游默认 | 提供时为 1–5，并且必须同时提供非空 `query`；主要供查询聚焦型抽取使用。 |
| `include_images` | boolean | 否 | `false` | 请求上游返回图片 URL；具体能力依 Provider 而异。 |
| `include_favicon` | boolean | 否 | `false` | 请求 favicon；具体能力依 Provider 而异。 |
| `include_raw` | boolean | 否 | `false` | 在每条结果中附带归一化前的上游条目。 |
| `options` | object | 否 | `{}` | Provider 专用高级参数，例如 Exa 的 `max_characters`、Firecrawl 的 `only_main_content`。 |

模式语义：

- `fallback`：按路由顺序调用 Provider。已经成功抽取的 URL 不再重复调用，只把未成功的 URL 交给下一个 Provider。
- `parallel`：所有选定 Provider 并发抽取全部 URL；同一 URL 会合并 Provider 归因，正文优先采用路由顺序靠前的结果。
- `single`：只调用路由后的第一个 Provider。

Jina Reader 和 Firecrawl 的上游接口一次只处理一个 URL。网关会按请求顺序串行调用这些 URL；每个 URL 都单独获取 Key、重试、计量和记录调用日志。一旦出现无效 Key、限流或上游故障等系统性错误，会保留已成功的结果并停止该 Provider 的剩余 URL。原生接口的总预算会按 URL 数量扩展，保证每个 URL 至少有一次完整的 Provider 超时窗口；换 Key 重试共享这份总预算，不会把最坏耗时再乘以最大重试次数。20 个 URL 的多 Provider `fallback` 在最坏情况下可能需要十几分钟，调用方也应配置足够长的 HTTP 超时。

Extract 默认不写搜索缓存，确保每次向上游获取当前页面内容。

为避免批量页面把网关内存或 PostgreSQL 日志撑爆，响应有明确大小预算：单条正文最多 512 KiB，单次原生响应的正文合计最多 8 MiB，最终 JSON 最多约 12 MiB；超过时会按各结果公平缩减，并在原生/MCP 结果中设置 `content_truncated: true`，同时在 `meta.response_truncated` 汇总标记。单条 `raw` 超过 64 KiB 时会替换为截断标记并设置 `raw_truncated: true`；图片列表也受数量和总字节限制，并通过 `images_truncated` 标记。Tavily 兼容响应保持其字段结构，因此只返回已受预算约束的 `raw_content`，不会额外加入这些 One Search 扩展标记。请求日志只保存更短的正文/图片预览，单条 Extract 日志 JSON 另有 4 MiB 硬上限。

### 响应

```json
{
  "results": [
    {
      "url": "https://example.com/article-a",
      "title": "Article A",
      "content": "# Extracted content",
      "format": "markdown",
      "provider": "exa",
      "providers": ["exa"],
      "images": ["https://example.com/cover.png"]
    }
  ],
  "failed_results": [
    {
      "url": "https://example.com/article-b",
      "error_type": "upstream",
      "error": "exa: ...; jina: ..."
    }
  ],
  "providers": [
    {
      "provider": "exa",
      "status": "success",
      "latency_ms": 420,
      "result_count": 1,
      "cached": false
    }
  ],
  "usage": [
    {
      "unit": "credits",
      "quantity": 1,
      "metadata": { "provider": "tavily" }
    }
  ],
  "meta": {
    "request_id": "req_xxx",
    "mode": "fallback",
    "compat_format": "native",
    "latency_ms": 430,
    "total_results": 1,
    "total_failed": 1,
    "providers_queried": ["exa", "jina"]
  }
}
```

批量请求允许部分成功：HTTP 请求本身成功时返回 `200`，逐 URL 的失败放在 `failed_results`。请求参数不合法时返回 `400`；普通 Token 缺少 `extract` scope 或请求未授权 Provider 时返回 `403`。所有 Provider 均出现系统性失败时不伪装成部分成功：无可用 Key / Provider 返回 `503`，上游认证或协议错误返回 `502`，限流或额度耗尽返回 `429`，整体超时返回 `504`。

## Tavily Extract 兼容接口

```text
POST /v1/compat/tavily/extract
```

该接口受运行设置 `compat_tavily_enabled` 控制，鉴权方式与原生接口相同。`urls` 同时接受单个字符串和字符串数组：

```bash
curl -X POST http://localhost:5173/v1/compat/tavily/extract \
  -H "Authorization: Bearer osr_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "urls": "https://example.com/article",
    "query": "database migration",
    "chunks_per_source": 3,
    "extract_depth": "advanced",
    "format": "markdown",
    "include_images": true,
    "timeout": 12.5,
    "include_usage": true
  }'
```

主要兼容参数：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `urls` | string 或 string[] | 单个 URL 或 1–20 个 URL。 |
| `query` | string | 可选的内容聚焦查询。 |
| `chunks_per_source` | integer | 1–5；使用时必须同时提供非空 `query`。 |
| `extract_depth` | string | `basic` 或 `advanced`。 |
| `format` | string | `markdown` 或 `text`。 |
| `include_images` | boolean | 请求图片 URL。 |
| `include_favicon` | boolean | 请求 favicon URL。 |
| `timeout` | number | 1–60 秒，可以使用小数。它会传给 Tavily，并限制整个兼容请求（包括走其它 Provider 的 fallback）；网关会额外保留少量 HTTP 收尾时间。 |
| `include_usage` | boolean | 为 `true` 时在响应中加入 `usage.credits`；为 `false` 或省略时不返回 `usage`。 |
| `providers` / `mode` | string[] / string | One Search Relay 扩展，用于控制渠道和编排模式。 |

响应字段为 Tavily 风格的 `results[].raw_content`、`failed_results`、`response_time` 和 `request_id`。`include_usage=true` 时还会返回本次各上游 credit 计量之和；无法提供 credit 计量的渠道不会凭空估算。

## MCP 工具

启用 MCP 后，`tools/list` 会同时返回 `search` 和 `extract`。调用示例：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "extract",
    "arguments": {
      "urls": ["https://example.com/article"],
      "mode": "fallback",
      "format": "markdown"
    }
  }
}
```

MCP 返回的 `content` 文本只提供有界预览，避免把多个页面的完整正文重复塞入模型文本上下文；完整、未按 MCP 预览截断的正文位于 `structuredContent.results[].content`。`structuredContent` 的整体格式与原生 `/v1/extract` 响应一致。普通 Token 调用该工具同样需要 `extract` scope。完整 MCP 配置见 [mcp.md](mcp.md)。

## 安全边界

Extract 接受调用方提供的绝对 HTTP(S) URL。默认情况下，网关会拒绝 localhost、URL 中的私网/环回/链路本地/非全局单播 IP、运营商级 NAT 地址和常见内网域名后缀；普通域名会在调用上游前并发解析，只要任一地址不是公网地址或无法安全解析，请求就会被拒绝。确有可信内网抓取需求时，可在管理台“系统设置 → 安全”开启 `allow_private_extract_targets`。

DNS 校验与上游实际抓取之间仍存在时间差，无法单独消除 DNS rebinding，也不等同于完整目标白名单：

- 不要向不可信用户、浏览器前端或无法约束的第三方授予 `extract` scope；只需要搜索时保留默认的 `search` scope。
- 自托管 Jina Reader、Firecrawl 或其它负责实际抓取的服务时，应在抓取服务所在网络配置出口 ACL、HTTP 代理规则或目标域名白名单。
- 抓取服务侧仍应在建立连接时再次检查最终地址，避免重绑定或重定向落入私网；至少阻止云实例元数据地址以及 Kubernetes / Docker 管理端点。
- 抓取服务不要与数据库、宿主机管理端口或控制平面共享不受限制的网络权限。
