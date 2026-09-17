# zcode2api

Z.AI ZCode Coding Plan → OpenAI/Anthropic 兼容網關（**Go 版，現為主線**）。

把 Z.AI Coding Plan 賬號池包裝成標準 API：多賬號輪詢（優惠額度優先）、驗證碼全自動求解、
額度與套餐到期監控、OAuth 登錄、賬號級出站代理、活動套餐自動領取、賬號歸檔，
單二進制交付（前端已內嵌，無外部運行時）。

> 📦 歷史沿革：本項目原為 Python 實現，現已由 Go 重寫版取代成為主線。
> Python 舊版保留在 [`python-legacy`](../../tree/python-legacy) 分支（僅歸檔維護，不再更新）。

## 端點一覽

| 端點 | 協議 | 說明 |
|------|------|------|
| `POST /v1/messages` | Anthropic Messages | 流式/非流式，字節級透傳 |
| `POST /v1/chat/completions` | OpenAI Chat | 本地轉換為 Anthropic Messages，含工具調用 |
| `POST /v1/responses` | OpenAI Responses | 服務 Codex CLI（無狀態模式；`previous_response_id` 返回 400） |
| `POST /async/v1/messages` | Anthropic 異步 | ticket + keepalive + 流中斷語義 |
| `GET /v1/models` | 雙兼容超集 | Anthropic 與 OpenAI 形態字段並存 |
| `/admin/*` | — | 內嵌 React 管理後台 |
| `/guest` | — | 訪客帳號提交頁（需管理員開啟邀請碼） |

對外三種請求格式，內部統一走 Anthropic Messages 上游管道：選號循環、驗證碼求解、
錯誤分類（401/402/429 碼族/3010/F001）、賬號狀態機與用量統計只維護一份。

## 快速開始

```bash
# 下載現成產物（Releases 頁：linux × amd64/arm64）
# 或源碼構建：
go build -o zcode2api ./cmd/zcode2api
./zcode2api serve            # http://127.0.0.1:3000

# 驗證碼自動求解：首次啟動自動下載補丁 Chromium（約 200MB，無需 Python）；
# 也可用 ZCODE_CAPTCHA_BROWSER_BIN 指定已有的瀏覽器二進制
ZCODE_CAPTCHA_BROWSER=true ./zcode2api serve
```

首次啟動橫幅輸出後台密碼與網關 API Key（也可 CLI 設定）。
瀏覽器求解不可用時自動回退人工回填（後台 `/admin/captcha`），功能不中斷。

## CLI

```
zcode2api serve [--port 3000]        啟動網關 + 後台 UI
zcode2api login zai [--no-browser]   OAuth 登錄 Z.AI 並入池（自動領取活動套餐）
zcode2api add-account zai <name> <jwt|key>
zcode2api accounts [zai]             查看賬號列表
zcode2api remove-account <provider> <id|name>
zcode2api quota                      查看各賬號實時額度
zcode2api status                     配置概覽
zcode2api set-admin-key <key>        設置後台密碼
zcode2api export [file] / import <file>   賬號導出/導入（與 python-legacy 互通）
```

## 部署（Linux）

交互式管理腳本與 Docker 方案見 [`deploy/README.md`](deploy/README.md)。

```bash
sudo ./deploy/manage.sh            # 交互式選單：安裝/更新/卸載/狀態/服務控制
```

或非交互：

```bash
sudo ./deploy/manage.sh install                # 二進制 + systemd（已實測）
sudo ./deploy/manage.sh install --port 3010 --user zcode
sudo ./deploy/manage.sh update                 # 更新（自動比對 Release 版本）
sudo ./deploy/manage.sh uninstall              # 卸載（--purge 連數據刪除）
sudo ./deploy/manage.sh docker-install         # Docker（未驗證）
```

安裝完成後自動啟動服務，**首次啟動的後台密碼與網關 API Key 只顯示一次，請立即保存**。

> ⚠️ **Docker 方案未經驗證，不保證可用。** 倉庫內的 `Dockerfile` 與
> `docker-compose.yml` 為參考實作，從未經 `docker build` 實測（開發環境無容器運行時）。
> 請自行驗證與調整；本專案不對 Docker 路徑提供支援承諾。

> 💡 驗證碼瀏覽器：首次使用時自動從 cloakbrowser.dev 下載補丁 Chromium
> （SHA256SUMS + Ed25519 簽名校驗，GitHub Releases 兜底），緩存於
> `CLOAKBROWSER_CACHE_DIR`，零 Python 依賴。`manage.sh` 會一併裝好其系統依賴
> （按發行版命名差異自動解析）。下載源可用 `CLOAKBROWSER_DOWNLOAD_URL` 覆蓋；
> `ZCODE_CAPTCHA_BROWSER_BIN` 可指向任意已有瀏覽器。實測部分發行版自帶的
> Chromium（如 Debian 13）會被風控拒絕——自動下載的補丁二進制即為此問題的內建解法。

> ⚠️ 放在反向代理後時注意：後台登入失敗限速以 `RemoteAddr` 為鍵，不信任
> `X-Forwarded-For`（防偽造），因此反代後所有客戶端會共用同一失敗桶
> （5 分鐘 10 次即整站 429）。**解法**：以 `--host 127.0.0.1` 安裝，讓服務只監聽
> 回環、僅由本機反代轉發，詳見 [`deploy/README.md`](deploy/README.md)。

## 配置（環境變量）

核心項（全部見 `internal/config/config.go`）：

| 變量 | 默認 | 說明 |
|------|------|------|
| `ZCODE_PORT` / `ZCODE_HOST` | 3000 / 0.0.0.0 | 監聽地址 |
| `ZCODE_DATA_DIR` | `./data` | 賬號庫、密鑰、設備指紋 |
| `ZCODE_CAPTCHA_BROWSER` | false | 啟用 rod 瀏覽器池自動求解 |
| `ZCODE_CAPTCHA_BROWSER_BIN` | 自動發現 | Chromium 二進制路徑 |
| `ZCODE_ASYNC_ENABLED` | true | 掛載 /async/v1/messages 空閒池 |

訪客提交的邀請碼存於數據庫（後台「設置」頁可改），不走環境變量。

## 賬號級出站代理

賬號配置 `proxy_url` 後，該賬號的網關請求、額度查詢與套餐領取均走對應代理；
支持 `http(s)://`（CONNECT）與 `socks4://`、`socks5://`、`socks5h://`（socks5h
由代理解析域名）。代理無效時回退直連並記錄 `last_error`。

## 套餐自動領取

JWT 賬號入池（批量添加 / OAuth / CLI login）後自動：激活事件上報 →
`billing/preview` 按優先級逐個 `billing/claim`（驗證碼 3007 自動換碼重試一次）。
後台賬號頁另有「領取套餐」按鈕（工具欄全量 + JWT 賬號行內單賬號）。

## 發佈與開發

```bash
go build ./... && go vet ./... && go test ./...   # 全量驗證
go test -race ./...                               # 併發檢查（需 C 工具鏈）
```

- 推 `v*` tag → GitHub Actions 自動交叉編譯 Linux 產物（linux/amd64、linux/arm64）
  並上傳 Releases。僅維護這兩個平台：本項目面向服務端自部署。
- 前端改動：`cd frontend && npm install && npm run build`，把更新後的 `frontend/dist`
  一併提交（`dist` 已入庫並由 `go:embed` 打包；改版後要刪掉舊的 hash 檔）。
- 行為契約與里程碑台账見 [`PLAN.md`](PLAN.md)（唯一權威，含逐項驗收狀態）。

### 維護者注意

- **`deploy/manage.sh` 的 systemd 模板有兩份**：`deploy/zcode2api.service` 與腳本內
  `write_unit()` 的 heredoc（單獨下載腳本時走後者）。改模板時**兩處都要改**。
- **部署腳本必須保持 LF**：`.gitattributes` 已對 `*.sh` / `*.service` 強制 `eol=lf`，
  改動後用 `git ls-files --eol deploy/` 確認索引為 `i/lf`；可執行位用
  `git update-index --chmod=+x` 設定（Windows 檔案系統不保留該位）。
- **`.env` 不會被自動載入**：專案未引入 dotenv，`manage.sh` 產生的 `.env` 由 systemd
  的 `EnvironmentFile` 讀取；本機直接跑二進制時需自行 source。
- **`frontend/dist` 的 embed 宣告必須在倉庫根包**（`webui.go`）：`go:embed` 只能引用
  宣告檔所在目錄樹內的文件，`internal/web` 無法引用它。
- **上游回應形態**：zcode.z.ai 的非流式回應是**標準 Anthropic Messages 頂層形態**
  （`id`/`content`/`stop_reason`/`usage` 都在頂層，**沒有**嵌套 `message` 物件）。
  權威依據見 `internal/gateway/usage.go` 頭部註釋。
- **測試隔離**：一律 `config.DBPath = filepath.Join(t.TempDir(), "accounts.db")`；
  captcha 測試用 `SetSolver`（假求解器）+ `SetConfigProvider`（固定配置），否則會打真實
  上游；e2e 帳號用 api_key 模式（憑證不含兩個點）即不觸發驗證碼路徑，離線穩定。

## 訪客提交帳號

管理員在後台「設置」頁填寫**邀請碼**後，`/guest` 頁面即對外開放（清空邀請碼
即關閉）。訪客的提交路徑刻意比後台窄：

### 人機驗證（可選，自建 Cap）

在後台「設置」頁填入 Cap 後台給出的**三個值**即啟用：

| 欄位 | 取自 Cap 後台 | 範例 |
|------|--------------|------|
| 實例地址 | 你的 Cap 部署地址（不含 site key） | `https://cap.example.com` |
| Site Key | 建立 site key 後的識別碼 | `d9256640cb53` |
| 密鑰 | **secret key**（不是管理員 ADMIN_KEY） | — |

實際呼叫地址為 `{實例地址}/{Site Key}/`，設定頁會即時顯示拼出的結果供核對。

- **三項都填**才啟用；只填部分會被拒絕儲存（避免留下半殘配置）。
  三項皆留空即停用。
- 密鑰**只留在服務端**，絕不下發瀏覽器；拼好的地址會回顯給前端（widget 需靠它取題）。
- Site Key 填錯時返回 502（路徑不存在屬配置問題），而非報成「驗證未通過」。
- 兩步各驗一次：Cap token 是一次性的，第一步用過即失效，第二步會自動重新求解。
- 未配置時不渲染驗證元件、後端也跳過校驗——自建實例位址因部署而異，無法給預設值。
- Cap 服務不可達時返回 502（管理員要查配置），而非報成「驗證未通過」誤導訪客。

- **只走 OAuth 授權**，不提供令牌輸入框。完成授權能證明提交者確實持有該帳號；
  貼上一串 JWT 什麼都證明不了。
- **實測通過才入池**。授權只說明「現在持有」，不說明「當下可用」——帳號可能已
  被封、額度耗盡或地區受限。後端會用該憑證發起一次最小的真實請求
  （`max_tokens=1`），成功才寫入賬號池；失敗直接丟棄。
- **不回顯任何帳號信息**。回應只有成功與否，不含 ID、郵箱、額度或賬號列表。
- **邀請碼 + 每 IP 每日 3 次**。配額在開始授權時扣減；未設邀請碼時入口關閉
  （fail closed）而非開放。
- **可選人機驗證**。接入自建 [Cap](https://trycap.dev) 實例後，兩步各驗一次
  （Cap token 一次性，第二步需重新求解）。未配置時整段跳過。

> ⚠️ 實測需要驗證碼求解器，因此訪客提交**要求 `ZCODE_CAPTCHA_BROWSER=true`**；
> 未啟用時提交會因驗證碼不可用而失敗。

## 賬號歸檔

不再需要調用的賬號可「歸檔」：歸檔即強制停用並從賬號池隱藏，僅在後台歸檔區
保留記錄（累計用量可查）；歸檔賬號不參與調度、套餐領取與額度刷新，恢復後
保持停用狀態，需手動啟用才會重新入池。

## License

AGPL-3.0（見 [LICENSE](LICENSE)），僅供學習研究與個人自部署使用；使用本項目產生的
一切行為與後果由使用者自行承擔，請自行遵守上游服務條款。本項目與 Z.AI 無任何關聯。
