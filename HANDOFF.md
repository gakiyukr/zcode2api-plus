# zcode2api Go 版交接文檔

> 給新會話（Claude 或人類協作者）的快速上手指南。計劃與進度台账在 `PLAN.md`（唯一權威），
> 本文檔只做「狀態快照 + 工作流 + 紅線 + 踩坑記錄」，避免重複維護。
> 最後更新：2026-09-15（M0-M8 代码全部完成；M2 后台 UI 已浏览器实测通过；
> M0/M1/M5/M6/M7/M8 真机验收仍待做）。

---

## 1. 當前狀態快照

| 項目 | 狀態 |
|------|------|
| 模塊 | `zcode2api`（Go 1.25，標準庫優先，僅 sqlite/rod 為外部依賴） |
| 倉庫 | `C:\Projects\zcode2api-plus`，分支 `go-rewrite`，remote `origin` = github.com/gakiyukr/zcode2api-plus |
| 分支 | `go-rewrite`（Go 主線）、`python-legacy`（Python 版歸檔，僅作行為契約參照，**不再更新**） |
| 里程碑 | M0-M8 代码全部完成：M0 骨架+數據層、M1 網關核心、M2 Admin API+SPA（**已驗收**）、M3 額度+async、M4 OpenAI 兼容層、M5 rod 驗證碼池、M6 OAuth+代理+CLI+交付、M7 `/v1/responses`、M8 套餐自動領取 |
| 測試 | `go build ./... && go vet ./... && go test ./...` 全綠（13 個含測試的包、171 個 Test 函式）；`-race` **已驗證**（2026-09-15 於 Debian 12 伺服器，Go 1.25.14 + gcc 12，13 包全綠） |
| 行為契約 | Python 版（本倉庫 `python-legacy` 分支，`app/`）為權威對照；Go 版增量見 `PLAN.md` §5.7-§5.9 |

## 2. 新會話上手步驟

1. 讀 `PLAN.md` 全文（約 390 行）——範圍、§5.x 端點契約、里程碑勾選狀態都在那裡。
2. 本文件 §4 紅線與 §6 踩坑記錄**必讀**。
3. 驗證環境：`cd C:\Projects\zcode2api-plus && go build ./... && go vet ./... && go test ./...`。
4. 從 `PLAN.md` 未勾選的第一項開工（當前全部是**真機驗收**項，需真實賬號環境；另有 M9 已知缺陷清單）。
5. 用戶母語溝通用**繁體中文**（AGENTS.md 全局強制）；提交訊息用英文。

## 3. Git 與語言規範（每次提交都適用）

- 提交訊息**英文**，`git commit -S`（強制 GPG 簽名），正文描述做了什麼與為什麼。
- 推送走本地代理：`HTTPS_PROXY=http://127.0.0.1:7890 git push origin go-rewrite`。
- 代碼註釋與文檔：本倉庫既有慣例為簡體中文（M0 起延續，已隨提交固化），新代碼註釋保持
  簡體與周圍一致；**獨立新文檔**（如本文件）按 AGENTS.md 用繁體。代碼標識符一律英文。

## 4. 安全紅線（逐字遵守，無例外）

1. `data/` 目錄含**真實賬號憑證**（accounts.db、device_mid.txt 等）：絕不可提交、複製入
   測試夾具、寫入任何文檔或日誌。測試一律 `config.DBPath = filepath.Join(t.TempDir(), "accounts.db")` 隔離。
2. 後台密鑰（admin key）、網關密鑰（gateway_key）屬敏感信息，不落入倉庫文件。
3. 提交前 `git status` 確認 `data/` 未被暫存（.gitignore 已覆蓋，勿移除）。
4. 本機驗證請用臨時數據目錄（`ZCODE_DATA_DIR=/tmp/...`），勿對真實 `data/` 起服務做實驗。
5. **嚴禁在倉庫目錄跑帶工作區影響的 git 指令**（`git checkout -- .`、`git restore`、`git clean`、
   `git stash` 等）。腳本中呼叫 git 時務必顯式傳 `cwd=`；一次失誤即可抹掉未提交的全部修改。

## 5. 接下來的工作（按 PLAN.md 順序）

**代碼全部完成（M0-M8）**，剩餘全部是**真機驗收**項，需真實賬號環境：

### 欠賬驗收（合併一次做）
- **M0**：真實 Python 版 `data/accounts.db` 互通實測（Go 讀取 → Go 寫回 → Python 版仍可讀）；
  §7 規劃的脫敏 `testdata/` 夾具尚未提交。
- **M1**：真實賬號非流式 + 流式各打通一次。
- **M4**：openai 官方 Python 客戶端指向網關跑通非流式/流式/工具調用三場景。
- **M5**：真實賬號連續 20 次 JWT 請求全自動通過（無 F001）。
- **M6**：Release CI 產物可運行（推 tag 觸發）；兩版本交替用同一 db 無異常。（`-race` 已完成）
- **M7**：Codex CLI 指向網關無狀態模式完整會話。
- **M8**：真機領取一次成功（billing/preview + claim + 激活上報全鏈路）。

### 已驗收
- **M2**：後台 UI 六頁（登入/儀表板/帳號池/代理/驗證中心/設置）瀏覽器實測通過，含經 UI 新增帳號；
  限速單測（`auth_test.go` 5 組）通過。見 PLAN.md §6 M2。

### M9 已知缺陷（代碼審查發現，見 PLAN.md §6 M9）
11 項非阻塞行為問題（asyncpool 分類分歧、BrowserSolver 鎖粒度、include_usage 外洩、
gofmt 未覆蓋、go.mod indirect 標記等），正式發布前處理。

## 6. 踩坑記錄（新會話必讀，避免重蹈）

1. **上游響應形態**：zcode.z.ai 非流式響應是**標準 Anthropic Messages 頂層形態**——
   id/content/stop_reason/usage 都在頂層，**沒有**嵌套 `message` 對象。權威依據是
   `internal/gateway/usage.go` 頭部註釋。M4 曾誤設嵌套形態，e2e 階段才修正。
2. **測試基建三件套**：① config 變量隔離（DBPath/DataDir/UpstreamZai/Fallback）+ `t.TempDir` + t.Cleanup 還原；
   ② captcha.Manager 離線化：`SetSolver`（假求解器）+ `SetConfigProvider`（固定配置）——否則
   FetchConfig 打真實 zcode.z.ai（經代理約 2s/次，離線回退 cn region 會讓斷言漂移）；
   ③ e2e 賬號用 api_key 模式（credential 不含兩個點）即不觸發驗證碼路徑，離線穩定；
   JWT 模式會走 captcha + zcode_system 注入，僅在專測這些語義時使用（gateway 包 e2e 已覆蓋）。
3. **inflight 廣播語義**：quota 的併發去重用 `done chan struct{}` close 廣播 + result 共享讀；
   容量 1 的 channel 只能配對一個接收者，會死鎖（M3 踩過）。
4. **字面反斜杠序列寫文件**：Edit 工具的 JSON 參數層會把 `\u4e2d` 解碼為字符；perl 替換側把
   `\u` 當大小寫轉義。要寫入字面 `\uXXXX` 用 `perl -i -pe 's/\x5cu…/'`（`\x5c` = 字面反斜杠）。
5. **Go map JSON 鍵序**：`encoding/json` 按鍵字母序序列化，字符串斷言不要假設鍵序
   （如 `"delta":{"role":…}`），分鍵 Contains 或用 reflect.DeepEqual。
6. **gateway 工具已導出**（供 openai/asyncpool 複用，勿再寫私有版）：`WriteJSON` / `WriteAuthError` /
   `IncomingHeaders` / `AnyToString` / `ModelAllowed` / `NormalizeBody` / `AvailableModels`。
7. **引擎錯誤體形態**：`runResult.Body` 已是 `{"error":{message,type,code}}`，OpenAI 客戶端
   兼容，handler 直接透傳即可。注意 `runResult` 型別未導出，跨包只能就近取 `.Delivered`/`.Status`/`.Body`。
8. **`go:embed` 的目錄約束**：embed 只能引用宣告檔所在目錄樹內的文件，因此 `frontend/dist`
   的 embed 宣告必須放在**倉庫根包**（`webui.go`），`internal/web` 無法引用；改前端後務必
   `npm install && npm run build` 並把新的 `dist/` 一併提交（舊 hash 檔要刪）。
9. **`.env` 不會被自動載入**：專案未引入 dotenv，`.env` 需部署方自行 source；CI/部署請直接設環境變量。
10. **部署腳本必須保持 LF**：`.gitattributes` 已對 `*.sh` 與 `*.service` 強制 `eol=lf`，
    改動後用 `git ls-files --eol deploy/` 確認索引為 `i/lf`；腳本可執行位用
    `git update-index --chmod=+x` 設定（Windows 上檔案系統不保留該位）。
11. **`deploy/manage.sh` 自足性**：systemd 單元模板同時內嵌在腳本內（`write_unit()` 的 `UNIT_EOF` heredoc），
    單獨下載 `manage.sh` 執行也能運作；修改模板時**兩處都要改**（`deploy/zcode2api.service` 與腳本內嵌版）。
12. **出站代理環境變數**：專案不讀 `HTTP_PROXY` 決定賬號代理，但部分內部 `http.Client`
    未設 Transport，會走 Go 預設 Transport 而**受環境代理影響**。systemd 服務不繼承登入 shell
    環境，但 `/etc/environment` 與 systemd `DefaultEnvironment` 會被繼承，部署時需留意。

## 7. 包結構速查

| 包 | 職責 | 測試 |
|----|------|------|
| `internal/config` | 全部 `ZCODE_*` 環境變量（包級變量，測試直接改） | — |
| `internal/model` | Account 狀態機 + 模型可用性 + JSON 契約 | ✓ |
| `internal/store` | SQLite（modernc 純 Go）+ 密鑰引導 + 輪詢游標 + 代理線路 + 導入導出 | ✓ |
| `internal/upstream` | build_request：頭 + zcode_system 注入 + 客戶端頭過濾 | ✓ |
| `internal/captcha` | 驗證碼：Manager + rod 池（pool/browser_solver/solve）+ 二進制自動下載（browserdl） | ✓（5 檔共 33 組，含 1 組真機除錯預設 skip） |
| `internal/gateway` | /v1/messages + /v1/models、選號循環 engine.go、整形 body.go、分類 classify.go、統計 usage.go | ✓（25 組，含 engine_test 13 組 e2e） |
| `internal/openai` | OpenAI 兼容層：convert / respond / stream / responses / responses_stream / handler | ✓（32 組） |
| `internal/asyncpool` | /async/v1/messages ticket 全語義 | ✓（16 組） |
| `internal/quota` | 額度查詢 + 緩存去重 + 後台 monitor | ✓ |
| `internal/claim` | 套餐自動領取（billing/preview + claim）+ 激活事件上報 | ✓（8 組） |
| `internal/oauth` | Z.AI OAuth 登錄鏈 + API Key 兌換 | ✓ |
| `internal/proxy` | 賬號級出站代理（http/https CONNECT + socks4/4a/5/5h 撥號器 + Transport 緩存） | ✓ |
| `internal/auth` | 網關 fail-closed + 後台限速（10 次/5min） | ✓ |
| `internal/adminapi` | /admin/api/* 全端點（accounts/proxies/login/claim/monitor） | ✓ |
| `internal/web` | 終端日誌（logs.go）+ SPA 托管（spa.go）；`webui.go`（倉庫根）go:embed SPA | — |
| `frontend/` | React SPA（**不重寫**，dist 直接 embed；改前端需 `npm install && npm run build`） | — |
| `cmd/zcode2api` | main.go 入口接線 + cli.go 全部子命令 | — |
| `deploy/` | Linux 一鍵部署：`manage.sh`（交互式管理，二進制 + Docker 的安裝/更新/卸載）、systemd 模板、部署指南 | — |
| `Dockerfile`、`docker-compose.yml` | 容器部署參考實作（**未經驗證、不保證可用**，僅供起點） | — |

## 8. 驗證命令

```bash
cd C:\Projects\zcode2api-plus
go build ./... && go vet ./... && go test ./...   # 全量驗證（當前全綠）
go test ./internal/openai/ -v                     # OpenAI 轉換層詳情
go build -o z2a ./cmd/zcode2api && ZCODE_DATA_DIR=/tmp/z2a ./z2a serve   # 臨時目錄起服務自測
CGO_ENABLED=1 go test -race ./...                 # 競態檢查（需 gcc；本機無，已於伺服器驗證）
HTTPS_PROXY=http://127.0.0.1:7890 git push origin go-rewrite   # 推送

# 部署腳本（僅 Linux；Windows 上可用 WSL 驗證語法與流程）
bash -n deploy/manage.sh && echo "語法 OK"
bash deploy/manage.sh help                        # 選項說明（非 root 可看）
git ls-files --eol deploy/                        # 確認索引為 i/lf 且腳本為 100755
```

## 9. 部署（僅 Linux）

完整指南見 `deploy/README.md`。**單一入口是 `deploy/manage.sh`**：

```bash
sudo ./deploy/manage.sh            # 交互式選單（安裝/更新/卸載/狀態/服務控制）
sudo ./deploy/manage.sh install -y # 非交互（腳本/CI 用）
```

- **二進制**（**已實測**）：`manage.sh install` / `update` / `uninstall`。
  `update` 會比對 Release 版本、備份舊二進制、下載失敗自動回滾。
  自動處理系統依賴、systemd 服務、密鑰提示。
- **Docker**（**參考實作，未經驗證、不保證可用**）：`manage.sh docker-install` /
  `docker-update` / `docker-uninstall`。開發環境無容器運行時，從未執行 `docker build`。
  腳本會先顯示未驗證警告並要求確認，再校驗構建上下文完整性。
  兩卷必須持久化：`/app/data`（賬號庫）、`/app/browser`（Chromium 緩存）。
- 驗證碼瀏覽器的系統依賴清單由 `ldd` 對官方二進制**實測得出**，並按發行版命名差異
  （`libasound2` vs `libasound2t64`）在安裝時逐個探測存在性，不硬編碼發行版。
- 管理腳本的 systemd 模板內嵌於 `write_unit()` 的 heredoc，**與 `deploy/zcode2api.service`
  兩處需同步修改**（腳本優先使用同目錄模板檔，缺失時回退內嵌版）。
