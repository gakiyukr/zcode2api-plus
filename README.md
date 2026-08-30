# zcode2api

将 ZCode (zcode.z.ai) Coding Plan 额度转为标准 Anthropic Messages API，支持多账号轮询、
额度用完自动换号、实时用量监控、后台管理 UI 与鉴权，以及阿里云无痕验证自动续期。

## 快速开始

```bash
pip install -r requirements.txt
# 无痕验证求解器（无浏览器，Node + jsdom）。需已安装 Node.js：
cd captcha_node && npm install && cd ..
cp .env.example .env                     # 按需修改

python main.py serve                     # 启动网关 + 后台 UI（默认端口 3000）
```

- 后台管理：`http://localhost:3000/admin`（默认密码 `zcode`）
- 对话端点：`http://localhost:3000/v1/messages`（兼容 Anthropic Messages 协议）

## Docker 部署

镜像内同时包含 Python(网关)与 Node(无浏览器无痕验证求解器),开箱即用。

```bash
# 方式一：docker compose（推荐）
docker compose up -d --build
# 账号 / 设置持久化在宿主机 ./data 目录；停止：docker compose down

# 方式二：docker 原生命令
docker build -t zcode2api:latest .
docker run -d --name zcode2api \
  -p 3000:3000 \
  -v "$(pwd)/data:/data" \
  -e ZCODE_ADMIN_KEY=zcode \
  --restart unless-stopped \
  zcode2api:latest
```

- 数据卷:容器内 `/data`(对应 `ZCODE_DATA_DIR`)存放 `accounts.db`,务必挂载到宿主机以持久化。
- 环境变量同下方「环境变量」表,可在 `docker-compose.yml` 的 `environment` 下覆盖。
- **请勿**将 `.env`、`data/` 打入镜像——已在 `.dockerignore` 中排除。

### 自动构建镜像(GHCR)

`.github/workflows/docker-build.yml` 会在**每次更新**(push 到 `master` 或打 `v*` tag)时
**自动构建并发布镜像到 GHCR(GitHub 容器仓库,`ghcr.io`)**,使用内置 `GITHUB_TOKEN`,
**不使用 Docker Hub**;Pull Request 仅构建验证、不推送。

```bash
# 拉取并运行已发布镜像（tag: latest 或 sha-xxxxxxx）
docker run -d --name zcode2api -p 3000:3000 \
  -v "$(pwd)/data:/data" -e ZCODE_ADMIN_KEY=zcode \
  ghcr.io/yuanhhs/zcode2api:latest
```

> 首次发布后,GHCR 上的包默认可能为私有;如需公开拉取,请到仓库 **Packages → 该包 → Package settings → Change visibility** 设为 Public。

## 后台 UI

| 页面 | 说明 |
|------|------|
| `/admin/login` | 后台登录（Bearer 密钥鉴权，凭证加密存于浏览器 localStorage）|
| `/admin/accounts` | 账号池：新增/导入/导出、启用禁用、**实时额度与状态监控**（每 5 秒刷新）|
| `/admin/settings` | 后台密码、网关 API Key |

账号池页实时展示每个账号的状态（正常 / 额度用完 / 限流 / 异常 / 禁用）、各模型剩余额度、
调用与失败次数。请求按 round-robin 分发，**某账号额度用完会自动切换到下一个账号**，并在 UI 中即时反映。

## 賬號調度統計（Token）

网关在每次成功回應的透传过程中解析上游 `usage`，为每个账号累计调度 token：

- 统计口径：`輸入`（input_tokens）、`輸出`（output_tokens）、`快取`（cache_read + cache_creation），仅计入成功回應；串流中断不计量。
- 支持 SSE（`message_start` / `message_delta`）与非串流 JSON 两种回應形态，解析在透传时顺带完成，不影响流式转发。
- 查看入口：後台「账号池」概览卡（累計調度 Tokens）与明细表 Tokens 栏（悬停可见精确值）；CLI `python main.py accounts` 亦会显示。
- 数据随账号持久化于 `data/accounts.db`；行内「重置 Token 統計」按钮可清零单个账号的累计值。

## 多账号轮询与换号

- 在「账号池」粘贴 Coding Plan JWT（3 段点分）或 API Key，每行一个即可加入轮询。
- 网关每次请求选择下一个「可用」账号（跳过用完 / 限流 / 异常 / 禁用）。
- 命中额度用完信号（余额为 0、上游 402、错误体含 quota/余额 等）→ 标记 `exhausted` 并换下一个账号。
- 上游 429 → 标记 `cooling` 冷却一段时间后自动恢复；401/403（非验证码）→ 标记 `invalid`。
- 后台任务按 `ZCODE_QUOTA_REFRESH_INTERVAL` 周期刷新各账号额度；也可在 UI 手动刷新。

## 鉴权

- **后台鉴权**：所有 `/admin/api/*` 需 `Authorization: Bearer <后台密码>`。
- **网关鉴权（可选）**：在「设置」配置「网关 API Key」后，`/v1/messages` 须携带
  `Authorization: Bearer <key>` 或 `x-api-key: <key>`；留空则不校验。

## 无痕验证（无浏览器）

Coding Plan（JWT）模式调用 `zcode.z.ai` 上游时需要阿里云无痕验证参数
（请求头 `X-Aliyun-Captcha-Verify-Param`）。本项目**不启动任何真实浏览器**，
而是用 **Node + jsdom** 在模拟浏览器环境中运行阿里云官方无痕 SDK 来求得该参数。

- 求解器位于 `captcha_node/solver.js`；首次使用前需执行 `cd captcha_node && npm install`。
- `app/captcha.py` 以子进程方式调用求解器，内置结果缓存（默认 45s）、并发去重与失败重试。
- 求解器在 jsdom 中补齐了 SDK 依赖的浏览器 API（`matchMedia`、canvas/WebGL、`Worker`、`OffscreenCanvas`），
  执行 `startTracelessVerification` 后输出 `verifyParam`。
- `verifyParam` 实为 `base64(JSON{certifyId, sceneId, isSign, securityToken})`，由阿里云服务端签发。
- 仅 Coding Plan（JWT）账号需要；API Key 账号走 `api.z.ai` 回退端点，无需验证码。

> 求解器运行的是阿里云自家混淆 SDK。若阿里云更新其指纹逻辑（feilin / cloudauth-device），
> 可能需要相应调整 `solver.js` 中补齐的浏览器 API 桩。该方案无需真实浏览器，比无头 Chromium 轻量很多。

## 命令行

```bash
python main.py serve [--port 3000]                 # 启动服务
python main.py login zai [--no-browser]            # OAuth 登录 Z.AI 并自动入池
python main.py add-account zai <name> <jwt|key>    # 添加轮询账号
python main.py accounts [zai|bigmodel]             # 查看账号列表
python main.py remove-account <provider> <id|name> # 删除账号
python main.py quota                               # 查看各账号实时额度
python main.py status                              # 查看配置概览
python main.py set-admin-key <key>                 # 设置后台密码
python main.py export [file] / import <file>       # 导出 / 导入账号
```

`login zai` 会打开 ZCode 官方授权页；授权完成后，将浏览器地址栏中的完整登录完成页地址粘贴回终端即可完成凭证交换。
使用 `--no-browser` 时需自行打开终端输出的授权链接。

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ZCODE_PORT` | 3000 | 服务端口 |
| `ZCODE_HOST` | 0.0.0.0 | 监听地址 |
| `ZCODE_ADMIN_KEY` | zcode | 后台密码初始值（之后以 DB 为准）|
| `ZCODE_DATA_DIR` | ./data | 数据目录（SQLite 存放处）|
| `ZCODE_QUOTA_REFRESH_INTERVAL` | 60 | 后台刷新额度间隔（秒），0 关闭 |
| `ZCODE_CLIENT_VERSION` | 3.7.7 | ZCode 客户端版本（额度请求会附带此版本）|
| `ZCODE_COOLING_SECONDS` | 300 | 限流冷却时长（秒）|
| `ZCODE_NODE_PATH` | node | 无痕验证求解器使用的 Node 可执行文件 |
| `ZCODE_CAPTCHA_TIMEOUT` | 40 | 单次验证码求解超时（秒）|
| `ZCODE_CAPTCHA_RETRIES` | 4 | 验证码求解失败重试次数 |
| `CAPTCHA_CACHE_TTL` | 45000 | 验证码缓存时长 (ms) |
| `UPSTREAM_USER_AGENT` | `ZCode/<客户端版本>` | 上游请求 User-Agent |
| `ZAI_UPSTREAM_URL` / `ZAI_FALLBACK_URL` / `BIGMODEL_UPSTREAM_URL` | — | 上游端点 |

## 项目结构

```
├── app/
│   ├── main.py            # FastAPI 应用工厂 + 生命周期
│   ├── settings.py        # 环境变量 / 配置
│   ├── models.py          # Account 数据模型与状态
│   ├── store.py           # SQLite 持久化 + 轮询游标（data/accounts.db）
│   ├── agent.py           # 上游请求构建
│   ├── captcha.py         # 无痕验证求解（调用 Node 求解器）
│   ├── quota.py           # 额度查询 + 后台用量监控
│   ├── oauth.py           # Z.AI OAuth 登录流程
│   ├── auth_admin.py      # 后台 / 网关鉴权
│   ├── usage.py           # Token 调度统计（UsageCollector：解析上游 usage）
│   ├── logs.py            # 彩色终端日志
│   ├── routes/            # gateway / admin_api / pages
│   └── statics/           # app.css, auth.js, toast.js, header.js, admin/*.html
├── captcha_node/          # 无浏览器无痕验证求解器（Node + jsdom，solver.js）
├── main.py                # 命令行入口（serve / login / accounts / quota ...）
├── data/                  # 运行时生成：accounts.db (SQLite)
├── Dockerfile             # 镜像（Python + Node）
├── docker-compose.yml     # 一键部署
├── .dockerignore
├── .github/workflows/     # docker-build.yml（仅构建验证，不推送 Docker Hub）
├── docs/ARCHITECTURE.md   # 架构概览
├── requirements.txt
└── .env.example
```

## 技术栈

- Python 3.13 · FastAPI · Uvicorn · httpx
- SQLite（账号 / 设置持久化，WAL 模式）
- Node.js + jsdom（无浏览器求解阿里云无痕验证 → verifyParam）

## 文档

- [架构概览 docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 系统架构图、请求流程、账号状态机、无痕验证流程与已知限制。

## 致谢

- UI 设计参考:[chenyme/grok2api](https://github.com/chenyme/grok2api)
- 社区:[linux.do](https://linux.do)

## 许可证

本项目采用 [AGPL-3.0](LICENSE) 许可证。

## 重要免责声明

本仓库仅供学习、研究、个人实验和内部验证使用，不提供任何形式的商业授权、适用性保证或结果保证。

作者及仓库维护者不对因使用、修改、分发、部署或依赖本项目而产生的任何直接或间接损失、账号封禁、数据丢失、法律风险或第三方索赔负责。

请勿将本项目用于违反服务条款、协议、法律法规或平台规则的场景。商业使用前请自行确认 LICENSE、相关协议以及你是否获得了作者的书面许可。
