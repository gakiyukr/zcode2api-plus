# zcode2api 部署指南（Linux）

本目錄提供 Linux 的一鍵部署：**裸二進制 + systemd**（推薦、已實測）與 **Docker**（未經驗證）。

本目錄面向 Linux 服務端；Releases 僅提供 linux/amd64 與 linux/arm64 產物。

> ⚠️ **Docker 方案不提供任何保證**
>
> 本倉庫的 `Dockerfile` 與 `docker-compose.yml` 為**盡力而為的參考實作**，
> **從未經 `docker build` 驗證**（開發環境無容器運行時）。可能無法構建或運行。
>
> 二進制方案（方案 A）已完整實測，建議優先使用。
> 若要用 Docker，請自行驗證與調整，不保證可用性、不保證後續維護。

---

## 方案選擇

| 方案 | 狀態 | 適用場景 | 注意 |
|------|------|----------|------|
| [二進制 + systemd](#方案-a裸二進制--systemd推薦) | **已實測** | 長期運行的伺服器 | 需要 root 執行管理腳本 |
| [Docker](#方案-bdocker未經驗證) | **未經驗證，不保證可用** | 自行承擔風險 | 映像需在自有伺服器構建；無 CI 映像 |

兩者都透過 [`manage.sh`](#交互式管理腳本推薦入口) 管理（安裝／更新／卸載）。

---

## 交互式管理腳本（推薦入口）

`manage.sh` 是單一入口，涵蓋二進制與 Docker 的安裝／更新／卸載，並提供交互式選單。

```bash
sudo ./deploy/manage.sh          # 交互式選單
```

選單結構：

```
  二进制: 已安装 v2.0.1-go [active]
  Docker 已安装

  1) 安装二进制          5) Docker 安装（未验证）
  2) 更新二进制          6) Docker 更新
  3) 卸载二进制          7) Docker 卸载
  4) 查看状态            8) 服务控制（启停/重启/日志）
  0) 退出
```

### 非交互用法（供腳本／CI）

```bash
sudo ./deploy/manage.sh install            # 二進制安裝
sudo ./deploy/manage.sh update             # 更新（自動比對 Release 版本）
sudo ./deploy/manage.sh uninstall          # 卸載
sudo ./deploy/manage.sh status             # 查看狀態
sudo ./deploy/manage.sh docker-install     # Docker 安裝
sudo ./deploy/manage.sh docker-update      # Docker 更新
sudo ./deploy/manage.sh docker-uninstall   # Docker 卸載
```

### 常用選項

```bash
--dir DIR            # 安裝目錄（預設 /opt/zcode2api）
--port PORT          # 監聽端口（預設 3000）
--host ADDR          # 監聽地址（預設 0.0.0.0；反向代理後建議 127.0.0.1）
--user USER          # 以非特權賬號運行（不存在則自動建立）
--local              # 從本機源碼構建（需 Go 工具鏈）
--version TAG        # 指定 Release 標籤，預設取最新
--no-browser         # 不裝驗證碼瀏覽器依賴（改用後台人工回填）
--no-prefetch-browser  # 安裝時不預下載 Chromium（預設會下載，約 200MB）
--no-deps            # 跳過系統依賴安裝
--purge              # 卸載時連同數據目錄刪除
--keep-user          # 卸載時保留系統賬號
--volumes            # Docker 卸載時連同數據卷刪除
-y, --yes            # 所有確認自動回答 yes（非交互場景）
```

完整說明：`./deploy/manage.sh help`

### 單檔部署

`manage.sh` 自帶 systemd 單元模板（內嵌 heredoc），可單獨複製到伺服器使用：

```bash
scp deploy/manage.sh root@server:/root/
ssh root@server 'bash manage.sh install'
```

---

## 方案 A：裸二進制 + systemd（推薦）

### 一鍵安裝

```bash
# 交互式（推薦）
sudo ./deploy/manage.sh

# 或非交互
sudo ./deploy/manage.sh install
sudo ./deploy/manage.sh install --port 3010 --user zcode
```

安裝完成後會自動啟動服務，並在首次啟動時把**後台密碼**與**網關 API Key** 打印到日誌與終端。
**請立即保存這兩個密鑰**——之後不再顯示（可在後台「系統設定」頁查看）。

### 安裝腳本做了什麼

1. 檢查 root 權限與 CPU 架構（僅 amd64 / arm64）
2. 安裝基礎依賴（`curl`、`tar`）
3. 安裝驗證碼瀏覽器的共享庫（見 [驗證碼瀏覽器](#驗證碼瀏覽器重要)）
4. 取得二進制：從 GitHub Releases 下載，或用 `--local` 就地編譯
5. 生成 `/opt/zcode2api/.env`（已存在則保留）
6. **預下載補丁 Chromium**（約 200MB，實測約 20 秒；`--no-prefetch-browser` 可跳過）
7. 渲染並安裝 `/etc/systemd/system/zcode2api.service`
8. `systemctl enable --now zcode2api`

> **為何預設預下載**：瀏覽器池是惰性啟動的，若不在安裝階段下載，
> 首次 JWT 請求會因驗證碼不可用而失敗（並觸發 60 秒冷卻），
> 使用者需等下一次請求才成功。安裝時一次下載可完全避免此情況。
>
> 已存在快取時命令會直接跳過，不會重複下載。
> 也可事後單獨執行：`zcode2api prefetch-browser`。

### 目錄結構

```
/opt/zcode2api/
├── zcode2api              # 二進制
├── .env                   # 環境配置（權限 0600）
├── .installed-version     # 已安裝的 Release 標籤
├── data/                  # SQLite 賬號庫、密鑰、設備指紋  ← 必須備份
└── browser/               # 補丁 Chromium 緩存（可重建，不必備份）
```

### 更新

```bash
sudo ./deploy/manage.sh update        # 交互式：顯示當前與最新版本，確認後更新
sudo ./deploy/manage.sh update -y     # 非交互
```

更新流程：查詢最新 Release → 比對當前版本 → 備份現有二進制 → 停止服務 →
下載新版 → 失敗自動回滾 → 啟動服務。若已是最新版，會明確提示。

### 日常運維

```bash
sudo ./deploy/manage.sh status      # 狀態總覽（含版本、服務、數據大小、Docker）
sudo ./deploy/manage.sh             # 選單 8：啟停/重啟/實時日誌
systemctl status zcode2api          # 或直接用 systemctl
journalctl -u zcode2api -f
```

### 卸載

```bash
sudo ./deploy/manage.sh uninstall             # 移除服務與二進制，保留數據
sudo ./deploy/manage.sh uninstall --purge     # 連同數據一併刪除
sudo ./deploy/manage.sh uninstall --keep-user # 保留安裝時建立的系統賬號
```

---

## 方案 B：Docker（未經驗證）

> ⚠️ **本方案未經 `docker build` 驗證，不保證可用。** 以下內容為參考實作與預期用法，
> 可能因基礎映像變動、依賴清單不完整或 `go:embed` 路徑問題而構建失敗。
> 請自行驗證與調整；本專案不對 Docker 路徑提供支援承諾。

**映像在自有伺服器上構建**，不使用任何 CI 預編譯產物。

### 用管理腳本（推薦）

```bash
sudo ./deploy/manage.sh docker-install     # 或選單 5
sudo ./deploy/manage.sh docker-update      # 或選單 6
sudo ./deploy/manage.sh docker-uninstall   # 或選單 7（--volumes 一併刪數據卷）
```

腳本會：確認 `docker compose` 可用 → 顯示未驗證警告 → 在當前倉庫或克隆到
`/opt/zcode2api-docker` → 校驗構建上下文（`Dockerfile` 與 `docker-compose.yml` 齊備）
→ 執行 `compose up -d --build`。

### 手動構建（預期用法）

```bash
git clone https://github.com/gakiyukr/zcode2api-plus.git
cd zcode2api-plus
docker compose up -d --build
docker compose logs -f              # 首次啟動會輸出後台密碼與網關密鑰（預期行為）
```

或不用 compose：

```bash
docker build -t zcode2api:latest .
docker run -d --name zcode2api \
  -p 3000:3000 \
  -v zcode2api-data:/app/data \
  -v zcode2api-browser:/app/browser \
  -e ZCODE_CAPTCHA_BROWSER=true \
  zcode2api:latest
```

### 構建參數

| 參數 | 預設 | 說明 |
|------|------|------|
| `PREFETCH_BROWSER` | `false` | 設 `true` 在構建時預下載 Chromium（約 200MB）。映像更大，但首次啟動即可用 |

```bash
docker build -t zcode2api:latest --build-arg PREFETCH_BROWSER=true .
```

### 必須持久化的卷

| 容器路徑 | 內容 | 不持久化的後果 |
|----------|------|----------------|
| `/app/data` | SQLite 賬號庫、密鑰、設備指紋 | **重建容器即丟失全部賬號** |
| `/app/browser` | 補丁 Chromium 緩存 | 每次重建都重新下載約 200MB |

### 容器內以 root 運行

映像預設以 root 運行，這是刻意的：

- Chromium 啟動參數已硬編碼 `--no-sandbox`（見 `internal/captcha/solve.go`），無沙箱可失去；
- 避免綁定掛載卷的 UID 不匹配——自架部署最常見的坑。

若需非 root 運行，請自行在 compose 中指定 `user:` 並確保兩個卷的屬主匹配。

---

## 驗證碼瀏覽器（重要）

JWT 賬號請求需要阿里雲無痕驗證令牌。程式用補丁 Chromium（cloakbrowser）自動求解，
該瀏覽器由程式**首次使用時自動下載**（約 200MB，SHA256SUMS + Ed25519 簽名校驗）。

### 系統依賴

補丁 Chromium 需要一批圖形與 NSS 共享庫。`manage.sh` 會自動安裝（**此路徑已實測**）；
Docker 映像的依賴清單亦按同一份 `ldd` 結果寫入，但**未經構建驗證**。

**依賴清單按發行版命名差異處理**：安裝前逐個探測套件是否存在，因此
Ubuntu 24.04+ 的 `libasound2t64` 與 Debian 的 `libasound2` 都能正確解析。

> 依賴清單由對官方 `cloakbrowser-linux-x64` 二進制執行 `ldd` 實測得出，非推測。

### 三種取得方式（按優先級）

1. `ZCODE_CAPTCHA_BROWSER_BIN` — 指定任意已有的 Chromium 二進制
2. `CLOAKBROWSER_BINARY_PATH` — 同上（cloakbrowser 慣例命名）
3. `CLOAKBROWSER_CACHE_DIR` 下已有的 `chromium-*` 目錄 → 自動下載

### 不安裝瀏覽器依賴的情況

用 `--no-browser` 安裝（或 `ZCODE_CAPTCHA_BROWSER=false`），驗證碼回退到**後台人工回填**
（`/admin/captcha`），功能不中斷，但需人工介入。

> 部分發行版自帶的 Chromium 會被上游風控拒絕（實測 Debian 13 的系統 Chromium 如此），
> 這也是預設自動下載補丁二進制的原因。

---

## 部署注意事項

### 出站代理環境變數

程式**不讀取** `HTTP_PROXY` / `HTTPS_PROXY` 來決定賬號出站代理（賬號代理在後台按賬號配置）。
但部分內部 HTTP 客戶端使用 Go 標準庫的預設 Transport，**會受這些環境變數影響**。

systemd 系統服務不繼承登入 shell 的環境變數，因此正常情況下不受影響；
但若你透過 `/etc/environment` 或 systemd 的 `DefaultEnvironment` 設定了全域代理，
上游請求可能被導向該代理。建議在伺服器上明確檢查：

```bash
systemctl show-environment | grep -i proxy
```

### 反向代理部署（重要）

後台登入失敗限速以 `RemoteAddr` 為鍵，**刻意不信任 `X-Forwarded-For`**
（無條件信任該頭會讓攻擊者偽造來源繞過限速）。

這帶來一個後果：**若把服務放在反向代理之後，所有請求的 `RemoteAddr` 都是反代 IP**，
於是 10 次失敗（無論來自多少個不同客戶端）就會鎖死整個後台——單一 IP 攻擊者
即可用 10 個請求讓所有人無法登入。

**解法：讓服務只監聽回環，由反向代理在本機轉發。**

```bash
# 安裝時指定
sudo ./deploy/manage.sh install --host 127.0.0.1

# 或改既有安裝的 .env 後重啟
sudo sed -i 's/^ZCODE_HOST=.*/ZCODE_HOST=127.0.0.1/' /opt/zcode2api/.env
sudo systemctl restart zcode2api
```

這樣外部無法直連該端口（實測外部介面連線被拒），只能經反向代理，
而反代與服務同機，`RemoteAddr` 恆為 `127.0.0.1`——攻擊面收斂到「只能從本機發起」。
Nginx 範例：

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_set_header Host $host;
    # SSE 透傳（/v1/messages 串流必需）
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

> 注意：`--host 127.0.0.1` 時，**網關端點（`/v1/messages` 等）也只監聽本機**。
> 若 API 客戶端在外部，需一併經反向代理轉發，或改用 `--host 0.0.0.0` 並
> 僅把後台路徑限制在內網（例如 Nginx 對 `/admin` 加 IP 白名單）。

### 資料備份

**只需備份 `data/` 與 `.env`**——二進制可從 Releases 或源碼重建，瀏覽器快取會自動下載。

```bash
# 停止服務後複製（含 SQLite 的 WAL 文件，三者必須一起）
systemctl stop zcode2api
tar -cJf /backup/zcode2api-$(date +%F).tar.xz -C /opt/zcode2api data .env .installed-version
systemctl start zcode2api
```

實測：資料目錄 4.2MB（其中 WAL 4MB），壓縮後約 **35KB**。

| 備份項 | 必要性 |
|--------|--------|
| `data/accounts.db` + `-wal` + `-shm` | **必須**（帳號憑證、額度狀態、用量統計） |
| `data/device_mid.txt` | **必須**（裝置指紋，缺失會被上游視為新裝置） |
| `.env` | 建議（含 `ZCODE_PORT`、`ZCODE_CAPTCHA_BROWSER_BIN` 等部署參數） |
| `.installed-version` | 可選（供 `manage.sh update` 比對版本） |
| 二進制 | **不需**（`manage.sh install` 或 Releases 重新取得） |
| `browser/` | **不需**（首次使用時自動下載，約 200MB） |

### 從備份還原

```bash
# 1. 部署新實例（會自動取得二進制與系統依賴）
sudo ./deploy/manage.sh install --port 3002 --no-prefetch-browser

# 2. 停止服務，還原資料
sudo systemctl stop zcode2api
sudo tar -xJf /backup/zcode2api-2026-09-15.tar.xz -C /opt/zcode2api
sudo chown -R zcode2api:zcode2api /opt/zcode2api/data
sudo systemctl start zcode2api

# 3. 驗證
curl -sS http://127.0.0.1:3002/meta          # {"version":"..."}
sudo journalctl -u zcode2api -n 20           # 確認無錯誤
```

> **還原後必須修正屬主**：備份保留了原始 UID/GID，若目標機器沒有同 UID 的賬號，
> 服務（以 `User=zcode2api` 運行）將無法讀寫資料庫。
>
> **WAL 必須一起還原**：只還原 `accounts.db` 會丟失 WAL 中尚未合併的交易。

也可用後台的匯出功能（`/admin/api/export`，含明文憑證，請妥善保管）。

> 資料庫與 Python 版（`python-legacy` 分支）完全互通，可互相接續使用同一份 `data/accounts.db`。

### 防火牆

```bash
# 僅放行網關端口；後台建議限制來源 IP
sudo ufw allow 3000/tcp
```

---

## 疑難排解

| 現象 | 原因與處理 |
|------|-----------|
| 服務啟動失敗 | `journalctl -u zcode2api -n 50`；常見為端口佔用或數據目錄權限 |
| 網頁打不開但服務在跑 | 檢查 `ZCODE_HOST`（預設 `0.0.0.0`）與防火牆 |
| 驗證碼一直失敗 | 看日誌是否為瀏覽器池啟動失敗；確認已裝瀏覽器依賴，或改用後台人工回填 |
| 上游請求全部失敗 | 檢查是否被全域代理環境變數影響（見上） |
| Docker 內驗證碼失敗 | 該路徑未經驗證；先確認映像能構建，再檢查 `ZCODE_CAPTCHA_BROWSER=true` 與 `/app/browser` 卷可寫 |
| 忘記後台密碼 | 後台「系統設定」可查看；或用 `zcode2api set-admin-key <新密碼>` |

---

## 安全提醒

- `.env` 與 `data/` 含**明文賬號憑證與密鑰**，權限應為 `0600`（腳本已設定），切勿提交到版本庫。
- 網關 API Key 為 fail-closed：未配置時拒絕所有網關請求，不會放行未鑑權流量。
- 首次啟動的隨機密鑰只顯示一次，請立即保存。
