# zcode2api Go 版重写计划

> 本仓库 `C:\Projects\zcode2api-plus`（分支 `go-rewrite`）即 Go 重写仓库（原 `go/`
> 子目录已上提到仓库根，Python 版实验工作区内容已移除）。计划与行为契约参照
> Python 版（原主仓库 `app/` + `main.py`，现归档于本仓库 `python-legacy` 分支）
> 与 `HANDOFF.md`。
> 目标：用 Go 重写 Python 版的全部后端功能，
> 做到**与 Python 版行为对齐、数据互通（共用同一个 `data/accounts.db`）、前端零改动**。
> 本文档是唯一的计划与进度台账，每完成一项就勾选对应 `- [ ]`。

---

## 1. 背景与动机

- Python 版已稳定可用（FastAPI + httpx + SQLite + cloakbrowser），但部署形态依赖 Python 运行时 + Node 子进程。
- Go 版的收益：**单二进制部署**、goroutine 并发模型天然消除"事件循环阻塞 / 全局锁串行"两类问题、
  可借机**甩掉 Node 运行时**（jsdom 求解器已被上游 F001 风控判死，无移植价值）。
- 前端（`frontend/`，React SPA）**不重写**：构建产物 `frontend/dist` 直接 `go:embed` 进二进制。

## 2. 范围

**移植（与 Python 版 1:1 对齐）：**

- [x] `/v1/messages` 网关：多账号轮询、SSE/JSON 流式透传、错误分类与自动换号
- [x] `/v1/models`（Anthropic / OpenAI 双兼容超集形态，见 §5.7）
- [x] `/v1/chat/completions` **OpenAI（GPT）兼容层——Go 版增量功能**：请求/响应双向转换 +
  流式 SSE 重编码 + 工具调用，同步走网关引擎（详见 §5.7）
- [x] `/v1/responses`（OpenAI Responses API，服务 Codex CLI 生态；划界见 §5.8）
- [x] `/async/v1/messages`：ticket + SSE keepalive + 流中断终止语义（`_MidStreamError`）
- [x] 账号状态机（active/exhausted/cooling/invalid/disabled）+ 按模型可用性调度
- [x] 额度监控（`billing/balance` 解析、多订阅合并、15s 缓存 + 并发去重、后台周期刷新）
- [x] 调度 token 统计（UsageCollector：SSE `message_start`/`message_delta`、JSON 顶层 usage）
- [x] Admin API `/admin/api/*` 全部端点 + SPA 托管（`/admin/*` catch-all 回落 index.html）
- [x] 鉴权：后台密钥（含单 IP 失败限速 10 次/5 分钟）+ 网关密钥（fail-closed）
- [x] OAuth 登录（Z.AI 授权 → JWT 入池 → API Key 兑换链）
- [x] 账号级出站代理（http/https/socks4/socks5/socks5h）+ 命名代理管理 + 出口探测
- [x] 验证码：真实 Chromium 池（rod）+ 人工回填兜底
- [x] SQLite 持久化（accounts + meta，WAL）与 **Python 版数据库互通**
- [x] CLI 子命令（serve / login / add-account / accounts / remove-account / quota / status / set-admin-key / export / import）
- [x] Release CI（2026-09-11 定案：放弃 Docker 裸二进制交付；GitHub Actions 推 v* tag 构建五平台
  linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64 并上传 Releases）— `.github/workflows/release.yml`
- [x] **Linux 一键部署**（仅 Linux）：`deploy/manage.sh` 单一交互式管理脚本
  （二进制的安装/更新/卸载/状态 + Docker 的安装/更新/卸载 + 服务控制，**已实测**）
  + systemd 单元模板。另有 `Dockerfile` + `docker-compose.yml` 作为参考实现，
  **未经验证、不保证可用**（需在自有服务器构建，不提供 CI 编译服务）
- [x] **套餐自动领取（Go 版增量，2026-09-10 后新增）**：billing/preview + billing/claim、
  激活事件上报、业务码翻译、3007 换码重试、入池自动领取（← Python 版 `app/claim.py` + `app/telemetry.py`，见 §5.9）

**明确不移植：**

- `captcha_node/solver.js`（jsdom 无浏览器求解——已被上游 F001 风控识别，回退路径改为人工回填）
- 裸机 systemd 部署（Python 版已删除）
- Python 版曾有的 OpenAI 端点残留（残缺实现已从 Python 版删除；Go 版按 §5.7 完整契约重新实现，
  属增量功能而非移植项）
- `/v1/completions`（legacy）、`/v1/embeddings` 等其它 OpenAI 端点

## 3. 技术选型

| 领域 | 选择 | 理由 |
|------|------|------|
| Go | 1.25 | 需要 `ServeMux` 的 method + wildcard 路由增强（`go.mod` 锁定 `go 1.25.0`） |
| HTTP | 标准库 `net/http` | 不引框架；SSE 用 `http.Flusher` 手写透传 |
| SQLite | `modernc.org/sqlite` | 纯 Go 无 CGo → 可交叉编译单文件 |
| 浏览器自动化 | `github.com/go-rod/rod` | 验证码求解；复用 cloakbrowser 下载的 Chromium 二进制 |
| 前端嵌入 | `embed` | dist 打进二进制，单文件交付 |
| 配置 | 环境变量（沿用 `ZCODE_*` 命名），不引入 `.env` 加载 | 与 Python 版配置兼容；`.env` 需由部署方自行 source |
| 日志 | 自写彩色终端输出（`internal/web/logs.go`） | 对齐 Python 版日志形态；只记元信息不记消息内容 |
| 依赖原则 | 最少依赖：sqlite、rod，其余标准库 | 便于审计与长期维护 |

## 4. 目录结构（实际）

```text
.（仓库根，即原 go/ 上提）
├── PLAN.md                      # 本文件（计划 + 进度台账）
├── go.mod / go.sum
├── webui.go                     # go:embed frontend/dist（embed 只能引用本包目录树，
│                                #   故声明放在仓库根包，由 cmd 引入）
├── frontend/                    # React SPA（不重写；dist 已入库并 embed）
├── deploy/                      # Linux 一键部署（见 deploy/README.md）
│   ├── manage.sh                # 交互式管理：安装/更新/卸载/状态/服务控制（二进制 + Docker）
│   ├── zcode2api.service        # systemd 单元模板（占位符由 manage.sh 渲染；亦内嵌于脚本）
│   └── README.md                # 部署指南（含疑难排解与注意事项）
├── Dockerfile                   # 参考实现（未验证，不保证可用；含 Chromium 依赖）
├── docker-compose.yml
├── .dockerignore
├── cmd/zcode2api/
│   ├── main.go                  # 入口 + serve()（依赖装配、横幅、监听）
│   └── cli.go                   # 全部 CLI 子命令（serve / login / accounts / ...）
└── internal/
    ├── config/config.go         # ← app/settings.py（环境变量、路径、上游端点、常量）
    ├── model/account.go         # ← app/models.py（Account、状态机、模型可用性、JSON 字段对齐）
    ├── store/store.go           # ← app/store.py（SQLite 单连接 + 轮询游标 + meta + 密钥引导）
    ├── gateway/
    │   ├── engine.go            # 选号重试循环 + 上游调用核心（三个端点共用）
    │   ├── handler.go           # ← routes/gateway.py（/v1/messages、/v1/models 字节级透传）
    │   ├── body.go              # ← _normalize_body（模型名映射、content 桥接、system 注入）
    │   ├── classify.go          # 错误分类（鉴权/402/429 码族/3010/3007/F001/captcha 头）
    │   └── usage.go             # ← app/usage.py（UsageCollector）
    ├── openai/                  # Go 版增量（Python 版无对应实现）
    │   ├── convert.go           # OpenAI Chat Completions → Anthropic 请求转换（§5.7）
    │   ├── respond.go           # Anthropic → OpenAI 响应转换（非流式）
    │   ├── stream.go            # /v1/chat/completions 流式 SSE 重编码
    │   ├── responses.go         # /v1/responses 请求/响应转换（§5.8）
    │   ├── responses_stream.go  # /v1/responses 流式 response.* 事件序列
    │   └── handler.go           # 两个端点的 HTTP 层（复用 gateway.Engine）
    ├── upstream/
    │   ├── request.go           # ← app/agent.py（build_request + 头剔除表）
    │   └── zcode_system.json    # ← app/zcode_system.json（embed 注入）
    ├── quota/quota.go           # ← app/quota.py（fetch_quota、多订阅合并、monitor）
    ├── captcha/
    │   ├── captcha.go           # ← app/captcha.py（缓存、人工回填、Solver 编排）
    │   ├── pool.go              # ← app/captcha_browser.py（有界 worker 池：超时判死不误投）
    │   ├── browser_solver.go    # Solver 实现（惰性启动 + 配置键重建 + 失败冷却）
    │   ├── solve.go             # rod worker：页面 HTML + 求解 JS（从 Python 版逐字抄写）
    │   ├── browserdl.go         # cloakbrowser Chromium 自动下载（SHA256SUMS + Ed25519 验签）
    │   └── AliyunCaptcha.js.txt # 阿里云无痕 SDK（embed 素材）
    ├── oauth/oauth.go           # ← app/oauth.py
    ├── auth/auth.go             # ← app/auth_admin.py（Bearer 校验 + 失败限速）
    ├── adminapi/                # ← routes/admin_api.py
    │   ├── adminapi.go          # 路由注册 + guard + 响应工具
    │   ├── accounts.go          # 账号 CRUD / 额度刷新 / 设置 / 验证码 / 导出导入
    │   ├── proxies.go           # 代理线路 CRUD + 出口探测
    │   ├── login.go             # OAuth 登录 start/complete
    │   ├── claim.go             # 套餐 preview / claim
    │   └── monitor.go           # 监控与用量
    ├── claim/                   # Go 版增量（← app/claim.py + app/telemetry.py）
    │   ├── claim.go             # billing/preview + billing/claim + 3007 换码重试
    │   └── telemetry.go         # 激活事件上报
    ├── asyncpool/pool.go        # ← routes/async_pool.py（ticket + SSE + 泄漏防护）
    ├── proxy/                   # ← app/proxy.py（Transport 缓存 + socks4/4a/5/5h 拨号器）
    │   ├── proxy.go             # URL 归一化与 scheme 白名单
    │   └── client.go            # TransportFor / ClientFor / socks 握手
    └── web/
        ├── spa.go               # /admin catch-all + /assets 静态 + /meta
        └── logs.go              # ← app/logs.py（彩色终端）
```

Python 版 `app/zcode_system.json` 已复制为 `internal/upstream/zcode_system.json` 并 `embed`。

## 5. 行为契约（必须与 Python 版逐字对齐的部分）

重写时**先抄数字、再抄逻辑**。以下契约直接从 Python 版提取，Go 实现以此为验收依据：

### 5.1 常量

| 项 | 值 |
|----|----|
| `MAX_CAPTCHA_RETRIES` / `MAX_ACCOUNT_ATTEMPTS` | 3 / 5 |
| 3010 并发准入重试延迟 | 1s、2s（第 3 次失败原样回传 429） |
| 对外模型白名单 | `glm-5.3-flash`、`GLM-5.3`（normalize 后比对） |
| `MODEL_NAME_MAP` | 仅 `{"glm-5.3": "GLM-5.3"}` |
| 429 额度上限码族 | 1113、1308-1311、1313、1316-1321 → 标记该模型 exhausted；其余 429 → cooling |
| 业务码语义 | `1005`(HTTP 200)=当日额度用完；`3007`=验证码失效；`3010`=并发准入；F001=风控指纹拒绝 |
| 冷却 / 刷新 | `COOLING_SECONDS=300`、`QUOTA_REFRESH_INTERVAL=60`（0=关闭，运行中可改） |
| 验证码缓存 | Node/人工令牌 45s；配置 600s；浏览器令牌**不缓存** |
| 验证码池 | workers=1、startup=90s、request=45s、queue=60s、shutdown=10s、失败冷却=60s |
| 后台限速 | 单 IP 滑动窗口 300s 内失败 10 次 → 一律 429，成功清零 |
| 额度缓存 | 结果 TTL 15s + inflight 去重 + 删除账号清理 |
| 端口 / 密钥 | 3000；admin_key 缺失随机生成（历史默认 `zcode` 强制轮换）；gateway_key `sk-` 前缀，fail-closed |

### 5.2 错误分类链（`/v1/messages`，顺序不可变）

1. 验证码挑战（响应头 `x-aliyun-captcha-*` / body `code=3007` / `F001` 文本仅限 400/403）→ 刷新令牌同账号重试
2. 401/403 → 账号 `invalid`，换号（JWT 上游为**裸 401 空 body**；api.z.ai 为 `error.type=1000/1001/1003`）
3. 402 → 该模型 exhausted，换号 + 触发额度刷新
4. 429 且 code∈3010 → 等待重试（账号状态不变）
5. 429 且 code∈额度上限码族 → 该模型 exhausted 换号；其余 429 → cooling 换号
6. 503 → cooling 换号
7. 其余 → **不做状态推断，原样透传上游响应**

### 5.3 上游请求

- JWT 账号 → `https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`，`Authorization: Bearer <jwt>`，
  **必须注入 `zcode_system.json` 到顶层 `system`**（否则上游 405），并携带验证码头 + `X-Device-Mid`。
- API Key 账号 → `https://api.z.ai/api/anthropic/v1/messages`，`x-api-key` 头，无需验证码。
- 固定头：`anthropic-version: 2023-06-01`、`User-Agent: ZCode/3.7.7`、`X-ZCode-App-Version: 3.7.7`、
  `X-ZCode-Agent: glm`、`HTTP-Referer: https://zcode.z.ai/`、`X-Device-Mid: <uuid4 持久化于 data/device_mid.txt>`。
- 透传剔除：host/content-length/x-api-key/authorization/user-agent/http-referer/accept-encoding/connection/cookie/验证码头；
  `x-zcode*` 开头一律剔除。

### 5.4 数据互通（最高优先级契约）

Go 版**直接打开 Python 版的 `data/accounts.db`**，schema 完全一致：

```sql
accounts(id TEXT PK, provider, name, mode, status, enabled INT, created_at REAL, data TEXT/*JSON*/)
meta(key TEXT PK, value TEXT)
```

- `accounts.data` 是 `Account` 全量 JSON。Go 结构体的 json tag 必须**逐一对应 Python dataclass 字段**
  （snake_case）：`id,name,provider,mode,email,jwt_token,api_key,enabled,status,quota,exhausted_models,
  disabled_models,plan,plans,usage,use_count,fail_count,total_input_tokens,total_output_tokens,
  total_cache_creation_tokens,total_cache_read_tokens,last_used_at,last_checked_at,cooling_until,
  last_error,proxy_url,proxy_id,created_at`；未知字段忽略（对齐 `Account.from_dict`）。
- 导出/导入格式（`version:1` + `providers.{name,mode,secret,disabled_models}`）保持一致。
- 两个版本可交替打开同一个 db（不做 schema 迁移）。

### 5.5 验证码（照抄 `captcha_browser.py` 的思路与字符串）

- **HTML 与求解 JS 逐字抄写**：`AliyunCaptchaConfig` 先于内联 SDK（SDK 内嵌于
  `internal/captcha/AliyunCaptcha.js.txt`，`</script>` 替换为 `<\/script>`）；`initAliyunCaptcha` 配置
  （mode=popup、language=en、`#cap`/`#btn`）、`getInstance` 内 `startTracelessVerification ?? show`、
  `success` 写 `window.__zcodeOutcome`、轮询间隔 250ms、SDK 加载 20s / 单次求解 40s。
- **成功复用同一页面，失败重载页面清 SDK 内部状态**；token 只在内存。
- **池语义对齐**：worker 槽位 = 并发上限；请求超时 → 关闭该浏览器实例并替换，
  迟到的 TOKEN 通过 context 取消保证**绝不误投**后续请求；启动阶段失败 → 60s 冷却回退人工回填提示。
- **浏览器二进制复用**：rod 通过 `launcher.Bin(...)` 启动 cloakbrowser 的同一 Chromium + 相同
  `_BROWSER_ARGS`（`--disable-dev-shm-usage` 等 5 项），驱动差异极小化。
  二进制发现链：`ZCODE_CAPTCHA_BROWSER_BIN` → `CLOAKBROWSER_BINARY_PATH` → `CLOAKBROWSER_CACHE_DIR`
  下版本最高者 → **自动下载**（`browserdl.go`：cloakbrowser.dev 主站 + GitHub Releases 兜底，
  SHA256SUMS 的 Ed25519 验签，失败不降级），因此**无需 Python 预下载**。
- 回退链：浏览器池不可用/失败冷却 → 返回 503 `captcha_required`（提示后台 `/admin/captcha` 人工回填），
  人工令牌缓存 45s。

### 5.6 async ticket 语义

- SSE 事件：`ticket`(pending) → `ready` → `data:` chunk… → `done`/`error`；每 10s `: keepalive`；总超时 300s。
- 泄漏防护三件套照搬：SSE 退出 finally 释放 + 中止后台任务；孤儿 ticket 建票时清扫（生命周期 + 60s 宽限）。
- 流中断：**已发出 chunk → 终止票务（upstream_stream_interrupted）不重试**；零 chunk → 换号重试（最多 3 次，指数退避 2^n）。

### 5.7 OpenAI（GPT）兼容端点契约（Go 版增量）

- 路由：`POST /v1/chat/completions`，**同步**走网关引擎（选号/验证码/错误分类全复用），不经 async ticket 池；
  鉴权与 `/v1/messages` 相同（网关密钥 fail-closed）。
- `/v1/models` 返回双兼容超集：每项同时带 Anthropic 侧 `id/display_name/type:"model"` 与
  OpenAI 侧 `object:"model"/created/owned_by:"zcode2api"`，顶层 `object:"list"`。

**请求转换（OpenAI → Anthropic Messages）：**

| OpenAI 字段 | 处理 |
|-------------|------|
| `messages[].role = system/developer` | 全部归并到顶层 `system`，按顺序拼接，且排在 zcode_system 块之后 |
| `role = user/assistant` | 原样映射；content 字符串 → `[{type:"text"}]` |
| content part `text` | text block |
| content part `image_url`（data URL） | image block（base64，media_type 从 data URL 解析） |
| content part `image_url`（http[s]） | image block（`source.type="url"`）；上游支持度未知，失败按上游错误原样透传 |
| assistant 消息含 `tool_calls` | assistant 消息 + `tool_use` block（`arguments` JSON 字符串解析为 `input`） |
| `role = tool` | user 消息 + `tool_result` block（`tool_use_id` 绑定） |
| `model` | 白名单校验 + `MODEL_NAME_MAP`（与 /v1/messages 一致，白名单外 400） |
| `max_tokens` / `max_completion_tokens` | → `max_tokens`；两者皆缺省 8192 |
| `temperature` / `top_p` | 透传 |
| `stop`（string 或 array） | → `stop_sequences` |
| `stream` | true → OpenAI chunk 流；false → JSON |
| `tools` / `tool_choice` | tools → Anthropic tools（`parameters` → `input_schema`）；choice `auto/none` 透传语义、named → `{type:"tool",name}` |
| `n` | 仅支持 1，>1 返回 400 |
| `presence_penalty` / `frequency_penalty` / `logprobs` / `user` 等 | 静默忽略（README 声明） |

**响应转换（Anthropic → OpenAI）：**

- 非流式：`choices[0].message` 中 text blocks 拼接为 `content`；`tool_use` →
  `tool_calls[{id, type:"function", function:{name, arguments(JSON 字符串)}}]`；
  `stop_reason` 映射：`end_turn/stop_sequence→stop`、`max_tokens→length`、`tool_use→tool_calls`、
  `refusal→content_filter`；usage：`prompt_tokens = input + cache_read + cache_creation`、
  `completion_tokens = output`（cache 细节放 `prompt_tokens_details.cached_tokens`）；`id = "chatcmpl-<上游 id>"`。
- 流式（**SSE 重编码，不是透传**）：`message_start` → 首个 chunk（`delta:{role:"assistant"}`）；
  `content_block_delta`(text_delta) → `delta.content` 增量；`content_block_start`(tool_use) →
  `tool_calls` delta（id/name）+ `input_json_delta` → `arguments` 增量；`message_delta` →
  `finish_reason` 终止 chunk；`message_stop` → `data: [DONE]`；`ping` 事件丢弃；
  `stream_options.include_usage` 时在终止前附 usage chunk。
- UsageCollector 在重编码旁路照常解析 Anthropic 事件——账号调度统计不受转换影响。

### 5.8 `/v1/responses`（Go 版增量，见 M7）

- 定位：服务 Codex CLI 等 Responses 生态客户端；复用 §5.7 的引擎与转换基建，增量约 300-500 行。
- v1 范围：`instructions` → system；`input`（字符串 / 类型化 item 数组：message、function_call、
  function_call_output）→ messages；扁平 `tools` → Anthropic tools；`max_output_tokens` → `max_tokens`；
  `reasoning.effort` → 上游 `output_config.effort`；输出端 text → `output_text`、tool_use → `function_call`
  item；流式重编码为 `response.*` 事件序列（`response.output_text.delta` 等）。
- **状态化划界**：无状态用法全支持（`store:false` + 每轮完整历史，Codex 默认即此）；
  带 `previous_response_id` 的请求 v1 返回明确 400；内存 LRU 回放列为后续可选增强，不阻塞。
- 实现状态：M7 已完成（`internal/openai/responses.go` + `responses_stream.go`）。

### 5.9 套餐自动领取（Go 版增量，2026-09-10 新增）

- 定位：Python 版 2026-09-10 上线的活动套餐自动领取（`app/claim.py` + `app/telemetry.py`），
  Go 版对齐移植（M8 已完成，见 `internal/claim/`）。
- 链路：`GET {BILLING_BASE}/billing/preview?app_version=&platform=` → 解析
  `data.plans[]`（plan_id/name/priority + model_usage token grants）→
  `POST {BILLING_BASE}/billing/claim` body `{"plan_id"}`，需验证码头
  `X-Aliyun-Captcha-Verify-Param`（+ 可选 `X-Aliyun-Captcha-Verify-Region`）。
- 业务码映射：1001 套餐不存在 / 1002 活动结束 / 1003 已领取过 / 1004 不符合条件 /
  1005 今日名额用完 / 3001 参数错误 / 3007 验证码失败（换码重试一次）/ 401 未登录。
- 激活上报：preview 前对 `https://zcode.z.ai/api/v1/event/report` 发 `app_launch` +
  `app_daily_active` 两事件（16 字段体，无 Authorization；疑似活动投放资格信号；
  失败仅记日志不阻断）。device_mid 沿用本机持久化标识。
- 触发点：入池后（批量添加 / OAuth 完成 / CLI login）后台自动全量领取 +
  Admin API `GET /claim/preview`、`POST /claim`（account_ids 可选，冷却账号跳过）+
  前端账单页按钮（工具栏全量 + JWT 账号行内单账号）。
- 复用项：鉴权头与 quota 同源（`X-ZCode-App-Version`/`X-Platform`/`X-Device-Mid`）；
  请求走账号代理（与 Python 版 make_async_client 语义一致）；
  验证码经 M5 的 captcha manager 求解/人工回填。

## 6. 里程碑

### M0 骨架 + 数据层
- [x] go.mod / 目录骨架 / config（全部 `ZCODE_*` 环境变量）— `internal/config/config.go`
- [x] model.Account + 状态机 + 模型可用性 + JSON 契约单测 — `internal/model/`
- [x] store：SQLite 单连接 + 密钥引导（随机生成、`zcode` 轮换）+ 轮询游标 + 代理 + 导入导出 + 单测 — `internal/store/`
- [ ] **互通验收**：用 Python 版生成的真实 `data/accounts.db` 打开 → 账号/设置完整可读，Go 写回后 Python 版也能读
  （代码就绪，`go build/vet/test ./...` 全绿；尚缺一份真实 Python 版 db 做双向实测，
  §7 规划的脱敏 `testdata/` 夹具亦未提交）
### M1 网关核心
- [x] build_request（头 + zcode_system 注入 + 剔除表 + 客户端头过滤）— `internal/upstream/`
- [x] 请求整形 + 错误分类链 + 模型白名单 + UsageCollector（含单测）— `internal/gateway/{body,classify,usage}.go`
- [x] captcha 管理器（配置缓存 10min / 人工回填 45s / Solver 接口）— `internal/captcha/`（浏览器池 M5 落地）
- [x] 鉴权：网关 fail-closed + 后台限速（10 次/5min）— `internal/auth/`；终端日志 — `internal/web/`
- [x] /v1/messages 选号循环（engine.go）+ JSON/SSE 字节级透传（handler.go）
- [x] /v1/models（M1 先按 Python 版形态，M4 已扩展为双兼容超集）
- [x] httptest e2e：鉴权、透传、白名单、401/402/429 码族/3010/500 透传、1005、JWT 注入、验证码 503（13 组用例）
- [ ] **验收**：真实账号非流式 + 流式各打通一次（依赖 M5 浏览器池或 M2 人工回填端点，两者均已就绪）
### M2 Admin API + 鉴权 + SPA
- [x] auth（Bearer + 限速）、admin_api 全部端点、embed dist + catch-all
- [x] **验收**：浏览器完整走一遍后台 UI（登录/仪表板/账号池/代理/验证中心/设置六页均正常渲染，
  经 UI 新增账号成功）；限速单测（auth_test.go 5 组，含失败上限与成功清零）
### M3 额度监控 + async
- [x] fetch_quota（解析 + 多订阅合并 + 15s 缓存 + inflight 去重 + 清理）+ 后台 monitor
- [x] /async/v1/messages（ticket 全语义）
- [x] **验收**：移植 test_quota / test_usage / test_async_pool 全部用例
### M4 OpenAI 兼容层（Go 版增量，见 §5.7）
- [x] 请求转换：system 归并、content blocks、图片、tools/tool_calls/tool_result、stop_sequences
- [x] 响应转换：非流式 JSON + 流式 SSE 重编码 + stop_reason/usage 映射
- [x] /v1/models 扩展为双兼容超集
- [x] **验收**：§5.7 每条映射至少一个单测（convert 11 + respond/stream 9 + e2e 5）；
  httptest 全链路覆盖非流式、流式、一次工具调用三种场景（openai 官方客户端真机
  跑通待真实账号环境，与 M0/M1 验收合并执行）
### M5 验证码池（代码完成，待真机验收）
- [x] rod 池（复用 cloakbrowser 二进制）+ manager（缓存/人工回填/冷却）— `internal/captcha/{pool,browser_solver,solve,browserdl}.go`
- [x] 失败注入单测：超时判死替换、崩溃替换、迟到 token 不误投、队列超时、Stop 幂等（pool_test.go 14 组）
- [ ] **验收**：真实账号连续 20 次 JWT 请求全部自动通过（无 F001）
### M6 OAuth + 代理 + CLI + 交付（代码完成，待真机验收）
- [x] OAuth 登录链（internal/oauth + adminapi login 端点 + CLI login）、账号代理出口
  （internal/proxy：http/https CONNECT + 手写 socks4/4a/5/5h，Transport 缓存；引擎与 quota 已接线）、
  CLI 子命令（serve/login/add-account/accounts/remove-account/quota/status/set-admin-key/export/import）、
  Release CI（推 `v*` tag 交叉编译五平台并上传 Releases）
- [x] Linux 一键部署（`deploy/manage.sh` 单一交互式管理脚本，**已实测**）：
  二进制安装/更新（版本比对+回滚）/卸载/状态 + Docker 安装/更新/卸载 + 服务控制；
  systemd 单元模板内嵌于脚本，单文件可部署。另附 `Dockerfile` + `docker-compose.yml`
  参考实现，**未经验证、不保证可用**
- [ ] **验收**：Release CI 产物可运行；`-race` 下全测试通过；两版本交替使用同一 db 无异常
### M7 `/v1/responses` 端点（代码完成，待真机验收）
- [x] 请求/响应/流式转换 + 状态化划界（`previous_response_id` v1 先 400）—
  `internal/openai/{responses,responses_stream}.go`（7 组单测，含 2 组 e2e）
- [ ] **验收**：Codex CLI 指向网关完成一次完整会话（无状态模式）
### M8 套餐自动领取（代码完成，待真机验收）
- [x] claim 核心链（preview 解析 / claim 业务码 / 3007 换码重试）+ 激活事件上报 —
  `internal/claim/`（8 组单测对照 Python tests/test_claim.py）
- [x] Admin API `/claim/preview` + `/claim` + 入池自动领取触发点（批量添加 / OAuth / CLI login）
  + 前端按钮（工具栏全量 + JWT 账号行内单账号）
- [x] **验收**：单测覆盖业务码映射与 3007 换码重试语义（claim_test.go 8 组）
- [ ] **验收**：真机领取一次成功（billing/preview + claim + 激活上报全链路）

### M9 已知缺陷修复（2026-09-15 完成，仅剩 1 项待评估）
以下为代码审查确认的行为问题，**已全部修复**（每项附回归测试）：

- [x] `asyncpool` 错误分类与 engine 分歧 → 抽出 `gateway.MarkAccount` / `MarkModelExhausted` /
  `IsQuotaExhaustedCode` 共用，asyncpool 现按同一顺序分类 401/402/3010/429 码族/503；
  新增 `TestQuotaExhaustedCodeMarksModelNotCooling`、`TestUnauthorizedMarksInvalid`、
  `TestConcurrencyLimitKeepsAccountState`
- [x] `BrowserSolver.Solve` 持锁跨 `pool.Solve` → 锁只保护池的选取与重建，求解在锁外执行；
  配置变更时以 `retired` 标记延迟关闭旧池，避免中止在途求解；
  新增 `TestBrowserSolverConcurrentSolvesDoNotSerialize`（验证 n 路并发）、
  `TestBrowserSolverConfigChangeDoesNotAbortInFlight`
- [x] async ticket 逾时不投递终止事件 → 补发 `ticket_timeout` 错误事件；
  新增 `TestTicketTimeoutEmitsErrorEvent`
- [x] `include_usage` 外泄到上游 → 不再写入上游请求体，handler 改从原始 OpenAI 请求读取；
  测试改为 `TestStreamOptionsIncludeUsageNotForwarded`
- [x] `responses_stream` 的 `item_id` 不一致 → 统一取 function_call item 的 id；
  `TestResponsesStreamEvents` 增加 id 一致性断言
- [x] socks5 IPv6 回退 → 本地解析优先 IPv4，仅有 IPv6 时以 ATYP=0x04 发送；
  新增 `TestSocks5LocalResolveFallsBackToIPv6`
- [x] `browserdl.extractTarGz` 无 symlink 分支 → 支持 `TypeSymlink`（限制链接目标在解包目录内）
  与 `TypeLink`，未知类型记日志而非静默丢弃；新增 `TestExtractTarGzPreservesSymlink`、
  `TestExtractTarGzRejectsEscapingSymlink`
- [x] `quota` 代理回退无日志 → 补 `web.Warn`
- [x] `go.mod` 将 `go-rod/rod` 标为 indirect → `go mod tidy` 修正，并补齐 go.sum 缺失条目
- [x] `gofmt` 未覆盖 → 全部 62 个 Go 档已格式化

**待评估（未修改）**：
- [ ] 后台限速以 `RemoteAddr` 为键、不信任 `X-Forwarded-For`（`auth.go:129-135`）。
  这是**刻意的安全取舍**（信任 `X-Forwarded-For` 会让攻击者伪造头绕过限速），
  已在 README 与 `deploy/README.md` 说明「建议后台仅绑定内网」；
  若确需反代支持，应改为显式配置可信代理列表，而非无条件信任该头。

## 7. 测试策略

- 单测**逐个移植** Python 版 `tests/`（错误分类、池协议、路由白名单、quota 合并、oauth、usage、鉴权引导），
  保持同名用例语义，便于两边对照。当前 23 个测试文件、171 个 `Test` 函数，`go test ./...` 全绿。
- OpenAI 转换层：§5.7 每条映射一行单测；流式重编码按事件序列断言输出 chunk 序列；
  最终用 openai 官方客户端（python）指向网关做真客户端回归（待真实账号环境）。
- httptest 起完整服务打 mock 上游做端到端；SSE 用 `curl -N` 与 Python 版逐字节对比分块行为。
- 全部测试在 `-race` 下通过（**待办**：本机无 gcc，需在有 C 工具链的环境补跑）。
- 数据互通夹具：把一份脱敏 `accounts.db` 提交到 `testdata/` 作为固定夹具（**尚未提交**）。

## 8. 风险与对策

| 风险 | 对策 |
|------|------|
| rod 驱动 cloakbrowser 二进制过不了风控 | 启动参数逐项对齐；不行则退 `--headless=new` / go-rod/stealth；最终兜底 = 人工回填（功能不中断） |
| OpenAI↔Anthropic 转换长尾（工具调用分片、多 system、图片 URL） | §5.7 映射表逐行单测；不支持的行为（n>1 等）显式 400 并在 README 声明 |
| SSE 分块/flush 行为与 Python 不一致 | httptest + curl -N 字节级对照；flush 每个 chunk |
| Account JSON 字段错漏导致 db 互读失败 | M0 就做互通验收；结构体 tag 对照 dataclass 逐一 review |
| Go 无 jsdom 兜底 | 接受——jsdom 本已被风控判死；人工回填为最终兜底 |
| rod 版本 API 变动 | go.mod 锁定 minor 版本 |

## 9. 交付形态

- `go build ./cmd/zcode2api` → 单二进制（前端已 embed），仅 Chromium 运行库为外部依赖。
- Release CI：推 `v*` tag → GitHub Actions 交叉编译五平台产物
  （linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64；
  CGO_ENABLED=0，`-trimpath -ldflags="-s -w"`）→ 上传 GitHub Releases。
- **Linux 一键部署（`deploy/manage.sh`，单一入口）**：
  - 交互式选单：安装/更新/卸载二进制、查看状态、服务控制（启停/重启/日志）、Docker 三个操作。
  - 非交互子命令：`install` / `update` / `uninstall` / `status` / `docker-install` /
    `docker-update` / `docker-uninstall`，配合 `-y` 可用于脚本与 CI。
  - 二进制路径（**已实测**）：自动装系统依赖（含验证码浏览器的共享库，按发行版命名差异解析）→
    取二进制（Releases 下载或 `--local` 就地编译）→ 生成 `.env` → 渲染 systemd unit →
    `systemctl enable --now` → 打印首次启动密钥。更新时备份旧二进制，下载失败自动回滚。
  - systemd 单元模板同时内嵌于脚本，单文件下载即可运行。
  - `Dockerfile` + `docker-compose.yml`：**参考实现，未经验证、不保证可用**（开发环境无容器运行时，
    从未执行 `docker build`）。仅作起点，需自行验证与调整；两卷需持久化
    （`/app/data` 账号库、`/app/browser` 浏览器缓存）。
- Python 版归档于本仓库 `python-legacy` 分支，仅作行为契约参照，不再更新。
