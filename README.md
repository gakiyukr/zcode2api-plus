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

對外三種請求格式，內部統一走 Anthropic Messages 上游管道：選號循環、驗證碼求解、
錯誤分類（401/402/429 碼族/3010/F001）、賬號狀態機與用量統計只維護一份。

## 快速開始

```bash
# 下載現成產物（Releases 頁：linux × amd64/arm64、darwin × amd64/arm64、windows × amd64）
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
sudo ./deploy/manage.sh install --port 3010 --user zcode --prefetch-browser
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

## 賬號級出站代理

賬號配置 `proxy_url` 後，該賬號的網關請求、額度查詢與套餐領取均走對應代理；
支持 `http(s)://`（CONNECT）與 `socks4://`、`socks5://`、`socks5h://`（socks5h
由代理解析域名）。代理無效時回退直連並記錄 `last_error`。

## 套餐自動領取

JWT 賬號入池（批量添加 / OAuth / CLI login）後自動：激活事件上報 →
`billing/preview` 按優先級逐個 `billing/claim`（驗證碼 3007 自動換碼重試一次）。
後台賬號頁另有「領取套餐」按鈕（工具欄全量 + JWT 賬號行內單賬號）。

## 發佈與開發

- 推 `v*` tag → GitHub Actions 自動交叉編譯五平台產物（linux/amd64、linux/arm64、
  darwin/amd64、darwin/arm64、windows/amd64）並上傳 Releases。
- 全量驗證：`go build ./... && go vet ./... && go test ./...`；併發檢查 `go test -race ./...`
  （需 C 工具鏈）。
- 前端改動：`cd frontend && npm install && npm run build`，並把更新後的 `frontend/dist` 一併提交
  （`dist` 已入庫並由 `go:embed` 打包）。
- 行為契約與里程碑台账見 `PLAN.md`；交接注意事項見 `HANDOFF.md`。

## 賬號歸檔

不再需要調用的賬號可「歸檔」：歸檔即強制停用並從賬號池隱藏，僅在後台歸檔區
保留記錄（累計用量可查）；歸檔賬號不參與調度、套餐領取與額度刷新，恢復後
保持停用狀態，需手動啟用才會重新入池。

## License

AGPL-3.0（見 [LICENSE](LICENSE)），僅供學習研究與個人自部署使用；使用本項目產生的
一切行為與後果由使用者自行承擔，請自行遵守上游服務條款。本項目與 Z.AI 無任何關聯。
