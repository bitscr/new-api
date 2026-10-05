<div align="center">

![new-api](/web/public/logo.png)

# New API

**AI 模型网关与资产管理平台** — 兼容 OpenAI / Claude / Midjourney 等接口格式的 API 聚合与中继服务。

Docker 镜像、源码编译、配置与部署教程见下文。

</div>

---

## 项目来源与致谢

本项目为 **new-api** 的再开发与独立化版本，代码主体源自上游 **Futureppo 维护的 [new-api](https://github.com/Futureppo/new-api)** 仓库（其上游可继续追溯至 [CalciumIon](https://github.com/CalciumIon/new-api) / [QuantumNous](https://github.com/QuantumNous/new-api) 维护的原版项目）。

> [!NOTE]
> - 本仓库的 Git 历史已重置为单一初始提交（内容独立于上游，不含任何上游提交与历史数据），与上游**没有公共祖先**，无法直接 fast-forward 同步
> - 如需拉取上游更新：`git remote add upstream https://github.com/Futureppo/new-api.git`，然后 `git fetch upstream` 手动合并
> - 本项目遵循上游的开放精神继续独立维护，感谢所有上游开发者

---

## 功能特性

- ✅ **多模型聚合**：一个接口聚合 OpenAI、Claude、Gemini、Midjourney、Rerank 等多种模型，统一 OpenAI 兼容格式对外提供
- ✅ **渠道与负载均衡**：同一模型配置多个渠道，按优先级/权重自动分配；渠道失败自动回退重试（支持分组、令牌配额）
- ✅ **虚拟 `auto` 模型**：分组内自动路由到当前可用性最好的模型
- ✅ **多租户管理**：用户、分组、配额、令牌、邀请与禁用管理面板
- ✅ **可观测性**：请求日志、错误日志、令牌/额度统计
- ✅ **事件通知**：Webhook 通知、渠道健康检查与告警能力
- ✅ **一键部署**：支持 Docker / Docker Compose / 源码编译；数据库可选 SQLite、MySQL、PostgreSQL

> [!IMPORTANT]
> - 本项目仅用于学习与自托管使用，不对稳定性与技术支持作任何保证
> - 使用者须遵守当地法律法规与所使用上游模型服务（如 OpenAI）的服务条款，不得将其用于非法用途
> - 在中国境内请勿将未登记的生成式 AI 服务面向公众开放

---

## 快速开始（Docker，推荐）

### 1. 拉取镜像并运行（默认 SQLite）

```bash
docker pull ghcr.io/bitscr/new-api:latest

docker run -d \
  --name new-api \
  --restart always \
  -p 3000:3000 \
  -v ./new-api-data:/data \
  -v ./new-api-logs:/app/logs \
  ghcr.io/bitscr/new-api:latest
```

- 数据目录：容器内 `/data`（SQLite 数据库文件默认存放在此）
- 日志目录：容器内 `/app/logs`
- 默认端口：`3000`

### 2. 初始化

浏览器打开 <http://localhost:3000>，按照引导完成初始化（创建管理员账号）。

> 如果端口被占用，可改用其他端口：`-p 8080:3000`。

### 3. 验证

```bash
curl http://localhost:3000/api/status
```

返回 `{"success":true,...}` 即服务正常。

---

## Docker Compose 部署（Redis + 数据库）

仓库内自带 `docker-compose.yml`，默认组合为 **Redis + PostgreSQL**（也可改用 MySQL），包含健康检查与持久化卷：

```bash
git clone https://github.com/bitscr/new-api.git
cd new-api
docker compose up -d
```

> ⚠️ **生产部署前务必修改所有默认密码**（PostgreSQL `123456`、Redis `123456`），并取消注释设置 `SESSION_SECRET` 随机字符串。

数据库切换说明（详见 `docker-compose.yml` 文件头注释）：

- **PostgreSQL**（默认）：直接 `docker compose up -d`
- **MySQL**：注释 Postgres 相关 `SQL_DSN` 行、取消注释 MySQL 服务与 `SQL_DSN=root:123456@tcp(mysql:3306)/new-api`，并在 `depends_on` / `volumes` 中取消相应的 MySQL 注释

---

## 源码编译安装

### 前置要求

| 组件 | 版本要求 | 用途 |
| --- | --- | --- |
| Go | ≥ 1.25.1（推荐 1.26.x） | 后端编译 |
| bun | ≥ 1.2 | 前端依赖与构建 |
| Node.js（可选） | 20+ | 若使用 `npm` 替代 bun 执行前端脚本 |

### 1. 克隆仓库

```bash
git clone https://github.com/bitscr/new-api.git
cd new-api
```

### 2. 构建前端

```bash
cd web
bun install
DISABLE_ESLINT_PLUGIN='true' bun run build
cd ..
```

构建产物输出到 `web/dist`。

### 3. 构建后端

```bash
# 下载依赖
go mod download

# 编译（如需注入版本号，参考仓库 Dockerfile 中的 ldflags 写法）
go build -o new-api
```

### 4. 运行

```bash
# 将 web/dist 与后端二进制放在同一目录结构下（或直接使用仓库根目录）
./new-api
```

默认监听 `3000` 端口，可用 `--port` 参数修改：

```bash
./new-api --port 8080
```

### 5. 配置数据库（可选）

默认使用 SQLite，数据文件保存于数据目录。如需使用 MySQL / PostgreSQL，通过环境变量指定：

```bash
# PostgreSQL
export SQL_DSN="postgresql://user:password@host:5432/new-api"

# MySQL
export SQL_DSN="user:password@tcp(host:3306)/new-api"

./new-api
```

---

## 环境变量

| 变量 | 说明 | 示例 |
| --- | --- | --- |
| `SQL_DSN` | 数据库连接串；留空使用 SQLite | `postgresql://root:123456@postgres:5432/new-api` |
| `REDIS_CONN_STRING` | Redis 连接串（速率限制/缓存等） | `redis://:123456@redis:6379` |
| `SESSION_SECRET` | 会话签名密钥；**多机部署必须设置随机字符串** | `random_string` |
| `TZ` | 时区 | `Asia/Shanghai` |
| `ERROR_LOG_ENABLED` | 是否启用错误日志记录 | `true` |
| `BATCH_UPDATE_ENABLED` | 是否启用批量更新 | `true` |
| `NODE_NAME` | 节点名称（审计日志标识；多实例建议设置） | `node-1` |
| `STREAMING_TIMEOUT` | 流模式无响应超时（秒），默认 `120` | `300` |
| `SYNC_FREQUENCY` | 需要定期数据库同步时设置（秒） | `60` |

---

## 反向代理（Nginx + HTTPS 示例）

```nginx
server {
    listen 443 ssl;
    server_name api.example.com;

    ssl_certificate     /etc/letsencrypt/live/api.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.example.com/privkey.pem;

    client_max_body_size 64m;

    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 600s;
    }
}

server {
    listen 80;
    server_name api.example.com;
    return 301 https://$host$request_uri;
}
```

---

## 升级与备份

**Docker Compose 升级：**

```bash
docker compose pull
docker compose up -d
```

**备份：**

- SQLite：备份数据目录（默认 `/data` 或 `./new-api-data`）中的数据库文件
- PostgreSQL / MySQL：使用对应 `pg_dump` / `mysqldump` 备份
- 日志：备份 `/app/logs` 目录

---

## 问题与支持

- 问题报告与功能建议：[Issues](https://github.com/bitscr/new-api/issues)
- 安全漏洞：请使用 GitHub Security Advisories 私密报告，请勿在公开 Issue 中披露

## 许可

GNU Affero General Public License v3.0（AGPL-3.0），详见 [LICENSE](./LICENSE)。