# One Search

自托管 Web Search / Extract API 中转与聚合网关。

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
git clone https://github.com/CncCbz/one-search.git
cd one-search
./install.sh
```

向导会依次引导完成：

1. 选择内置 PostgreSQL，或已有的外部 PostgreSQL 15+。
2. 外部数据库可选择加入另一个 Docker 网络，并填写网络名称。
3. 外部数据库连接信息可按主机、端口、库名、用户名、密码和 SSL 模式逐项填写，也可直接粘贴完整 `DATABASE_URL`。密码和连接串采用隐藏输入。
4. 设置管理台端口、管理员账号、管理员密码生成方式，以及是否启用 MCP。
5. 查看不含密码的配置摘要并确认安装。

内置数据库密码和 `ENCRYPTION_KEY` 由脚本自动安全生成。安装完成后脚本会等待健康检查通过，并显示管理台地址和自动生成的首次管理员密码。

配置保存在权限为 `600` 的 `.env` 中。重复运行 `./install.sh` 时，可以选择沿用现有配置启动/更新，也可以重新进入配置向导；已有持久化密码和 `ENCRYPTION_KEY` 不会被静默替换。

### 手动安装

```bash
git clone https://github.com/CncCbz/one-search.git
cd one-search
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

## 使用外部 PostgreSQL

后端原生读取 `DATABASE_URL`。`external` 镜像默认使用外部数据库，不会初始化本地数据目录或覆盖连接串；`all-in-one` 镜像默认仍使用内置数据库，避免升级时被 `.env` 中遗留的开发连接串意外切换。外部数据库要求 PostgreSQL 15+，因为迁移使用了 `UNIQUE NULLS NOT DISTINCT`。

先创建项目专用账号和数据库，确保该账号拥有目标数据库及 schema，能够执行建表、索引和后续迁移：

```sql
CREATE ROLE one_search LOGIN;
\password one_search
CREATE DATABASE one_search OWNER one_search;
```

如果数据库运行在另一个 Compose 项目的 `shared-db` 网络中，在 `.env` 中填写：

```dotenv
DATABASE_URL=postgres://one_search:URL编码后的密码@shared-postgres:5432/one_search?sslmode=disable
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

也可以直接构建镜像：

```bash
# 轻量镜像：必须提供 DATABASE_URL 或 DATABASE_URL_FILE
docker build --target external -t one-search:external .

# 原有一体化镜像：默认目标和默认 embedded 模式
docker build --target all-in-one -t one-search:all-in-one .
```

`all-in-one` 镜像也保留了连接外部数据库的能力，但必须显式传入 `DATABASE_MODE=external` 和 `DATABASE_URL`；它仍然包含 PostgreSQL 软件及数据卷声明。希望镜像和运行配置都不包含内置数据库时，请使用 `external` 构建目标或专用 Compose 文件。

外部数据库暂时不可用时后端会退出，Compose 的 `restart: unless-stopped` 会继续重启；健康检查只有在数据库和 HTTP 服务均可用后才会通过。多副本部署时建议只让一个实例执行迁移，其他实例设置 `RUN_MIGRATIONS=false`。

## 首次配置

1. **平台管理**：启用要用的 Provider  
2. **Key 管理**：添加上游 API Key  
3. **搜索调试**：验证可用性  
4. **API 令牌**：创建业务用的 `osr_...` Token；需要正文抽取时同时勾选 `extract` 权限

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

完整接口见 [docs/extract.md](docs/extract.md)、[docs/admin-api-key.md](docs/admin-api-key.md)、[docs/mcp.md](docs/mcp.md)。

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
export ONE_SEARCH_API_TOKEN=osr_xxx
```

写入 `~/.codex/config.toml`：

```toml
[mcp_servers.one_search]
url = "http://localhost:5173/mcp"
bearer_token_env_var = "ONE_SEARCH_API_TOKEN"
enabled = true
tool_timeout_sec = 60
enabled_tools = ["search", "extract"]
```

启动 Codex 后输入 `/mcp`，应能看到 `one_search` 的 `search` 和 `extract`。

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
| `POSTGRES_PASSWORD` | — | 使用内置 PostgreSQL 时**必填**；外部数据库模式不需要 |
| `DATABASE_URL` | 空 | 外部 Compose 或容器直接运行时使用的 PostgreSQL 连接串 |
| `DATABASE_MODE` | 取决于镜像 | `all-in-one` 默认 `embedded`，`external` 默认 `external`；一体化镜像切外部库时须显式设为 `external` |
| `ADMIN_USERNAME` | `admin` | 首次管理员用户名 |
| `ADMIN_PASSWORD` | — | 生产**必填** |
| `ENCRYPTION_KEY` | — | **必填**，≥32 字符，加密敏感 Key |
| `API_AUTH_REQUIRED` | `true` | `/v1/*`、MCP 是否强制 Token |
| `MCP_ENABLED` | `true` | 是否开启 MCP |

### 可选（一般不用改，需要时加到 `.env`）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `APP_ENV` | Compose 下 `production` | 生产请保持 `production` |
| `POSTGRES_DB` / `POSTGRES_USER` | `one_search` | 库名 / 用户 |
| `HTTP_ADDR` | `:8080` | 仅独立后端部署可改；根 Dockerfile 的 Nginx 和健康检查固定使用 `8080` |
| `MCP_PATH` | `/mcp` | MCP 路径 |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:8080` | CORS 白名单 |
| `DATABASE_URL_FILE` | 空 | 外部连接串在容器内的文件路径，适合 Docker Secret；需要自行挂载，与 `DATABASE_URL` 二选一 |
| `DATABASE_DOCKER_NETWORK` | `shared-db` | 外部数据库 Compose 使用的 Docker 网络 |
| `RUN_MIGRATIONS` | `true` | 启动时自动迁移 |
| `REQUEST_TIMEOUT_MS` | `20000` | 上游请求超时 |
| `REQUEST_BODY_LIMIT_BYTES` | `1048576` | 请求体上限 |
| `SERVER_*_TIMEOUT_MS` | 见代码默认 | HTTP 服务器超时 |
| `ADMIN_SESSION_TTL_HOURS` | `24` | 管理 Session 时长 |
| `ADMIN_LOGIN_MAX_ATTEMPTS` 等 | 5 / 5min / 15min | 登录限速与锁定 |
| `VITE_API_BASE` | 空 | 前后端分离开发时指向后端 |
| `ONE_SEARCH_HTTP(S)_PROXY` | 空 | 容器访问上游时的代理 |

公网请在前面加 HTTPS 反代，转发 `/`、`/api/`、`/v1/`、`/healthz`（以及 `/mcp`）。

## 本地开发

```bash
# DB
docker run -d --name one-search-postgres \
  -e POSTGRES_DB=one_search -e POSTGRES_USER=one_search -e POSTGRES_PASSWORD=one_search \
  -p 15432:5432 postgres:16-alpine

# 后端
cd backend
export APP_ENV=development HTTP_ADDR=:18080 \
  DATABASE_URL='postgres://one_search:one_search@localhost:15432/one_search?sslmode=disable' \
  ADMIN_PASSWORD=admin123456 \
  ENCRYPTION_KEY=local-test-encryption-key-for-runtime \
  RUN_MIGRATIONS=true MIGRATIONS_DIR=migrations
go run ./cmd/server

# 前端
cd frontend && npm install && npm run dev
# 分离运行时可设 VITE_API_BASE=http://localhost:18080
```

## 目录

```text
backend/     Go API + migrations
frontend/    Vue 管理台
deploy/      all-in-one 入口 + nginx
docs/        接口文档
```

## License

[Apache License 2.0](LICENSE) © 2026 CncCbz

## 致谢

感谢 [LINUX DO](https://linux.do) 社区。
