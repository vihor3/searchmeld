# SearchMeld

自托管 Web Search / Extract API 中转与聚合网关。

SearchMeld 是基于 One Search 演进的独立项目，不属于 GitHub fork network，也不会自动合并上游代码；后续更新会先审查，再按需选择性引入。具体流程见 [UPSTREAM.md](UPSTREAM.md)。

统一接入 Exa、You.com、Jina、Tavily、Firecrawl、Serper、Brave，提供：

- 统一搜索接口 `POST /v1/search`（`parallel` / `fallback` / `single`）
- URL 内容抽取接口 `POST /v1/extract`，支持 Exa、Jina、Tavily、Firecrawl
- Tavily Search / Extract、Serper、OpenAI 兼容接口
- Web 管理台：Provider、Key、Token、调试、日志、用量、审计
- 可选 MCP（`search` / `extract` 工具）

预览图
<p>
  <img src="./docs/images/搜索调试.png" alt="搜索调试" width="132" />
  <img src="./docs/images/仪表盘.png" alt="仪表盘" width="120" />
</p>

## 快速部署

需要 Docker 24+ / Compose v2，以及至少一个上游 Provider API Key。

### 一键安装

一键安装脚本不需要设置环境变量，也不接受安装参数。直接运行：

```bash
git clone https://github.com/vihor3/searchmeld.git
cd searchmeld
./install.sh
```

向导会依次引导完成：

1. 选择内置 PostgreSQL，或已有的外部 PostgreSQL 15+。
2. 外部数据库可选择加入另一个 Docker 网络，并填写网络名称。
3. 外部数据库连接信息可按主机、端口、库名、用户名、密码和 SSL 模式逐项填写，也可直接粘贴完整 `DATABASE_URL`。密码和连接串采用隐藏输入。
4. 设置管理台端口、管理员账号、管理员密码生成方式，以及是否启用 MCP。
5. 选择是否为应用容器设置两台自定义 DNS；默认关闭，继续使用 Docker/宿主机 DNS。
6. 查看不含密码的配置摘要并确认安装。

内置数据库密码和 `ENCRYPTION_KEY` 由脚本自动安全生成。安装完成后脚本会等待健康检查通过，并显示管理台地址和自动生成的首次管理员密码。

配置保存在权限为 `600` 的 `.env` 中。重复运行 `./install.sh` 时：

- 如果当前目录是配置了 upstream 的 Git 仓库，可选择从当前分支的上游远端 fast-forward 更新。脚本会保留 `.env` 和数据库数据，先执行 `docker compose build --pull`；构建成功后才切换容器并等待健康检查。
- 也可以不拉取远端代码，仅使用当前版本重新构建启动，或者重新进入配置向导。
- 自动更新不会覆盖未提交的已跟踪文件修改；检测到本地修改或分支历史已经分叉时会停止，并要求先手动处理 Git 状态。
- 使用源码压缩包等非 Git 方式安装时，仍可使用当前版本重建和重新配置，但不会显示远端更新选项。

已有持久化密码、`ENCRYPTION_KEY`、内置 PostgreSQL Volume 和外部数据库连接不会在更新时被静默替换。

### 数据库升级与恢复

构建入口统一为根目录 `Dockerfile`：`all-in-one` 内置数据库，`external` 连接外部数据库。未被现有部署引用的旧 `Dockerfile.backend` / `Dockerfile.frontend` 已删除；自定义构建脚本请迁移到上述目标。

内置数据库固定为 **PostgreSQL 16**，不会跟随 Alpine 默认包自动升级大版本。启动前检查服务器版本和数据目录的 `PG_VERSION`；不同大版本、损坏的标记或非空但缺少标记的目录会被拒绝，不执行初始化或修改目录所有权。

升级前保留旧镜像和完整 `.env`（尤其是 `ENCRYPTION_KEY`），停止应用并确认 PostgreSQL 已干净关闭后再复制数据库卷，且验证备份可以恢复；仅停止业务写入不足以安全复制运行中的数据库目录。普通应用升级继续使用原数据卷；不要删除或编辑 `PG_VERSION` 来强行启动。大版本不匹配时，用匹配的旧 PostgreSQL 恢复服务或导出数据，再向新的兼容卷恢复，确认账号、Key 和业务数据后再切换。不要让升级或回滚直接操作唯一的数据副本。

自定义 DNS 只建议在 Docker DNS 间歇出现 `server misbehaving`、SERVFAIL 或超时时启用。Mihomo Fake-IP、企业内网和域名分流环境通常依赖宿主机 DNS，应保留默认关闭状态。启用后向导会要求填写两台不同的 IPv4/IPv6 DNS 地址，并为所有数据库部署方式叠加 `docker-compose.dns.yml`。

### 从 One Search 迁移

现有源码安装可以保留原目录名、`.env`、数据库和 Docker Volume，只把 Git 远端切换到 SearchMeld：

```bash
git remote set-url origin https://github.com/vihor3/searchmeld.git
git fetch origin
git switch main
git merge --ff-only origin/main
./install.sh
```

安装脚本继续识别旧版 `ONE_SEARCH_INSTALL_MODE`、`ONE_SEARCH_USE_SHARED_DB_NETWORK` 和代理变量；重新运行配置向导后会写入新的 `SEARCHMELD_*` 安装键。已有 `osr_` Token、`oak_` 管理员 Key、`one_search` 数据库和管理员账号不需要重建。管理台升级后先刷新旧页面，再登录一次；旧 `adm_` 请求头脚本的迁移见下节。

### 手动安装

```bash
git clone https://github.com/vihor3/searchmeld.git
cd searchmeld
cp .env.example .env
```

编辑 `.env`，至少填写：

```dotenv
POSTGRES_PASSWORD=强密码
ADMIN_PASSWORD=管理员密码
ENCRYPTION_KEY=至少32字符   # openssl rand -base64 32
```

```bash
docker compose up --build -d
curl http://localhost:5173/healthz
```

打开 <http://localhost:5173>，用管理员账号登录。

### 管理台登录与 HTTPS 反代

网页仍使用管理员账号和密码登录。一次登录可供同一浏览器配置中的独立标签页共享；管理接口每次检查服务端会话，页面进入和刷新也会检查 `/api/admin/me`。删除 Cookie、会话过期、后端重启或成功退出后，后续管理请求会被拒绝；空闲页面可能保留已显示的内容，直到下一次请求或导航。退出失败会显示可重试错误，不表示服务端会话已注销。

登录设置 `searchmeld_admin_session`：仅当前主机、`Path=/api/admin`、`HttpOnly`、`SameSite=Lax`，不设置持久化期限。服务端固定有效期默认 24 小时，不滑动续期；浏览器恢复会话也不会延长服务端期限。退出只撤销服务端会话，不清除 Cookie，以免延迟响应覆盖另一个标签页的新登录；留下的旧 Cookie 不再有效。网页不会从 sessionStorage/localStorage 恢复凭据，也不会发送管理员 Bearer Token。

- **直接 HTTP**：可信本机/内网可继续使用 `http://主机:端口`，`ADMIN_PUBLIC_ORIGIN` 留空时按实际连接和保留端口的 Host 校验。HTTP 不加密密码或 Cookie，不适合公网。
- **HTTPS 反代**：在启用公网入口前，在 `.env` 设置真实外部源，例如 `ADMIN_PUBLIC_ORIGIN=https://search.example.com`，非默认端口则写 `https://search.example.com:8443`。内置和外部数据库两种 Compose 都会传给后端，并据此设置 Cookie `Secure`。仅设置 `X-Forwarded-Proto` 或 `APP_ENV=production` 不能替代此配置；漏配时 HTTPS 页面的登录 Origin 校验会失败，不要移除 Origin/证明头来绕过。
- 该值只能是单个 HTTP(S) 源，不能含账号、子路径、查询、片段或通配符；允许末尾 `/`。部署应使用同一个规范入口，不把管理台挂到子路径。改动后须重新创建应用容器，仅重启旧容器不会更新环境。

例如内置数据库模式在编辑 `.env` 后执行 `docker compose up -d --force-recreate app`；外部数据库用 `docker compose -f docker-compose.external-db.yml up -d --force-recreate app`，原部署使用的共享网络/DNS overlay 仍按原顺序带上。一键安装也使用同一 `.env`，升级前先添加 HTTPS 配置；安装向导保留该非向导管理项，不新增账号或 Key 初始化步骤。

外层 HTTPS Nginx 在已有 TLS server 中转发整个站点，并保留原始 Host（含端口）：

```nginx
location / {
    proxy_pass http://127.0.0.1:5173;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host "";
    proxy_set_header X-Forwarded-Port "";
    proxy_set_header Forwarded "";
    proxy_set_header True-Client-IP "";
}
```

将上游端口替换为 `HOST_PORT`，证书与 TLS 监听由外层代理配置；避免把内部 HTTP 端口直接暴露到公网。内置 Nginx 覆盖客户端 IP 转发头，后端只信任环回 socket 对端提供的单个 `X-Real-IP`，忽略 `True-Client-IP`/XFF。独立外层代理的用户可能共用该代理的登录限速桶，不会自动信任整条代理链。

Cookie 不按 TCP 端口隔离，不要让不可信应用与管理台共享主机名。所有 Cookie 管理请求（包括 GET）和密码登录必须带 `X-SearchMeld-Admin: 1`；自定义客户端使用 `credentials: 'include'`。若携带 Origin，只允许规范源或 `CORS_ALLOWED_ORIGINS` 中明确配置的精确可信源；管理接口忽略 `*`，不反射任意/null Origin。同站点分离前端需显式配置完整源（含端口），跨站点嵌入不在支持范围。管理响应统一禁止缓存。

**升级迁移**：`POST /api/admin/login` 保留，但 JSON 仅返回 `{expires_at}`，不再返回 `token`，`adm_` 不再支持 Bearer 或 `X-API-Key` 管理鉴权。已打开的旧管理页面仍运行期待 token JSON 的旧 JavaScript，必须先刷新页面加载新版，再登录一次；只在旧表单中重新输入密码不够。新版页面和新标签页共享 Cookie。脚本改用已有 `oak_` 请求头；首次生成 Key 可在“系统设置”操作，也可通过 Cookie jar + 证明头调用原有登录和 Key 接口，见 [管理员 API Key 文档](docs/admin-api-key.md)。这不是浏览器专属的 HTTP 登录接口。已安装的 `oak_`、`osr_` 和账号数据保持不变；网页 Cookie 不授权业务 `/v1/*` 或需要鉴权的 MCP 调用。Key 轮换不注销浏览器会话，浏览器退出也不撤销集成 Key。

## 使用外部 PostgreSQL

后端原生读取 `DATABASE_URL`。`external` 镜像默认使用外部数据库，不会初始化本地数据目录或覆盖连接串；`all-in-one` 镜像默认仍使用内置数据库，避免升级时被 `.env` 中遗留的开发连接串意外切换。外部数据库要求 PostgreSQL 15+，因为迁移使用了 `UNIQUE NULLS NOT DISTINCT`。

数据库驱动已从 pgx v4 升为 v5，普通连接串无需修改。若曾配置 `prefer_simple_protocol` 或 `statement_cache_mode`，启动会给出迁移错误，不会静默改变查询协议。默认使用扩展协议；需要 simple protocol 时显式设置 `default_query_exec_mode=simple_protocol`。旧 `statement_cache_mode=describe` 对应 `default_query_exec_mode=cache_describe` 及正数 `description_cache_capacity`；关闭缓存应使用 `default_query_exec_mode=describe_exec`，不要仅把默认模式的缓存容量设为 0。

先创建项目专用账号和数据库，确保该账号拥有目标数据库及 schema，能够执行建表、索引和后续迁移：

安装向导新建外部数据库连接时，默认数据库名为 `searchmeld_db`、用户名为 `searchmeld`。已有 One Search 部署会继续沿用 `.env` 中保存的连接串，不需要迁移数据库。

```sql
CREATE ROLE searchmeld LOGIN;
\password searchmeld
CREATE DATABASE searchmeld_db OWNER searchmeld;
```

如果数据库运行在另一个 Compose 项目的 `shared-db` 网络中，在 `.env` 中填写：

```dotenv
DATABASE_URL=postgres://searchmeld:URL编码后的密码@shared-postgres:5432/searchmeld_db?sslmode=disable
DATABASE_DOCKER_NETWORK=shared-db
ADMIN_PASSWORD=管理员密码
ENCRYPTION_KEY=至少32字符
```

确保共享网络已经存在，然后组合外部数据库和共享网络两个 Compose 文件启动：

```bash
docker network inspect shared-db >/dev/null 2>&1 || docker network create --internal shared-db
docker compose \
  -f docker-compose.external-db.yml \
  -f docker-compose.shared-db.yml \
  up --build -d
curl http://localhost:5173/healthz
```

`docker-compose.external-db.yml` 会构建 Dockerfile 的 `external` 目标。该目标只包含前端、后端和 Nginx，不安装 PostgreSQL server、client 或 `su-exec`，也不声明数据库数据卷。`docker-compose.shared-db.yml` 只负责把应用接入已有的 `shared-db` 网络。

数据库位于远程服务器或宿主机时，只使用第一个文件即可，不要求 `shared-db` 网络：

```bash
docker compose -f docker-compose.external-db.yml up --build -d
```

手动部署需要自定义容器 DNS 时，在 `.env` 填写两台地址，并把 DNS 配置作为最后一个 Compose 文件叠加：

```dotenv
SEARCHMELD_USE_CUSTOM_DNS=true
SEARCHMELD_DNS_PRIMARY=223.5.5.5
SEARCHMELD_DNS_SECONDARY=1.12.12.12
```

```bash
docker compose \
  -f docker-compose.external-db.yml \
  -f docker-compose.shared-db.yml \
  -f docker-compose.dns.yml \
  up --build -d
```

不使用共享数据库网络时去掉 `docker-compose.shared-db.yml`；使用内置数据库时把前两个文件替换为 `docker-compose.yml`。禁用自定义 DNS 时不要加载 `docker-compose.dns.yml`。

镜像构建由 GitHub Actions 执行，保留 `external` 和 `all-in-one` 两个构建目标。

`all-in-one` 镜像也保留了连接外部数据库的能力，但必须显式传入 `DATABASE_MODE=external` 和 `DATABASE_URL`；它仍然包含 PostgreSQL 软件及数据卷声明。希望镜像和运行配置都不包含内置数据库时，请使用 `external` 构建目标或专用 Compose 文件。

外部数据库暂时不可用时后端会退出，Compose 的 `restart: unless-stopped` 会继续重启；健康检查只有在数据库和 HTTP 服务均可用后才会通过。多副本部署时建议只让一个实例执行迁移，其他实例设置 `RUN_MIGRATIONS=false`。

## 首次配置

1. **平台管理**：启用要用的 Provider  
2. **Key 管理**：添加上游 API Key  
3. **搜索调试**：验证可用性  
4. **API 令牌**：创建业务用的 `osr_...` Token；需要正文抽取时同时勾选 `extract` 权限

### Provider 额度与本地统计

管理台会区分官方余额、官方用量、上游响应、响应头快照和本地统计。所有经过本实例的成功 Provider 调用都会按 Key 记录 requests、credits、tokens 和 USD；上游未返回计量时，使用 Provider 设置或内置公开单价估算费用。

- Tavily、Firecrawl、You.com 使用普通 API Key 查询官方额度。
- Exa 配置 Team Management 密钥时查询官方周期用量；未配置时显示本实例累计费用。
- Jina 优先解析根地址返回的余额；格式不可识别时回退到本地累计 tokens。
- Serper 没有公开余额接口，按注册赠送的 2500 credits 减本实例全生命周期累计 credits 估算，不会按月重置。
- Brave 从正常搜索响应的 `X-RateLimit-*` headers 被动保存额度快照；点击查询不会额外发起一次收费搜索，没有快照时显示本地累计请求数。

本地统计只覆盖经过当前 SearchMeld 数据库记录的调用，无法感知同一个上游 Key 在其它客户端中的消费；这类结果会在管理台标记为“本地”或“本地估算”。

## 使用

```bash
curl -X POST http://localhost:5173/v1/search \
  -H "Authorization: Bearer osr_你的令牌" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "golang web search",
    "mode": "fallback",
    "limit": 5,
    "providers": ["brave", "tavily"]
  }'
```

也可用 `X-API-Key: osr_xxx`。

已知 URL 的正文抽取：

> 普通 `osr_` Token 必须在 `scopes` 中显式包含 `extract`。新建 Token 未选择权限、既有 Token 或默认 Token 都只有 `search`，升级后不会自动获得 Extract 权限；管理员 API Key `oak_...` 可以直接调用。

```bash
curl -X POST http://localhost:5173/v1/extract \
  -H "Authorization: Bearer osr_你的令牌" \
  -H "Content-Type: application/json" \
  -d '{
    "urls": [
      "https://example.com/article-a",
      "https://example.com/article-b"
    ],
    "mode": "fallback",
    "providers": ["exa", "jina", "tavily", "firecrawl"],
    "format": "markdown",
    "include_images": true
  }'
```

`fallback` 是 Extract 的默认模式：第一个渠道成功的 URL 不会再请求后续渠道，只把失败或缺失的 URL 继续向后补齐。单次最多 20 个绝对 `http` / `https` URL。完整参数和响应见 [docs/extract.md](docs/extract.md)。

安全提示：Extract 默认拒绝明显的私网、环回、链路本地和常见内网域名目标，并在调用上游前检查域名解析结果；可信内网场景可在“系统设置 → 安全”显式开启。不要向不可信用户授予 `extract` scope；自托管 Jina Reader、Firecrawl 或其它抓取服务时，仍应在抓取服务一侧配置出口 ACL、连接时地址复核或目标白名单，防止 DNS 重绑定、重定向访问云元数据和集群管理地址。

| 路径 | 说明 |
| --- | --- |
| `/` | 管理台 |
| `/healthz` | 健康检查 |
| `/v1/search` | 统一搜索 |
| `/v1/extract` | 统一 URL 内容抽取 |
| `/v1/compat/tavily/search` | Tavily 兼容 |
| `/v1/compat/tavily/extract` | Tavily Extract 兼容 |
| `/v1/compat/serper/search` | Serper 兼容 |
| `/v1/compat/openai/responses-search` | OpenAI 兼容 |
| `/mcp` | MCP（默认开启） |

两个 Tavily 兼容接口除 `Authorization: Bearer` 和 `X-API-Key` 外，也接受旧版 Tavily 客户端使用的 JSON `api_key` 字段。请求头与 JSON 同时存在时优先使用请求头；该兼容形式不会应用到 `/v1/search`、`/v1/extract` 或其它接口。

原生、兼容搜索和 MCP 搜索均执行普通 Token 的 `allowed_providers` 限制；省略 `providers` 不会绕过限制。平台完整配置和全站统计仅保留在 `/api/admin/providers`、`/api/admin/usage/summary`，原 `/v1/providers`、`/v1/usage/summary` 已移除；旧脚本应改用管理员 Key 调用管理接口。

完整接口见 [docs/extract.md](docs/extract.md)、[docs/admin-api-key.md](docs/admin-api-key.md)、[docs/mcp.md](docs/mcp.md)。

安全边界、源码审计发现及尚未解决的风险见 [安全审查报告](docs/security-review.md)。

## MCP 配置

`.env.example` 默认 `MCP_ENABLED=true`，端点：

```text
http://localhost:5173/mcp
```

先在管理台创建 `osr_...` Token（`API_AUTH_REQUIRED=true` 时必须）。需要同时使用 `search` 和 `extract` 工具时，Token 的 scopes 必须同时包含 `search` 和 `extract`；默认及既有 Token 不会自动增加 `extract`。自检：

```bash
curl http://localhost:5173/mcp
# 应返回 enabled:true、tools:["search","extract"]
```

### Codex

```bash
export SEARCHMELD_API_TOKEN=osr_xxx
```

写入 `~/.codex/config.toml`：

```toml
[mcp_servers.searchmeld]
url = "http://localhost:5173/mcp"
bearer_token_env_var = "SEARCHMELD_API_TOKEN"
enabled = true
tool_timeout_sec = 60
enabled_tools = ["search", "extract"]
```

启动 Codex 后输入 `/mcp`，应能看到 `searchmeld` 的 `search` 和 `extract`。

### Claude Desktop / 通用 HTTP MCP

多数客户端填：

```json
{
  "url": "http://localhost:5173/mcp",
  "headers": {
    "Authorization": "Bearer osr_xxx"
  }
}
```

更多参数、错误码、排错见 [docs/mcp.md](docs/mcp.md)。

## 配置说明

### 常用（`.env.example` 已列出）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `HOST_PORT` | `5173` | 宿主机端口 |
| `SEARCHMELD_INSTALL_MODE` | `embedded` | 安装脚本记录的数据库模式；通常由向导维护 |
| `SEARCHMELD_USE_SHARED_DB_NETWORK` | `false` | 安装脚本是否组合共享数据库网络配置 |
| `SEARCHMELD_USE_CUSTOM_DNS` | `false` | 是否由安装脚本叠加自定义容器 DNS；Mihomo Fake-IP/内网分流场景通常保持关闭 |
| `SEARCHMELD_DNS_PRIMARY` / `SEARCHMELD_DNS_SECONDARY` | 空 | 启用自定义 DNS 时使用的两台不同 IPv4/IPv6 地址 |
| `TZ` | `Asia/Shanghai` | 应用、Nginx 和内置 PostgreSQL 的容器时区；可改为 `UTC` 或其它有效 IANA 时区 |
| `POSTGRES_PASSWORD` | — | 使用内置 PostgreSQL 时**必填**；外部数据库模式不需要 |
| `DATABASE_URL` | 空 | 外部 Compose 或容器直接运行时使用的 PostgreSQL 连接串 |
| `DATABASE_MODE` | 取决于镜像 | `all-in-one` 默认 `embedded`，`external` 默认 `external`；一体化镜像切外部库时须显式设为 `external` |
| `ADMIN_USERNAME` | `admin` | 首次管理员用户名 |
| `ADMIN_PASSWORD` | — | 生产**必填** |
| `ADMIN_PUBLIC_ORIGIN` | 空 | 管理台规范外部 HTTP(S) 源；直接 HTTP 可留空，HTTPS 反代必须填写（含非默认端口） |
| `ENCRYPTION_KEY` | — | **必填**，≥32 字符，加密敏感 Key |
| `API_AUTH_REQUIRED` | `true` | 遗留环境字段；实际 `/v1/*`、MCP 鉴权以管理台保存的 `api_auth_required` 为准（默认开启），不能靠此环境值覆盖已存策略 |
| `MCP_ENABLED` | `true` | 是否开启 MCP |

### 可选（一般不用改，需要时加到 `.env`）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `APP_ENV` | Compose 下 `production` | 生产请保持 `production` |
| `POSTGRES_DB` / `POSTGRES_USER` | `one_search` | 库名 / 用户 |
| `HTTP_ADDR` | `:8080` | 仅独立后端部署可改；根 Dockerfile 的 Nginx 和健康检查固定使用 `8080` |
| `MCP_PATH` | `/mcp` | MCP 路径 |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:8080` | CORS 白名单；管理接口仅接受精确可信源并忽略 `*`，公网请移除不需要的开发源 |
| `DATABASE_URL_FILE` | 空 | 外部连接串在容器内的文件路径，适合 Docker Secret；需要自行挂载，与 `DATABASE_URL` 二选一 |
| `DATABASE_DOCKER_NETWORK` | `shared-db` | 外部数据库 Compose 使用的 Docker 网络 |
| `RUN_MIGRATIONS` | `true` | 启动时自动迁移 |
| `REQUEST_TIMEOUT_MS` | `20000` | 上游请求超时 |
| `REQUEST_BODY_LIMIT_BYTES` | `1048576` | 请求体上限 |
| `SERVER_*_TIMEOUT_MS` | 见代码默认 | HTTP 服务器超时 |
| `ADMIN_SESSION_TTL_HOURS` | `24` | 服务端固定管理会话时长；不随请求续期，后端重启即失效；自定义值须传入后端进程环境 |
| `ADMIN_LOGIN_MAX_ATTEMPTS` 等 | 5 / 5min / 15min | 登录限速与锁定 |
| `VITE_API_BASE` | 空 | 前后端分离开发时指向后端 |
| `SEARCHMELD_HTTP(S)_PROXY` | 空 | 容器访问上游时的代理；兼容旧版 `ONE_SEARCH_HTTP(S)_PROXY` |

公网请按上文配置 HTTPS 反代及 `ADMIN_PUBLIC_ORIGIN`，转发 `/`、`/api/`、`/v1/`、`/healthz`（以及 `/mcp`）。

## 开发与验证

**硬约束：本地只写代码，CI、测试、编译和打包全部在 GitHub Actions 的远端 runner 执行。**

- 本地仅进行源码和文档的阅读、编辑、代码审阅、Git 操作及必要的任务记录维护。
- 依赖安装、自动化测试、lint/格式检查、类型检查、编译、前端构建、打包、镜像构建和浏览器运行验证均交给 [GitHub Actions](.github/workflows/ci.yml)。
- 不得用本地 Docker、`act`、本机 self-hosted runner、后台进程或子代理绕过该规则，也不在本地启动开发服务进行验证。
- 验证结果必须对应具体提交 SHA 和远端 Actions 作业；尚未推送或远端执行失败时，明确标记待验证，不回退到本地执行。
- 可在明确授权后先提交、推送验证版本以触发 CI；只有对应远端检查通过后，才能标记验证完成。
- 工作流只负责验证和构建，不自动发布生产镜像或部署；提交、推送和发布仍按明确授权执行。

管理会话回归分为前端 mock 场景与真实打包集成：`deploy/admin_cookie_integration_test.cjs` 仅允许在 GitHub Actions 执行，覆盖 `all-in-one` 内置库和 `external` 独立 PostgreSQL，在直接 HTTP 和临时 TLS 代理中验证 Cookie、独立标签页、Origin/CSRF 及旧响应竞态。精确 TTL 由后端测试覆盖，不通过生产测试端点加速过期。

`deploy/database_upgrade_test.cjs` 在隔离卷中从固定的旧代码提交构建历史镜像，验证新镜像保留旧账号、管理员 Key、业务 Token 和持久化数据，并检查不兼容数据库目录的拒绝与恢复。它不是生产自动迁移程序，不接触现网数据库。

依赖检查覆盖完整 npm 锁文件、`govulncheck` 调用可达性和最终镜像的系统/Go 依赖。npm 和镜像从 low 级别开始阻断，不统一忽略未修复漏洞；下载或扫描失败也不能视为通过。前端和镜像均使用 `npm ci`，锁文件更新只在 Actions 生成并经代码审阅后提交。CI 保留 7 天的漏洞报告与合成 PNG，不上传 Cookie jar、数据库备份、凭据或追踪文件，并清理临时容器、卷、证书及浏览器资源。

这条约束适用于开发人员和所有编码代理，并替代旧的本地 Docker 验证说明。现有部署入口未因此调整。

## 目录

```text
backend/     Go API + migrations
frontend/    Vue 管理台
deploy/      all-in-one 入口 + nginx
docs/        接口文档
```

## License

[Apache License 2.0](LICENSE)。SearchMeld 保留原项目版权与许可证声明，并在 [NOTICE](NOTICE) 中记录来源和独立修改关系。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区。
