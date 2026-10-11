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

## 部署方式

有三种部署方式，按需选择：

| 方式 | 适用场景 | 状态 |
| --- | --- | --- |
| **二进制 + systemd** | 最轻量，无 Docker 环境 | ✅ 推荐 |
| **Docker / Compose** | 容器化环境 | ✅ 多架构镜像（amd64 / arm64 / 386 / armv7） |
| **源码编译** | 需要定制修改 | ✅ 可用 |

---

## 方式一：二进制部署（Linux，推荐）

### 一键安装（二进制 + systemd）

```bash
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash
```

一条命令做完：识别架构（amd64 / arm64）→ 下载最新 Release 资产并校验 sha256 → 建 `newapi` 用户与 `/opt/new-api` 目录 → 写 systemd unit（默认 `--port 3000`）→ 启动服务并等 `/api/status` 探活通过，最后打印当前版本和回滚命令。

```bash
# 指定版本安装
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash -s -- --version v0.0.1

# 换端口（会一起写进 unit；已装过的机器也可以单独调）
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo NEW_API_PORT=8080 bash
```

可调环境变量（写在 `sudo` 后面）：`NEW_API_VERSION`、`NEW_API_PORT`、`NEW_API_DIR`、`NEW_API_BIN`、`NEW_API_USER`、`NEW_API_SERVICE`、`NEW_API_REPO`、`NEW_API_ARCH`。

> 脚本只在**缺失时**创建 `/opt/new-api/env` 和 systemd unit。若 unit 已存在且与脚本模板不同，它不会覆盖，而是另存为 `newapi.service.new` 并提示你 `diff` —— 你手工改过 unit 的机器是安全的。

### 验证与初始化

```bash
curl http://localhost:3000/api/status   # 返回 {"success":true,...} 即服务正常
systemctl status new-api
```

浏览器打开 <http://服务器IP:3000> 完成管理员初始化。

### 常用运维命令

```bash
sudo systemctl restart new-api     # 重启
sudo systemctl stop new-api        # 停止
journalctl -u new-api -f           # 查看实时日志
ls /opt/new-api/logs               # 落盘日志目录
```

<details>
<summary>手动分步安装（等同于上面脚本做的事）</summary>

#### 1. 下载二进制

到 [Releases 页面](https://github.com/bitscr/new-api/releases) 下载对应架构的压缩包，或直接：

```bash
# 查看机器架构
uname -m   # x86_64 → amd64；aarch64 → arm64

# 下载并解压（以 amd64 为例，文件名以实际 Release 为准）
wget https://github.com/bitscr/new-api/releases/latest/download/new-api-linux-amd64.tar.gz
wget https://github.com/bitscr/new-api/releases/latest/download/new-api-linux-amd64.tar.gz.sha256

# Release 里的 .sha256 记录的是构建目录内的相对路径（release/xxx.tar.gz），
# 直接 sha256sum -c 会报找不到文件，这里取哈希值重拼本地文件名再校验
echo "$(awk '{print $1}' new-api-linux-amd64.tar.gz.sha256)  new-api-linux-amd64.tar.gz" | sha256sum -c -

tar -xzf new-api-linux-amd64.tar.gz
sudo install -m 0755 new-api-linux-amd64 /usr/local/bin/new-api
```

> 二进制由 CI 发布，且**只在手动触发时**构建：在仓库 Actions 页面选「Build release binaries」→ Run workflow，填入 tag（如 `v1.0.0`），产出 `new-api-linux-amd64.tar.gz` / `new-api-linux-arm64.tar.gz` 及其 `.sha256` 校验文件。推送代码或打 tag **不会**自动触发构建。

#### 2. 准备目录与配置

```bash
sudo useradd -r -s /usr/sbin/nologin newapi 2>/dev/null || true
sudo mkdir -p /opt/new-api/{data,logs,web}
sudo chown -R newapi:newapi /opt/new-api

# 如需使用 MySQL / PostgreSQL / Redis，编辑 /opt/new-api/env：
sudo tee /opt/new-api/env > /dev/null <<'EOF'
# SQL_DSN=postgresql://user:password@localhost:5432/new-api
# REDIS_CONN_STRING=redis://:password@localhost:6379
EOF
```

> 二进制内置前端资源（`go:embed`），**不需要**单独的 `web/dist` 目录。

#### 3. systemd 服务

```bash
sudo tee /etc/systemd/system/new-api.service > /dev/null <<'EOF'
[Unit]
Description=New API Service
After=network.target

[Service]
User=newapi
WorkingDirectory=/opt/new-api
EnvironmentFile=/opt/new-api/env
ExecStart=/usr/local/bin/new-api --port 3000 --log-dir /opt/new-api/logs
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now new-api
sudo systemctl status new-api
```

</details>

---

## 方式二：Docker 部署

镜像发布在 GitHub Container Registry：`ghcr.io/bitscr/new-api`，支持 **amd64 / arm64 / 386 / armv7**（`latest` 是多架构清单，Docker 会按本机架构自动选择）。

> **镜像是手动构建的。** 推送代码或打 tag **不会**触发构建；需要更新镜像时，到仓库 Actions 页面选「Publish Docker image to GHCR」→ Run workflow（可留空平台参数使用默认的四个架构）。

<details>
<summary>Docker 命令</summary>

### 1. 拉取镜像并运行（默认 SQLite）

```bash
docker pull ghcr.io/bitscr/new-api:latest

docker run -d \
  --name new-api \
  --restart always \
  -p 3000:3000 \
  -v ./new-api-data:/data \
  -v ./new-api-logs:/data/logs \
  ghcr.io/bitscr/new-api:latest
```

- 数据目录：容器内 `/data`（SQLite 数据库文件 `one-api.db` 就存放在这里）
- 日志目录：容器内 **`/data/logs`**
- 默认端口：`3000`

> **日志路径注意**：镜像的工作目录是 `/data`，且默认 `--log-dir` 是相对路径 `./logs`，所以日志落在 **`/data/logs`**。想改到别处，显式传参数：`docker run ... ghcr.io/bitscr/new-api:latest --log-dir /app/logs`（此时才该挂 `/app/logs`）。

### 2. 初始化

浏览器打开 <http://localhost:3000>，按照引导完成初始化（创建管理员账号）。

> 如果端口被占用，可改用其他端口：`-p 8080:3000`。

### 3. 验证

```bash
curl http://localhost:3000/api/status
```

返回 `{"success":true,...}` 即服务正常。

</details>

---

## 方式二补充：Docker Compose 部署（Redis + 数据库）

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

## 方式三：源码编译安装

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
| `STREAMING_TIMEOUT` | 流模式无响应超时（秒），默认 `300` | `300` |
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

### 一键升级（二进制 + systemd）

```bash
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash -s -- --update
```

脚本会：下载最新 Release → 校验 sha256 → 把旧二进制备份成 `/usr/local/bin/new-api.bak-<时间戳>` → 重启服务并探活，最后打印当前版本和回滚命令。端口、用户、数据目录、env、systemd unit 全部沿用现状，不会重建。

```bash
# 升级到指定版本
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash -s -- --update --version v0.0.2
```

回滚（脚本探活失败时也会把这条命令打出来）：

```bash
sudo systemctl stop new-api
sudo mv /usr/local/bin/new-api.bak-20261007132415 /usr/local/bin/new-api
sudo systemctl start new-api
```

<details>
<summary>手动升级（等同于上面脚本做的事）</summary>

```bash
# 1. 下载新版二进制并校验（版本见 Releases）
wget https://github.com/bitscr/new-api/releases/latest/download/new-api-linux-amd64.tar.gz
wget https://github.com/bitscr/new-api/releases/latest/download/new-api-linux-amd64.tar.gz.sha256
echo "$(awk '{print $1}' new-api-linux-amd64.tar.gz.sha256)  new-api-linux-amd64.tar.gz" | sha256sum -c -

# 2. 备份旧二进制、替换、重启
tar -xzf new-api-linux-amd64.tar.gz
sudo cp -a /usr/local/bin/new-api /usr/local/bin/new-api.bak-$(date +%Y%m%d)
sudo install -m 0755 new-api-linux-amd64 /usr/local/bin/new-api
sudo systemctl restart new-api
```

</details>

**Docker Compose 升级：**

```bash
docker compose pull
docker compose up -d
```

**备份：**

- SQLite（二进制部署）：备份工作目录下的数据库文件 `/opt/new-api/one-api.db`
- SQLite（Docker）：备份挂载出来的数据目录（默认 `/data` 或 `./new-api-data`）
- PostgreSQL / MySQL：使用对应 `pg_dump` / `mysqldump` 备份
- 配置：二进制部署的 `/opt/new-api/env`、Docker 的 `.env` 与 compose 文件
- 日志：备份 `/opt/new-api/logs` 或容器内 `/data/logs` 目录

> **备份前先停服务**：SQLite 在运行中写入，热拷贝可能拿到不一致的快照。
> `sudo systemctl stop new-api && sudo cp /opt/new-api/one-api.db ~/ && sudo systemctl start new-api`
> 若目录下存在非空的 `one-api.db-wal`，必须连它一起备份，否则会丢最近的事务。

---

## 数据迁移（换机器 / 重装后还原）

数据全在**一个数据库文件**里：渠道、密钥、用户、令牌、计费配置、面板设置、日志都在库中，前端资源已编进二进制（`go:embed`）。所以只要保住那个库文件，就能完整还原，不用重新配置。

> 文件名是 **`one-api.db`**（沿用历史命名，不是 `new-api.db`）。二进制部署在 `/opt/new-api/one-api.db`，Docker 部署在挂载出来的数据目录里（默认 `/data/one-api.db`）。

### 方式一：二进制部署 —— 用迁移脚本（推荐）

**先把库从旧机器拷出来。** 一定要先停服务，否则可能拿到不一致的快照：

```bash
# 在【旧机器】上执行
sudo systemctl stop new-api
sudo cp /opt/new-api/one-api.db ~/one-api.db
sudo systemctl start new-api
# 然后把这个文件传到新机器，例如:
#   scp ~/one-api.db user@新机器:~/
```

**脚本不需要和 db 文件放在一起。** 你从哪里运行脚本都行，只要 `--db` 后面写的路径对。下面三种写法都可以：

```bash
# 1) 绝对路径(最省事,推荐)
sudo bash migrate.sh --db /home/you/one-api.db

# 2) 相对路径:相对你【当前所在目录】解析,不是相对脚本
cd /srv/backup && sudo bash migrate.sh --db one-api.db

# 3) 先 cd 到 db 所在目录,再只写文件名
cd ~ && sudo bash migrate.sh --db one-api.db
```

**通过 curl 直接运行**（不用先下载脚本，注意 `bash -s --` 后面才是脚本的参数）：

```bash
# 第 1 步:先干跑看看(只检查,不写入、不重启)
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/migrate.sh \
  | sudo bash -s -- --db ~/one-api.db --dry-run

# 第 2 步:确认无误后执行
curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/migrate.sh \
  | sudo bash -s -- --db ~/one-api.db
```

`--dry-run` 会先把计划打出来，让你确认无误再动手。全新机器上的真实输出：

```
==> 校验源数据库
[new-api] 文件头校验通过（5140480 字节）
[new-api] 表结构校验通过（options/channels/users 均存在）

==> 迁移计划
  源数据库 : /root/one-api.db
  目标位置 : /opt/new-api/one-api.db
  数据目录 : /opt/new-api
  运行用户 : newapi
  服务名   : new-api
  模式     : 仅检查（--dry-run）

  结论 : 可以执行，未检测到冲突
[new-api] 仅检查完成，未做任何改动
```

如果目标机器上**已经有**数据库，`--dry-run` 会改报：

```
  结论 : 会被拒绝（需要 --force）
```

脚本会：校验传来的确实是 new-api 的库（不是 `-wal` 或压缩包）→ 停服务 → 备份现有库为 `one-api.db.bak-<时间戳>` → 还原 → 修正属主与权限 → 启动并探活 → 打印回滚命令。

- 目标已存在数据库时**默认拒绝覆盖**，避免误删线上数据；确认要覆盖加 `--force`
- 有非空 `-wal` 时自动一起还原，并清除目标侧陈旧的 `-wal`
- 支持 `NEW_API_DIR` / `NEW_API_USER` / `NEW_API_SERVICE` / `NEW_API_PORT` 覆盖默认值
- `--help` 查看全部选项（curl 管道下也能用）

> **关于 `~` 的坑**：`~/one-api.db` 是在你**当前 shell** 里先展开成 `/home/你的用户名/one-api.db` 再交给 sudo 的，所以一般没问题。但如果你用 `sudo -i` 或 `sudo sh -c "... ~/..."`，`~` 会变成 `/root`，文件就找不到了。不确定时用**绝对路径**最稳。


<details>
<summary>方式二：手动还原（等同于脚本做的事）</summary>

```bash
# 1. 停服务（必须:SQLite 运行中写入会产生不一致快照）
sudo systemctl stop new-api

# 2. 备份现有库
sudo cp -a /opt/new-api/one-api.db /opt/new-api/one-api.db.bak-$(date +%Y%m%d%H%M%S)
# 有非空 -wal 的话连它一起处理
sudo rm -f /opt/new-api/one-api.db-wal /opt/new-api/one-api.db-shm

# 3. 放入备份的库
sudo cp -f ~/one-api.db /opt/new-api/one-api.db

# 4. 关键:修正属主与权限(否则启动失败 attempt to write a readonly database)
sudo chown newapi:newapi /opt/new-api/one-api.db
sudo chmod 644 /opt/new-api/one-api.db
sudo chown newapi:newapi /opt/new-api          # 目录必须可写(journal 要在目录内建文件)

# 5. 启动并验证
sudo systemctl start new-api
curl -s http://127.0.0.1:3000/api/status
```

**这一步最容易踩坑。** 实测:库属主为 `root` 时服务启动立刻失败（`attempt to write a readonly database`）——因为程序以 `newapi` 用户运行，需要能写库、并在库所在目录创建 journal 文件。

</details>

### 方式三：Docker 部署 —— 把库放进挂载目录

Docker 路径不需要额外脚本，**全自动**：

```bash
# 1. 停掉容器
docker stop new-api

# 2. 把库放进挂载出来的数据目录
cp ~/one-api.db ./new-api-data/one-api.db

# 3. 重新启动
docker start new-api
```

容器内以 root 运行，没有属主问题。数据目录是 `/data`，库文件名 `one-api.db`。

### 迁移时不必带的东西

- **`SESSION_SECRET` 不需要带。** 它只用于 session cookie 签名，不参与数据加密；日志会显示它每次启动随机生成，属正常。实测带上与不带，原有 API 密钥鉴权都正常通过。
- **`logs/`、`data/`、`web/` 目录**：不承载数据（`web/` 为空，前端内嵌在二进制里）。
- **被覆盖掉的只有 `/opt/new-api/env` 里的 `env`-only 配置**（例如 `ERROR_LOG_ENABLED`）。这些不在数据库里，换机器后需要照原样填一次；面板里能配的选项都在库中，会随库还原。

### 环境变量 vs 数据库里

- **跟着库走（自动还原）**：所有面板可配项 —— 渠道、令牌、用户、分组、模型倍率、计费表达式、重试次数、系统名称等。
- **需要在新机器上重新提供**：`/opt/new-api/env` 里的 `env`-only 开关（如 `ERROR_LOG_ENABLED`、`STREAMING_TIMEOUT`、`MAX_REQUEST_BODY_MB`）、`SQL_DSN`（用外部数据库时）、`REDIS_CONN_STRING`，以及 systemd unit 里的 `--port`。

---

## 问题与支持

- 问题报告与功能建议：[Issues](https://github.com/bitscr/new-api/issues)
- 安全漏洞：请使用 GitHub Security Advisories 私密报告，请勿在公开 Issue 中披露

## 许可

GNU Affero General Public License v3.0（AGPL-3.0），详见 [LICENSE](./LICENSE)。