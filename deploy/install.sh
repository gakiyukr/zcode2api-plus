#!/usr/bin/env bash
# zcode2api-plus 裸機部署腳本（Debian/Ubuntu，systemd，免 Docker）
#
# 用法：
#   sudo bash deploy/install.sh                 # 標準安裝：Node + jsdom 求解器（免 Chromium）
#   sudo bash deploy/install.sh --with-browser  # 額外安裝真實 Chromium 驗證碼池
#
# 流程：安裝系統依賴 → 建 venv → 裝 Python / Node 依賴 → 產生環境設定 →
#       註冊並啟動 systemd 服務。可重複執行（更新依賴、重啟服務），
#       既有環境設定與資料目錄不會被覆蓋。
#
# 進階：APP_DIR=/srv/zcode2api RUN_USER=app sudo bash deploy/install.sh
set -euo pipefail

WITH_BROWSER=0
for arg in "$@"; do
  case "$arg" in
    --with-browser) WITH_BROWSER=1 ;;
    *) echo "未知參數: $arg（支援：--with-browser）" >&2; exit 1 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "請以 root 執行：sudo bash deploy/install.sh" >&2; exit 1; }

# 專案目錄預設為本腳本所在的倉庫根目錄；也可用 APP_DIR 覆蓋
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${APP_DIR:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
RUN_USER="${RUN_USER:-zcode2api}"
RUN_GROUP="$RUN_USER"
ENV_FILE="${APP_DIR}/deploy/zcode2api.env"

echo "==> 專案目錄：${APP_DIR}"
[[ -f "${APP_DIR}/main.py" && -f "${APP_DIR}/requirements.txt" ]] || {
  echo "錯誤：${APP_DIR} 不是專案根目錄（缺 main.py / requirements.txt）" >&2
  exit 1
}

# ── 1. 系統依賴 ─────────────────────────────────────────────────────────────
export DEBIAN_FRONTEND=noninteractive
echo "==> 安裝基礎套件（curl / git）"
apt-get update -qq
apt-get install -y --no-install-recommends curl ca-certificates gnupg git >/dev/null

# Python 3.11（Debian 12 自帶；Ubuntu 經 deadsnakes 取得；其餘回退系統 python3）
echo "==> 準備 Python 3.11"
if ! command -v python3.11 >/dev/null 2>&1; then
  if [[ -r /etc/os-release ]]; then
    . /etc/os-release
    if [[ "${ID:-}" == "ubuntu" ]]; then
      apt-get install -y --no-install-recommends software-properties-common >/dev/null
      add-apt-repository -y ppa:deadsnakes/ppa >/dev/null
      apt-get update -qq
      apt-get install -y --no-install-recommends python3.11 python3.11-venv >/dev/null
    fi
  fi
fi
PY_BIN="$(command -v python3.11 || command -v python3)"
echo "    使用解譯器：${PY_BIN} ($("${PY_BIN}" -c 'import sys;print("%d.%d" % sys.version_info[:2])'))"
if ! "${PY_BIN}" -m ensurepip --version >/dev/null 2>&1; then
  apt-get install -y --no-install-recommends python3-venv python3-pip >/dev/null
fi

# Node.js >= 18：驗證碼求解器以 Node 子進程運行（不裝也能跑，但 JWT 帳號無法過無痕驗證）
echo "==> 準備 Node.js"
NODE_MAJOR=0
if command -v node >/dev/null 2>&1; then
  NODE_MAJOR="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)"
fi
if [[ "${NODE_MAJOR}" -lt 18 ]]; then
  curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null
  apt-get install -y --no-install-recommends nodejs >/dev/null
fi
echo "    node $(node --version)"

# ── 2. Python 依賴 ──────────────────────────────────────────────────────────
echo "==> 建立 venv 並安裝 Python 依賴"
[[ -d "${APP_DIR}/venv" ]] || "${PY_BIN}" -m venv "${APP_DIR}/venv"
"${APP_DIR}/venv/bin/pip" install --upgrade pip >/dev/null
"${APP_DIR}/venv/bin/pip" install -r "${APP_DIR}/requirements.txt"

# ── 3. 驗證碼求解器（Node）依賴 ─────────────────────────────────────────────
echo "==> 安裝驗證碼求解器（Node）依賴"
( cd "${APP_DIR}/captcha_node" && npm ci --omit=dev >/dev/null )

# ── 4. 可選：真實 Chromium 驗證碼池（--with-browser）────────────────────────
CAPTCHA_BROWSER_VALUE=false
if [[ "${WITH_BROWSER}" -eq 1 ]]; then
  echo "==> 安裝 Chromium 系統庫並預下載瀏覽器二進制"
  # 庫清單與 Dockerfile 一致（Debian bookworm 已驗證可啟動）
  apt-get install -y --no-install-recommends \
    fonts-liberation libasound2 libatk-bridge2.0-0 libatk1.0-0 libcairo2 \
    libcups2 libdbus-1-3 libdrm2 libexpat1 libfontconfig1 libgbm1 \
    libglib2.0-0 libgtk-3-0 libnspr4 libnss3 libpango-1.0-0 libx11-6 \
    libx11-xcb1 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
    libxkbcommon0 libxrandr2 xdg-utils >/dev/null
  CLOAKBROWSER_CACHE_DIR="${APP_DIR}/data/cloakbrowser-cache" \
    "${APP_DIR}/venv/bin/python" -m cloakbrowser install
  CAPTCHA_BROWSER_VALUE=true
fi

# ── 5. 執行帳號與資料目錄 ───────────────────────────────────────────────────
echo "==> 建立系統帳號 ${RUN_USER} 與資料目錄"
id -u "${RUN_USER}" >/dev/null 2>&1 || \
  useradd --system --home-dir "${APP_DIR}" --no-create-home --shell /usr/sbin/nologin "${RUN_USER}"
mkdir -p "${APP_DIR}/data"
chown -R "${RUN_USER}:${RUN_GROUP}" "${APP_DIR}"

# ── 6. 環境設定（已存在則保留，不覆蓋）─────────────────────────────────────
if [[ ! -f "${ENV_FILE}" ]]; then
  echo "==> 產生環境設定 ${ENV_FILE}"
  sed -e "s#^ZCODE_DATA_DIR=.*#ZCODE_DATA_DIR=${APP_DIR}/data#" \
      -e "s#^ZCODE_NODE_PATH=.*#ZCODE_NODE_PATH=$(command -v node)#" \
      -e "s#^ZCODE_CAPTCHA_BROWSER=.*#ZCODE_CAPTCHA_BROWSER=${CAPTCHA_BROWSER_VALUE}#" \
      -e "s#^CLOAKBROWSER_CACHE_DIR=.*#CLOAKBROWSER_CACHE_DIR=${APP_DIR}/data/cloakbrowser-cache#" \
      "${APP_DIR}/deploy/zcode2api.env.example" > "${ENV_FILE}"
else
  echo "    保留既有環境設定 ${ENV_FILE}"
fi

# ── 7. systemd 服務 ─────────────────────────────────────────────────────────
echo "==> 註冊 systemd 服務"
sed -e "s#/opt/zcode2api#${APP_DIR}#g" \
    -e "s/^User=zcode2api$/User=${RUN_USER}/" \
    -e "s/^Group=zcode2api$/Group=${RUN_GROUP}/" \
    "${APP_DIR}/deploy/zcode2api.service" > /etc/systemd/system/zcode2api.service
systemctl daemon-reload
systemctl enable zcode2api >/dev/null
systemctl restart zcode2api

sleep 2
systemctl --no-pager --lines=0 status zcode2api || true
echo
echo "✔ 部署完成。"
echo "  服務管理 : systemctl {status|restart|stop} zcode2api"
echo "  即時日誌 : journalctl -u zcode2api -f"
echo "  環境設定 : ${ENV_FILE}（修改後 systemctl restart zcode2api）"
echo "  後台     : http://<主機>:3000/admin/login（預設密碼 zcode，登入後請立即修改）"
