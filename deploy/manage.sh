#!/usr/bin/env bash
# zcode2api Linux 管理腳本：二進制 / Docker 的安裝、更新、卸載與狀態查看。
#
# 交互式菜單（無參數運行時）：
#   sudo ./deploy/manage.sh
#
# 非交互子命令（供腳本/CI 調用）：
#   sudo ./deploy/manage.sh install          # 二進制安裝
#   sudo ./deploy/manage.sh update           # 二進制更新（比對版本）
#   sudo ./deploy/manage.sh uninstall        # 二進制卸載
#   sudo ./deploy/manage.sh status           # 查看安裝狀態
#   sudo ./deploy/manage.sh docker-install   # Docker 安裝
#   sudo ./deploy/manage.sh docker-update    # Docker 更新
#   sudo ./deploy/manage.sh docker-uninstall # Docker 卸載
#
# 僅支持 Linux。Windows / macOS 請直接使用 Releases 二進制。
set -euo pipefail

REPO="gakiyukr/zcode2api-plus"
REPO_URL="https://github.com/$REPO.git"
DIR="/opt/zcode2api"
DOCKER_DIR="/opt/zcode2api-docker"
PORT="3000"
HOST="0.0.0.0"
RUN_USER=""
RUN_GROUP=""
ENABLE_BROWSER="true"
WITH_DEPS="true"
SOURCE="release"
VERSION=""
# 預下載補丁 Chromium：預設開啟，避免新機首次請求等待數分鐘。
# 用 --no-prefetch-browser 關閉（或 --no-browser 一併跳過驗證碼瀏覽器）。
PREFETCH_BROWSER="true"
PURGE="false"
KEEP_USER="false"
ADOPT_DIR=""
DOCKER_VOLUMES="false"
ASSUME_YES="false"

SELF_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname -- "$SELF_DIR")"
SELF_BASENAME="$(basename -- "${BASH_SOURCE[0]}")"
# 掃描根目錄：手工部署常見於 /opt 下的自建目錄，故以 /opt 爲界遞歸查找。
SCAN_ROOT="${SCAN_ROOT:-/opt}"

# ── 輸出 ────────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
	C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'
	C_DIM=$'\033[90m'; C_BOLD=$'\033[1m'; C_CYAN=$'\033[36m'; C_RST=$'\033[0m'
else
	C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_BOLD=""; C_CYAN=""; C_RST=""
fi
info() { printf '%s[*]%s %s\n' "$C_DIM" "$C_RST" "$*"; }
ok()   { printf '%s[+]%s %s\n' "$C_OK" "$C_RST" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_WARN" "$C_RST" "$*" >&2; }
die()  { printf '%s[x]%s %s\n' "$C_ERR" "$C_RST" "$*" >&2; exit 1; }
hr()   { printf '%s%s%s\n' "$C_DIM" "──────────────────────────────────────────────────────────" "$C_RST"; }

confirm() {
	[ "$ASSUME_YES" = "true" ] && return 0
	local reply
	printf '%s?%s %s [y/N] ' "$C_CYAN" "$C_RST" "$1"
	read -r reply || return 1
	case "$reply" in [yY]|[yY][eE][sS]) return 0 ;; *) return 1 ;; esac
}

# ── 環境檢查 ────────────────────────────────────────────────────────────────
require_root() {
	[ "$(id -u)" -eq 0 ] || die "需要 root 權限。請用: sudo $0 $*"
}

require_linux() {
	[ "$(uname -s)" = "Linux" ] || die "本腳本僅支持 Linux（當前: $(uname -s)）"
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "不支持的架構: $(uname -m)（僅提供 amd64 / arm64）" ;;
	esac
}

detect_pkg_mgr() {
	PKG_MGR=""
	for m in apt-get dnf yum pacman apk; do
		if command -v "$m" >/dev/null 2>&1; then PKG_MGR="$m"; break; fi
	done
}

# validate_port 校驗 $PORT 合法性（交互與非交互路徑共用）。
validate_port() {
	case "$PORT" in
		''|*[!0-9]*) die "端口必須是數字: $PORT" ;;
	esac
	if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
		die "端口超出範圍（1-65535）: $PORT"
	fi
}

# validate_host 校驗 $HOST：接受 IP 字面量或空（表示全部接口）。
# 反向代理部署應設爲 127.0.0.1，只監聽迴環。
validate_host() {
	[ -z "$HOST" ] && return 0
	case "$HOST" in
		0.0.0.0|::|127.0.0.1|::1) return 0 ;;
	esac
	# 其餘情況要求是合法 IP 字面量（不做 DNS 解析，避免啟動期依賴網絡）
	if ! printf '%s' "$HOST" | grep -qE '^[0-9a-fA-F:.]+$'; then
		die "ZCODE_HOST 必須是 IP 字面量（如 127.0.0.1、0.0.0.0、::）: $HOST"
	fi
}

# ── 系統依賴 ────────────────────────────────────────────────────────────────
# 驗證碼瀏覽器（cloakbrowser 補丁 Chromium）所需的共享庫。
# 清單由 ldd 對官方 linux-x64 二進制實測得出；按發行版命名差異列出別名，
# 安裝前逐個探測存在性，避免因 t64 後綴等命名變化整體失敗。
BROWSER_DEPS_APT="libasound2t64 libasound2 libatk1.0-0t64 libatk1.0-0 libatk-bridge2.0-0t64 libatk-bridge2.0-0
libatspi2.0-0t64 libatspi2.0-0 libavahi-client3 libavahi-common3 libcairo2 libcups2t64 libcups2 libdatrie1
libdrm2 libfontconfig1 libfreetype6 libfribidi0 libgbm1 libglib2.0-0t64 libglib2.0-0 libgraphite2-3
libharfbuzz0b libnspr4 libnss3 libpango-1.0-0 libpixman-1-0 libpng16-16t64 libpng16-16 libthai0
libx11-6 libxau6 libxcb1 libxcb-render0 libxcb-shm0 libxcomposite1 libxdamage1 libxdmcp6 libxext6
libxfixes3 libxi6 libxkbcommon0 libxrandr2 libxrender1 libvulkan1 fonts-liberation"
BROWSER_DEPS_DNF="alsa-lib atk at-spi2-atk at-spi2-core avahi-libs cairo cups-libs libdrm fontconfig freetype
fribidi mesa-libgbm glib2 graphite2 harfbuzz nspr nss pango pixman libpng libthai libX11 libXau libxcb
libXcomposite libXdamage libXext libXfixes libXi libxkbcommon libXrandr libXrender vulkan-loader liberation-fonts"
BROWSER_DEPS_PACMAN="alsa-lib atk at-spi2-atk at-spi2-core avahi cairo cups libdrm fontconfig freetype2 fribidi
mesa glib2 graphite harfbuzz nspr nss pango pixman libpng libthai libx11 libxau libxcb libxcomposite
libxdamage libxext libxfixes libxi libxkbcommon libxrandr libxrender vulkan-icd-loader ttf-liberation"
BROWSER_DEPS_APK="alsa-lib atk at-spi2-atk at-spi2-core avahi cairo cups-libs libdrm fontconfig freetype
fribidi mesa-gl glib graphite2 harfbuzz nspr nss pango pixman libpng libthai libx11 libxau libxcb libxcomposite
libxdamage libxext libxfixes libxi libxkbcommon libxrandr libxrender vulkan-loader ttf-liberation"

filter_available() {
	local out="" p
	for p in $1; do
		case "$PKG_MGR" in
			apt-get) apt-cache show "$p" >/dev/null 2>&1 && out="$out $p" ;;
			dnf|yum) "$PKG_MGR" -q list --available "$p" >/dev/null 2>&1 && out="$out $p" ;;
			pacman) pacman -Si "$p" >/dev/null 2>&1 && out="$out $p" ;;
			apk) apk search -e "$p" >/dev/null 2>&1 && out="$out $p" ;;
		esac
	done
	printf '%s' "${out# }"
}

pkg_install() {
	[ -n "$1" ] || return 0
	info "安裝系統依賴: $1"
	case "$PKG_MGR" in
		apt-get) DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $1 ;;
		dnf) dnf install -y $1 ;;
		yum) yum install -y $1 ;;
		pacman) pacman -S --noconfirm --needed $1 ;;
		apk) apk add --no-cache $1 ;;
	esac
}

install_base_deps() {
	[ "$WITH_DEPS" = "true" ] || { info "跳過系統依賴（--no-deps）"; return 0; }
	[ -n "$PKG_MGR" ] || { warn "未識別包管理器，請手動安裝 curl 與 tar"; return 0; }
	local need=""
	for c in curl tar; do
		command -v "$c" >/dev/null 2>&1 || need="$need $c"
	done
	pkg_install "$(filter_available "$need")"
}

install_browser_deps() {
	[ "$ENABLE_BROWSER" = "true" ] || return 0
	[ "$WITH_DEPS" = "true" ] || { info "跳過瀏覽器依賴（--no-deps）"; return 0; }
	[ -n "$PKG_MGR" ] || { warn "未識別包管理器，請手動安裝驗證碼瀏覽器依賴"; return 0; }
	case "$PKG_MGR" in
		apt-get) info "更新包索引"; apt-get update -qq ;;
		apk) apk update >/dev/null ;;
	esac
	local candidates=""
	case "$PKG_MGR" in
		apt-get) candidates="$BROWSER_DEPS_APT" ;;
		dnf|yum) candidates="$BROWSER_DEPS_DNF" ;;
		pacman) candidates="$BROWSER_DEPS_PACMAN" ;;
		apk) candidates="$BROWSER_DEPS_APK" ;;
	esac
	local resolved
	resolved="$(filter_available "$candidates")"
	[ -n "$resolved" ] || { warn "瀏覽器依賴清單在本發行版上無法解析，請手動安裝"; return 0; }
	pkg_install "$resolved"
}

# ── 版本與下載 ──────────────────────────────────────────────────────────────
# human_size 把字節數格式化爲人類可讀形式（純 awk，避免依賴 numfmt，
# 後者屬於 coreutils 但部分精簡鏡像會裁掉）。
human_size() {
	awk -v n="${1:-0}" 'BEGIN {
		split("B KB MB GB TB", u, " ")
		i = 1
		while (n >= 1024 && i < 5) { n /= 1024; i++ }
		printf (i == 1 ? "%.0f %s" : "%.1f %s"), n, u[i]
	}'
}

installed_version() {
	[ -f "$DIR/.installed-version" ] && cat "$DIR/.installed-version" 2>/dev/null || true
}

latest_tag() {
	# || true：API 限流或網絡故障時 curl 返回非 0，set -e + pipefail 會讓腳本在此
	# 直接退出，連調用方的錯誤提示都來不及打印——表現爲「查詢最新 Release」後
	# 無聲終止。查詢失敗應返回空串，由調用方決定如何提示。
	curl -fsSL --connect-timeout 15 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true
}

# build_local 從源碼構建到 $DIR/zcode2api。
# 返回非 0 表示失敗（調用方決定回滾或退出）——同 fetch_release，不可在此 die。
build_local() {
	command -v go >/dev/null 2>&1 || { warn "--local 需要 Go 工具鏈（未找到 go）"; return 1; }
	[ -f "$REPO_ROOT/go.mod" ] || { warn "--local 需在倉庫內執行（未找到 $REPO_ROOT/go.mod）"; return 1; }
	info "從源碼構建（CGO_ENABLED=0，靜態）"
	( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
		go build -trimpath -ldflags="-s -w" -o "$DIR/zcode2api" ./cmd/zcode2api ) || { warn "構建失敗"; return 1; }
	chmod 0755 "$DIR/zcode2api" || return 1
	return 0
}

# fetch_release 下載指定 tag 到 $DIR/zcode2api。
# 返回非 0 表示失敗（調用方決定回滾或退出）——不可在此 die，
# 否則 bin_update 的回滾與重啟邏輯永遠不會執行，服務會停在停止狀態。
fetch_release() {
	local tag="$1"
	local url="https://github.com/$REPO/releases/download/$tag/zcode2api-linux-$ARCH"
	info "下載 $url"
	if ! curl -fL --retry 3 --connect-timeout 15 --progress-bar -o "$DIR/zcode2api" "$url"; then
		warn "下載失敗: $url"
		warn "若該版本尚未發佈產物，請改用 --local 從源碼構建"
		return 1
	fi
	chmod 0755 "$DIR/zcode2api" || return 1
	return 0
}

# ── systemd ─────────────────────────────────────────────────────────────────
UNIT_PATH="${UNIT_PATH:-/etc/systemd/system/zcode2api.service}"

write_unit() {
	local dst="$UNIT_PATH"
	local src="$SELF_DIR/zcode2api.service"
	local tmp
	tmp="$(mktemp)"
	# 注意：不可用 trap ... RETURN 搭配 local（函式返回後 trap 仍可能觸發，
	# 此時 local 變數已離開作用域，set -u 下會報 unbound variable）。
	local cleanup="rm -f '$tmp'"

	if [ -f "$src" ]; then
		info "使用服務模板 $src"
		cat "$src" >"$tmp"
	else
		info "使用內建服務模板"
		cat >"$tmp" <<'UNIT_EOF'
[Unit]
Description=zcode2api - Z.AI ZCode Coding Plan 網關
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=__ZCODE_USER__
Group=__ZCODE_GROUP__
WorkingDirectory=__ZCODE_DIR__
ExecStart=__ZCODE_DIR__/zcode2api serve
EnvironmentFile=-__ZCODE_DIR__/.env
Environment=ZCODE_PORT=__ZCODE_PORT__
Environment=ZCODE_DATA_DIR=__ZCODE_DIR__/data
Environment=ZCODE_CAPTCHA_BROWSER=__ZCODE_BROWSER__
Environment=CLOAKBROWSER_CACHE_DIR=__ZCODE_DIR__/browser
Restart=on-failure
RestartSec=5s

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=__ZCODE_DIR__
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
UNIT_EOF
	fi

	sed -e "s|__ZCODE_DIR__|$DIR|g" \
		-e "s|__ZCODE_USER__|$RUN_USER|g" \
		-e "s|__ZCODE_GROUP__|$RUN_GROUP|g" \
		-e "s|__ZCODE_PORT__|$PORT|g" \
		-e "s|__ZCODE_BROWSER__|$ENABLE_BROWSER|g" \
		"$tmp" >"$dst" || die "寫入服務單元失敗: $dst（權限不足？）"

	if ! grep -q '^ExecStart=' "$dst"; then
		die "服務單元內容異常（缺少 ExecStart）: $dst"
	fi

	# 安裝在 /home 或 /root 下時，ProtectHome=read-only 會使服務無法寫入數據目錄，
	# 此時移除該加固項（保留其餘沙箱限制）。
	case "$DIR" in
		/home/*|/root/*|/root)
			sed -i '/^ProtectHome=/d' "$dst"
			warn "安裝目錄位於家目錄下，已移除 systemd 的 ProtectHome 限制以允許寫入"
			;;
	esac

	chmod 0644 "$dst"
	eval "$cleanup"
	ok "已安裝 systemd 服務 $dst"
}

write_env_file() {
	if [ -f "$DIR/.env" ]; then
		info "保留既有 $DIR/.env"
		return 0
	fi
	cat >"$DIR/.env" <<EOF
# zcode2api 環境配置（由 manage.sh 生成，可自行編輯後 systemctl restart zcode2api）
# ZCODE_HOST=127.0.0.1 時僅監聽本機迴環，適合放在反向代理之後
ZCODE_HOST=$HOST
ZCODE_PORT=$PORT
ZCODE_DATA_DIR=$DIR/data
ZCODE_CAPTCHA_BROWSER=$ENABLE_BROWSER
CLOAKBROWSER_CACHE_DIR=$DIR/browser
EOF
	chmod 0600 "$DIR/.env"
	[ "$RUN_USER" != "root" ] && chown "$RUN_USER:$RUN_GROUP" "$DIR/.env" 2>/dev/null || true
	ok "已生成 $DIR/.env"
}

resolve_run_user() {
	if [ -z "$RUN_USER" ]; then
		RUN_USER="root"
		RUN_GROUP="root"
		return 0
	fi
	if id "$RUN_USER" >/dev/null 2>&1; then
		RUN_GROUP="$(id -gn "$RUN_USER")"
	else
		info "建立系統賬號 $RUN_USER"
		useradd --system --create-home --home-dir "$DIR" --shell /usr/sbin/nologin "$RUN_USER" \
			|| die "建立賬號 $RUN_USER 失敗"
		RUN_GROUP="$(id -gn "$RUN_USER")"
	fi
	[ -n "$RUN_GROUP" ] || die "無法確定賬號 $RUN_USER 的主組"
}

# installed_port 读取实际生效的监听端口。
#
# 不能直接用脚本变量 $PORT：它只是「安装时用的端口」，而 .env 里的
# ZCODE_PORT 才是服务实际监听的（用户可事后编辑 .env 改端口）。两者不一致时
# 提示里的地址会指向一个没人监听的端口。
installed_port() {
	local env_file="$DIR/.env" p
	if [ -f "$env_file" ]; then
		p="$(sed -n 's/^ZCODE_PORT=//p' "$env_file" 2>/dev/null | tail -1)"
		[ -n "$p" ] && { printf '%s' "$p"; return 0; }
	fi
	printf '%s' "$PORT"
}

# installed_host 读取实际监听地址；通配地址对浏览器无意义，显示回环。
installed_host() {
	local env_file="$DIR/.env" h
	if [ -f "$env_file" ]; then
		h="$(sed -n 's/^ZCODE_HOST=//p' "$env_file" 2>/dev/null | tail -1)"
		case "$h" in
			""|0.0.0.0|"::") ;; # 通配，回退到外部 IP
			*) printf '%s' "$h"; return 0 ;;
		esac
	fi
	hostname -I 2>/dev/null | awk '{print $1}'
}

# bin_show_keys 打印當前密鑰。
# 獨立成命令的理由：密鑰存在數據庫裡，忘了就只能在後台看，而後台需要密鑰才
# 進得去——沒有這個出口，遺忘等於重建。直接讀庫繞開這個死結。
bin_show_keys() {
	require_root
	local db="$DIR/data/accounts.db"
	[ -f "$db" ] || die "未找到數據庫: $db（請確認安裝目錄或用 --dir 指定）"
	command -v sqlite3 >/dev/null 2>&1 || die "需要 sqlite3 讀取密鑰，請先安裝"

	local admin_key gateway_key
	admin_key="$(sqlite3 "$db" "SELECT value FROM meta WHERE key='admin_key';" 2>/dev/null || true)"
	gateway_key="$(sqlite3 "$db" "SELECT value FROM meta WHERE key='gateway_key';" 2>/dev/null || true)"

	echo
	printf '%s密鑰%s\n' "$C_BOLD" "$C_RST"
	hr
	printf '  目錄           %s\n' "$DIR"
	printf '  後台密碼       %s\n' "${admin_key:-（未設置）}"
	printf '  網關 API Key   %s\n' "${gateway_key:-（未設置）}"
	echo
	info "後台位址: http://$(installed_host):$(installed_port)/admin/login"
	echo
}

print_keys_hint() {
	local ip
	ip="$(installed_host)"
	[ -n "$ip" ] || ip="<本機IP>"
	if ! systemctl is-active --quiet zcode2api.service 2>/dev/null; then
		warn "服務未處於運行狀態，請執行: journalctl -u zcode2api -n 50"
		return 0
	fi
	ok "服務運行中: http://$ip:$(installed_port)/admin/login"

	# 直接讀庫而非 grep 日誌。原本靠 journalctl 匹配二進制橫幅的字串，那條
	# 依賴極脆：文案改一個字（簡繁差異即足夠）就永遠匹配不到，而失敗是靜默的
	# ——首次安裝的密鑰提示會退化成「已存在於數據庫」，把剛生成的密鑰藏起來。
	# 庫裡的 meta 表是唯一權威來源，不受語言、日誌輪替與時間窗口影響。
	local db="$DIR/data/accounts.db"
	if [ ! -f "$db" ] || ! command -v sqlite3 >/dev/null 2>&1; then
		echo
		info "密鑰存放於 $db（meta 表）。"
		info "遺忘可在後台「系統設置」查看，或執行: sudo $SELF_BASENAME show-keys"
		return 0
	fi

	local admin_key gateway_key
	admin_key="$(sqlite3 "$db" "SELECT value FROM meta WHERE key='admin_key';" 2>/dev/null || true)"
	gateway_key="$(sqlite3 "$db" "SELECT value FROM meta WHERE key='gateway_key';" 2>/dev/null || true)"

	if [ -z "$admin_key" ] && [ -z "$gateway_key" ]; then
		echo
		info "未能從數據庫讀取密鑰；可在後台「系統設置」查看。"
		return 0
	fi

	echo
	printf '  %s後台密碼%s      %s\n' "$C_DIM" "$C_RST" "${admin_key:-（未設置）}"
	printf '  %s網關 API Key%s   %s\n' "$C_DIM" "$C_RST" "${gateway_key:-（未設置）}"
	printf '  %s如已遺忘，可在此查看或於後台「系統設置」修改。%s\n' "$C_DIM" "$C_RST"
}

# ── 二進制：安裝 / 更新 / 卸載 / 狀態 ───────────────────────────────────────
bin_install() {
	require_root
	detect_arch
	detect_pkg_mgr
	resolve_run_user
	validate_port
	validate_host
	info "安裝目錄: $DIR    運行賬號: $RUN_USER    架構: linux/$ARCH"
	install_base_deps
	install_browser_deps

	mkdir -p "$DIR/data"
	if [ "$RUN_USER" != "root" ]; then
		if ! chown -R "$RUN_USER:$RUN_GROUP" "$DIR" 2>/dev/null; then
			warn "無法變更 $DIR 屬主爲 $RUN_USER:$RUN_GROUP，請手動確認該賬號有寫入權限"
		fi
	fi

	if [ "$SOURCE" = "local" ]; then
		build_local || die "安裝失敗：從源碼構建未成功"
	else
		local tag="$VERSION"
		if [ -z "$tag" ]; then
			info "查詢最新 Release"
			tag="$(latest_tag)"
			[ -n "$tag" ] || die "未找到任何 Release 產物。請改用 --local 從源碼構建，或用 --version 指定標籤"
		fi
		fetch_release "$tag" || die "安裝失敗：無法下載 $tag。請改用 --local 從源碼構建，或用 --version 指定其他標籤"
		printf '%s' "$tag" >"$DIR/.installed-version"
		ok "已安裝版本 $tag"
	fi

	write_env_file
	write_unit

	if [ "$PREFETCH_BROWSER" = "true" ] && [ "$ENABLE_BROWSER" = "true" ]; then
		info "預下載補丁 Chromium（約 200MB，可能需要數分鐘）"
		if ( cd "$DIR" && set -a && . "$DIR/.env" && set +a && \
			timeout 1800 "$DIR/zcode2api" prefetch-browser ); then
			ok "瀏覽器預下載完成"
		else
			warn "預下載未成功；服務首次使用時會自動重試"
		fi
	fi

	systemctl daemon-reload || die "systemctl daemon-reload 失敗（本機可能未使用 systemd）"
	systemctl enable --now zcode2api.service || die "啟動服務失敗，請查看: journalctl -u zcode2api -n 50"
	sleep 2
	echo
	ok "安裝完成"
	print_summary
	print_keys_hint
}

bin_update() {
	require_root
	detect_arch
	[ -x "$DIR/zcode2api" ] || die "未檢測到已安裝的二進制（$DIR/zcode2api）。請先執行安裝"

	local current target
	current="$(installed_version)"
	[ -n "$current" ] || current="(未知)"

	if [ "$SOURCE" = "local" ]; then
		info "當前版本: $current；從源碼重新構建"
		confirm "確認更新？" || { info "已取消"; return 0; }
		cp -f "$DIR/zcode2api" "$DIR/zcode2api.bak" 2>/dev/null || true
		systemctl stop zcode2api.service 2>/dev/null || true
		if ! build_local; then
			warn "構建失敗，回滾到原二進制"
			[ -f "$DIR/zcode2api.bak" ] && mv -f "$DIR/zcode2api.bak" "$DIR/zcode2api"
			systemctl start zcode2api.service 2>/dev/null \
				|| warn "服務重啟失敗，請手動執行: systemctl start zcode2api"
			die "更新中止（已回滾到 $current）"
		fi
		rm -f "$DIR/zcode2api.bak"
		ok "已重新構建"
	else
		# --version 優先：GitHub API 未認證限流（每小時 60 次，共享出口 IP 極易撞上）
		# 時 latest_tag 會返回空並導致更新中止，顯式指定標籤可繞開該查詢。
		target="$VERSION"
		if [ -n "$target" ]; then
			info "當前版本: $current；目標版本（--version 指定）: $target"
		else
			info "當前版本: $current；查詢最新 Release"
			target="$(latest_tag)"
			[ -n "$target" ] || die "無法獲取最新版本（網絡問題或 GitHub API 限流）。請用 --version TAG 指定標籤"
		fi

		if [ "$current" = "$target" ]; then
			ok "已是最新版本 $current"
			confirm "仍要重新下載並覆蓋安裝？" || return 0
		else
			info "可更新: $current → $target"
			confirm "確認更新到 $target？" || { info "已取消"; return 0; }
		fi

		# 備份當前二進制，下載失敗可回滾
		cp -f "$DIR/zcode2api" "$DIR/zcode2api.bak" 2>/dev/null || true
		systemctl stop zcode2api.service 2>/dev/null || true

		if ! fetch_release "$target"; then
			warn "更新失敗，回滾到原二進制"
			if [ -f "$DIR/zcode2api.bak" ]; then
				mv -f "$DIR/zcode2api.bak" "$DIR/zcode2api" || warn "回滾失敗，請檢查 $DIR/zcode2api"
			fi
			systemctl start zcode2api.service 2>/dev/null \
				|| warn "服務重啟失敗，請手動執行: systemctl start zcode2api"
			die "更新中止（已回滾到 $current）"
		fi
		printf '%s' "$target" >"$DIR/.installed-version"
		rm -f "$DIR/zcode2api.bak"
	fi

	systemctl daemon-reload 2>/dev/null || true
	systemctl start zcode2api.service || die "啟動失敗，請查看: journalctl -u zcode2api -n 50"
	sleep 2
	echo
	ok "更新完成（$current → $(installed_version)）"
	print_keys_hint
}

bin_uninstall() {
	require_root
	local unit="$UNIT_PATH"
	local run_user=""
	[ -f "$unit" ] && run_user="$(sed -n 's/^User=//p' "$unit" | head -1)"

	if [ "$PURGE" != "true" ]; then
		if ! confirm "將移除服務與二進制，數據目錄 $DIR/data 會保留。繼續？"; then
			info "已取消"; return 0
		fi
	fi

	if [ -f "$unit" ]; then
		info "停止並禁用服務"
		systemctl disable --now zcode2api.service 2>/dev/null || warn "服務未在運行或取消失敗"
		rm -f "$unit"
		systemctl daemon-reload 2>/dev/null || true
		ok "已移除 $unit"
	else
		info "未找到 $unit，跳過服務移除"
	fi

	if [ "$PURGE" = "true" ]; then
		if [ -d "$DIR" ]; then
			warn "刪除 $DIR（含賬號憑證與數據庫）"
			rm -rf "$DIR"
			ok "已刪除 $DIR"
		fi
	else
		info "保留數據目錄 $DIR/data（--purge 可一併刪除）"
		for f in zcode2api zcode2api.bak .installed-version .env; do
			[ -e "$DIR/$f" ] && rm -f "$DIR/$f"
		done
		[ -d "$DIR/browser" ] && info "保留瀏覽器緩存 $DIR/browser"
		ok "已移除二進制與配置"
	fi

	if [ -n "$run_user" ] && [ "$run_user" != "root" ] && [ "$KEEP_USER" != "true" ]; then
		if id "$run_user" >/dev/null 2>&1; then
			info "刪除系統賬號 $run_user"
			userdel "$run_user" 2>/dev/null || warn "刪除賬號 $run_user 失敗，請手動處理"
		fi
	fi
	echo
	ok "卸載完成"
}

print_summary() {
	cat <<EOF

  服務狀態   systemctl status zcode2api
  實時日誌   journalctl -u zcode2api -f
  配置文件   $DIR/.env
  數據目錄   $DIR/data
  管理腳本   sudo $SELF_DIR/manage.sh

EOF
}

# probe_binary_version 從二進制中提取版本串（AppVersion 常量，形如 2.0.3-go）。
# 手工編譯或剝離符號的產物沒有 .installed-version，只能從內容反推；
# 用 grep -a 而非 strings：前者所有 Linux 發行版都有，後者常在最小化系統裡缺失。
probe_binary_version() {
	local bin="$1"
	[ -f "$bin" ] || return 0
	# || true：未匹配到版本串時 grep 返回 1，pipefail 會將其視爲失敗並終止調用方。
	grep -aoE '[0-9]+\.[0-9]+\.[0-9]+-go' "$bin" 2>/dev/null | sort -u | head -1 || true
}

# probe_binary_service 找出哪個 systemd 單元在運行給定的二進制。
probe_binary_service() {
	local bin="$1" unit exec_line
	command -v systemctl >/dev/null 2>&1 || return 0
	# 命令替換放進 for 的 word list 時退出碼會被忽略，但 WSL 等無 systemd 環境下
	# 仍可能因空列表產生意外行爲，故顯式容錯。
	for unit in $(systemctl list-units --type=service --state=running --no-legend --plain 2>/dev/null | awk '{print $1}' || true); do
		exec_line="$(systemctl show -p ExecStart --value "$unit" 2>/dev/null || true)"
		case "$exec_line" in
			*"$bin"*) printf '%s' "$unit"; return 0 ;;
		esac
	done
}

# probe_running_pid 找出直接以該路徑運行的進程（無 systemd 的手工部署）。
probe_running_pid() {
	local bin="$1"
	command -v pgrep >/dev/null 2>&1 || return 0
	# 同理：進程不在跑時 pgrep 返回 1，不能讓它冒泡成腳本錯誤。
	pgrep -f "^${bin}( |$)" 2>/dev/null | head -1 || true
}

# scan_report_one 輸出單個候選項的詳情。
# 返回 0 表示這是個可接管的部署目錄（含可執行文件），非 0 表示只是散落文件。
scan_report_one() {
	local bin="$1" dir ver size running svc pid mark

	dir="$(dirname "$bin")"
	ver="$(probe_binary_version "$bin")"
	size="$(stat -c '%s' "$bin" 2>/dev/null || echo 0)"
	svc="$(probe_binary_service "$bin")"
	pid="$(probe_running_pid "$bin")"

	if [ -n "$svc" ]; then
		running="運行中（$svc）"
	elif [ -n "$pid" ]; then
		running="運行中（PID $pid，無 systemd 單元）"
	else
		running="未運行"
	fi

	mark=" "
	[ "$dir" = "$DIR" ] && mark="*"

	printf '  %s %s\n' "$mark" "$bin"
	printf '      版本       %s\n' "${ver:-(未知，無 .installed-version)}"
	printf '      大小       %s\n' "$(human_size "$size")"
	printf '      狀態       %s\n' "$running"

	# 關聯資產：決定這是不是一個完整部署，以及能否安全接管
	local assets="" a
	for a in .env data browser .installed-version; do
		[ -e "$dir/$a" ] && assets="$assets $a"
	done
	[ -n "$assets" ] && printf '      關聯       %s\n' "$assets"

	if [ -x "$dir/zcode2api" ] && [ "$dir" != "$DIR" ]; then
		printf '      %s可接管%s    sudo %s adopt --dir %s\n' "$C_DIM" "$C_RST" "$SELF_BASENAME" "$dir"
	fi
	echo
	return 0
}

# bin_scan 在 /opt 下尋找本程序的既有安裝。
# 存在的理由：手工編譯部署的實例沒有 .installed-version，也沒有 systemd 單元，
# 管理腳本的固定 $DIR 看不見它，導致 update/status 報「未安裝」。
bin_scan() {
	require_root
	local found=0 bin

	echo
	printf '%s掃描既有安裝%s\n' "$C_BOLD" "$C_RST"
	hr
	info "掃描路徑: $DIR 及 $SCAN_ROOT 下的同名可執行文件"

	# 先查標準位置：這是腳本自己的安裝目錄
	if [ -x "$DIR/zcode2api" ]; then
		found=1
		scan_report_one "$DIR/zcode2api"
	fi

	# 再遞歸找散落的同名可執行文件。
	# 只認 basename 恰爲 zcode2api 的文件：zcode2api.bak / .old 這類是備份，
	# 與主程序同目錄，逐個詳報只會把同一部署刷屏。
	# -x 過濾可執行位，避免把 data/ 裡的緩存文件也報出來。
	local extras_file
	extras_file="$(mktemp)"
	while IFS= read -r bin; do
		[ -n "$bin" ] || continue
		[ "$bin" = "$DIR/zcode2api" ] && continue
		[ -x "$bin" ] || continue
		if [ "$(basename "$bin")" = "zcode2api" ]; then
			found=1
			scan_report_one "$bin"
		else
			printf '%s\n' "$bin" >>"$extras_file"
		fi
	done < <(find "$SCAN_ROOT" -maxdepth 4 -type f -name 'zcode2api*' 2>/dev/null | sort)

	if [ -s "$extras_file" ]; then
		printf '%s  同目錄下的其他文件（備份等，未詳列）:%s\n' "$C_DIM" "$C_RST"
		sed 's/^/    /' "$extras_file"
		echo
	fi
	rm -f "$extras_file"

	if [ "$found" -eq 0 ]; then
		warn "未找到本程序的任何安裝"
		echo
		info "可執行文件可能不在 $SCAN_ROOT，或名稱不同。手動確認:"
		printf '  find / -name "zcode2api*" -type f -executable 2>/dev/null\n'
		echo
		return 0
	fi

	printf '%s* = 管理腳本當前使用的目錄%s\n' "$C_DIM" "$C_RST"
	echo
	info "接管一個散落的部署（寫入 .installed-version、建 systemd 單元）:"
	printf '  sudo %s adopt --dir <目錄>\n' "$SELF_BASENAME"
	echo
}

# count_accounts 讀取某個數據目錄下的賬號總數，用於遷移前後校驗。
# 直接查 SQLite 而不是調用二進制的 accounts 子命令：後者需要啟動整套配置
# （密鑰、驗證碼瀏覽器等），而校驗只關心行數。
count_accounts() {
	local data_dir="$1" db="$1/accounts.db"
	[ -f "$db" ] || { printf '0'; return 0; }
	command -v sqlite3 >/dev/null 2>&1 || return 0
	sqlite3 "$db" 'SELECT COUNT(*) FROM accounts;' 2>/dev/null || printf '0'
}

# sqlite_backup 用 SQLite 自己的備份機制導出一致快照。
# 不能只 cp accounts.db：庫以 WAL 模式運行，最近的寫入還在 accounts.db-wal 裡，
# 只拷主文件會靜默丟掉最新數據。
snapshot_db() {
	local src_dir="$1" dst_file="$2" src_db="$1/accounts.db"
	[ -f "$src_db" ] || return 1
	if command -v sqlite3 >/dev/null 2>&1; then
		sqlite3 "$src_db" ".backup '$dst_file'" 2>/dev/null && return 0
	fi
	# 無 sqlite3 時退化爲整目錄拷貝（含 -wal/-shm），由調用方確保服務已停。
	cp -f "$src_db" "$dst_file" || return 1
	[ -f "$src_db-wal" ] && cp -f "$src_db-wal" "$dst_file-wal" 2>/dev/null
	[ -f "$src_db-shm" ] && cp -f "$src_db-shm" "$dst_file-shm" 2>/dev/null
	return 0
}

# migrate_pick_target 決定遷移目標目錄。
# 目標始終是標準目錄 $DIR：遷移的意義就是把散落部署收攏到標準位置。
# 若源本身就是 $DIR，則原地升級（無處分可搬）。
# 若 $DIR 已被別的部署佔用，加後綴——兩個不同實例不該被合併到同一目錄。
migrate_pick_target() {
	local src="$1"
	if [ "$src" = "$DIR" ]; then
		printf '%s' "$DIR"
		return 0
	fi
	if [ ! -e "$DIR/zcode2api" ]; then
		printf '%s' "$DIR"
		return 0
	fi
	printf '%s' "${DIR}-migrated"
}

# menu_migrate 交互式遷移：掃描候選 → 讓用戶選一個 → 走 bin_migrate。
# 菜單裡不能要求用戶先手打路徑，否則等於把掃描結果白報了。
menu_migrate() {
	require_root

	local -a cands=()
	local bin
	# 候選來自 scan 的同一判據：basename 恰爲 zcode2api 且可執行。
	# 標準目錄也算候選：手工放進去的部署同樣缺元數據，需要納管或升級，
	# 只是處理方式不同（就地而非搬遷），故在下方標註出來。
	while IFS= read -r bin; do
		[ -n "$bin" ] || continue
		[ -x "$bin" ] || continue
		[ "$(basename "$bin")" = "zcode2api" ] || continue
		cands+=("$(dirname "$bin")")
	done < <(find "$SCAN_ROOT" -maxdepth 4 -type f -name 'zcode2api' 2>/dev/null | sort)

	echo
	printf '%s遷移既有部署%s\n' "$C_BOLD" "$C_RST"
	hr

	if [ "${#cands[@]}" -eq 0 ]; then
		warn "未在 $SCAN_ROOT 下找到本程序的部署"
		info "若部署在別處，用命令行指定: sudo $SELF_BASENAME migrate --dir <目錄>"
		echo
		return 0
	fi

	local i
	for i in "${!cands[@]}"; do
		bin="${cands[$i]}/zcode2api"
		local note=""
		if [ "${cands[$i]}" = "$DIR" ]; then
			note="  （標準目錄，就地納管並升級）"
		fi
		printf '  %s%d)%s %s%s\n' "$C_CYAN" "$((i + 1))" "$C_RST" "${cands[$i]}" "$note"
		printf '       版本 %s   %s\n' \
			"$(probe_binary_version "$bin" || echo '?')" \
			"$(probe_binary_service "$bin" || echo '未運行')"
	done
	printf '  %s0)%s 取消\n' "$C_CYAN" "$C_RST"
	echo

	local pick
	printf '請選擇要遷移的部署: '
	read -r pick || return 0
	case "$pick" in
		0|"") info "已取消"; return 0 ;;
	esac
	if ! [[ "$pick" =~ ^[0-9]+$ ]] || [ "$pick" -lt 1 ] || [ "$pick" -gt "${#cands[@]}" ]; then
		warn "無效選擇: $pick"
		return 0
	fi

	ADOPT_DIR="${cands[$((pick - 1))]}"
	bin_migrate
}

# bin_migrate 把既有部署遷到標準目錄並升級到最新 Release。
# 與 adopt 的區別：adopt 只補管理元數據、不動二進制；migrate 會真正部署新版本，
# 因此每一步都可回滾，且舊目錄預設保留。
bin_migrate() {
	require_root
	detect_arch

	local src="${ADOPT_DIR:-}"
	[ -n "$src" ] || die "需要 --dir 指定源部署目錄，例如: sudo $SELF_BASENAME migrate --dir /opt/zcode2api-custom"
	src="$(cd "$src" 2>/dev/null && pwd)" || die "目錄不存在: ${ADOPT_DIR}"
	[ -f "$src/zcode2api" ] || die "$src 下沒有 zcode2api 可執行文件"

	local target
	target="$(migrate_pick_target "$src")"
	local target_is_src="false"
	[ "$target" = "$src" ] && target_is_src="true"

	echo
	printf '%s遷移既有部署%s\n' "$C_BOLD" "$C_RST"
	hr
	printf '  源目錄     %s\n' "$src"
	printf '  目標目錄   %s\n' "$target"
	printf '  源版本     %s\n' "$(probe_binary_version "$src/zcode2api" || echo '(未知)')"
	printf '  運行狀態   %s\n' "$(probe_binary_service "$src/zcode2api" || echo '未運行')"
	echo

	# 目標版本：優先 --version，否則查最新 Release（可能因 API 限流失敗）
	local tag="$VERSION"
	if [ -z "$tag" ]; then
		info "查詢最新 Release"
		tag="$(latest_tag)"
		if [ -z "$tag" ]; then
			die "無法獲取最新版本（網絡問題或 GitHub API 限流）。請用 --version TAG 指定標籤"
		fi
	fi
	printf '  目標版本   %s\n' "$tag"
	echo

	confirm "確認遷移？" || { info "已取消"; return 0; }

	# 1) 停止舊服務。必須在拷貝數據前停：WAL 模式下運行中的寫入會讓快照不一致。
	local svc
	svc="$(probe_binary_service "$src/zcode2api" || true)"
	local stopped_unit=""
	if [ -n "$svc" ]; then
		info "停止服務 $svc"
		systemctl stop "$svc" 2>/dev/null || true
		stopped_unit="$svc"
	fi
	# 也可能有無 systemd 的裸進程
	local pid
	pid="$(probe_running_pid "$src/zcode2api" || true)"
	if [ -n "$pid" ]; then
		warn "發現直接運行的進程 PID $pid，正在停止"
		kill "$pid" 2>/dev/null || true
		sleep 2
		kill -9 "$pid" 2>/dev/null || true
	fi

	# 2) 記錄源數據基線，供遷移後校驗
	local src_accounts=""
	if [ -f "$src/data/accounts.db" ]; then
		src_accounts="$(count_accounts "$src/data")"
		info "源數據賬號數: ${src_accounts:-未知}"
	fi

	# 3) 準備目標目錄
	if [ "$target_is_src" = "false" ]; then
		mkdir -p "$target/data" || die "無法創建 $target"
	fi

	# 4) 部署新二進制。先備份，失敗可回滾。
	local bak="$target/zcode2api.pre-migrate"
	cp -f "$target/zcode2api" "$bak" 2>/dev/null || true
	local dl_dir="$target"
	if [ "$target_is_src" = "false" ]; then
		info "下載 $tag 到 $target"
	else
		info "下載 $tag（原地升級）"
	fi
	local url="https://github.com/$REPO/releases/download/$tag/zcode2api-linux-$ARCH"
	if ! curl -fL --retry 3 --connect-timeout 15 --progress-bar -o "$target/zcode2api.new" "$url"; then
		warn "下載失敗: $url"
		[ -f "$bak" ] && mv -f "$bak" "$target/zcode2api"
		warn "遷移中止（二進制已回滾）"
		[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
		die "下載 $tag 失敗"
	fi
	chmod 0755 "$target/zcode2api.new" || die "設置可執行權限失敗"

	# 5) 遷移數據。目標已存在同名庫時先留副本，避免覆蓋掉目標自己的數據。
	if [ "$target_is_src" = "false" ] && [ -d "$src/data" ]; then
		if [ -f "$target/data/accounts.db" ]; then
			warn "$target/data 已有 accounts.db，保留爲 accounts.db.pre-migrate"
			mv -f "$target/data/accounts.db" "$target/data/accounts.db.pre-migrate" 2>/dev/null || true
		fi
		info "複製數據 $src/data → $target/data"
		if ! cp -a "$src/data/." "$target/data/" 2>/dev/null; then
			warn "數據複製失敗"
			rm -f "$target/zcode2api.new"
			[ -f "$bak" ] && mv -f "$bak" "$target/zcode2api"
			[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
			die "遷移中止（二進制已回滾，數據未改動）"
		fi
	fi

	# 6) 校驗賬號數。數量不符說明快照不完整，寧可停下讓人檢查。
	if [ -n "$src_accounts" ]; then
		local dst_accounts
		dst_accounts="$(count_accounts "$target/data")"
		if [ "$dst_accounts" != "$src_accounts" ]; then
			warn "賬號數不一致: 源 $src_accounts → 目標 $dst_accounts"
			warn "數據可能未完整遷移，已保留新二進制於 $target/zcode2api.new 供檢查"
			[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
			die "遷移校驗失敗，未切換"
		fi
		ok "賬號數校驗通過: $dst_accounts"
	fi

	# 7) 切換二進制
	mv -f "$target/zcode2api.new" "$target/zcode2api" || die "替換二進制失敗"
	ok "已部署 $tag"

	# 8) 遷移 .env（保留源配置：端口、密鑰等都在裡面）
	if [ "$target_is_src" = "false" ] && [ -f "$src/.env" ]; then
		if [ -f "$target/.env" ]; then
			cp -f "$target/.env" "$target/.env.pre-migrate" 2>/dev/null || true
		fi
		cp -f "$src/.env" "$target/.env" && chmod 0600 "$target/.env"
		info "已遷移 .env"
	fi

	printf '%s' "$tag" >"$target/.installed-version"

	# 9) 寫單元並啟動。DIR 臨時指向 target 以複用單元模板。
	local old_dir="$DIR"
	DIR="$target"
	write_unit || { DIR="$old_dir"; die "寫入 systemd 單元失敗"; }
	DIR="$old_dir"

	if [ -n "$stopped_unit" ] && [ "$stopped_unit" != "zcode2api.service" ]; then
		systemctl disable "$stopped_unit" 2>/dev/null || true
		info "已停用舊單元 $stopped_unit"
	fi
	systemctl daemon-reload 2>/dev/null || true
	systemctl restart zcode2api.service 2>/dev/null \
		|| systemctl start zcode2api.service 2>/dev/null \
		|| warn "服務啟動失敗，請手動執行: systemctl start zcode2api"
	ok "服務已啟動"

	# 10) 清理舊目錄。僅在數據已確認遷移且目標不是源目錄時進行。
	if [ "$target_is_src" = "false" ]; then
		echo
		if confirm "刪除舊目錄 $src？（數據已遷移到 $target）"; then
			rm -rf "$src" && ok "已刪除 $src"
		else
			info "保留 $src"
			warn "舊目錄仍在，兩個目錄都含 data/；確認新部署正常後可手動刪除"
		fi
	fi

	echo
	ok "遷移完成（$tag）"
	printf '  目錄   %s\n' "$target"
	printf '  管理   sudo %s status --dir %s\n' "$SELF_BASENAME" "$target"
	echo
}

# bin_adopt 把一個手工部署的目錄納入管理。
# 只補管理所需的元數據與單元，不動二進制本身——避免覆蓋用戶自行編譯的產物。
bin_adopt() {
	require_root
	detect_arch

	local src="${ADOPT_DIR:-}"
	[ -n "$src" ] || die "需要 --dir 指定部署目錄，例如: sudo $SELF_BASENAME adopt --dir /opt/zcode2api-custom"
	[ -d "$src" ] || die "目錄不存在: $src"

	local bin="$src/zcode2api"
	[ -f "$bin" ] || die "$src 下沒有 zcode2api 可執行文件"
	[ -x "$bin" ] || die "$bin 沒有可執行權限，請先 chmod +x"
	src="$(cd "$src" && pwd)"

	# 已在標準目錄的部署同樣需要納管：手工放進去的二進制沒有 .installed-version，
	# 也沒有 systemd 單元，status 會顯示版本未知、update 無從比對。
	# 故此處不再早退，只是無需搬遷。
	local in_place="false"
	[ "$src" = "$DIR" ] && in_place="true"

	local ver
	ver="$(probe_binary_version "$bin")"
	if [ "$in_place" = "true" ]; then
		info "接管目錄: $src（已在標準目錄，就地納管）"
	else
		info "接管目錄: $src"
	fi
	[ -n "$ver" ] && info "檢測到版本: $ver" || warn "未能從二進制提取版本，接管後 update 將無法比對版本"

	confirm "確認接管 $src？" || { info "已取消"; return 0; }

	# 單元裡的路徑與用戶需要指向新目錄：臨時切換 DIR 後複用現成的寫入邏輯，
	# 而不是把單元模板再抄一份（抄一份就會有兩處需要同步維護）。
	# 既有單元若與我們寫入的不同名，接管後兩個單元會同時拉起同一二進制、
	# 搶同一端口（後啟動者 bind 失敗）。故接管前先停用舊單元，而不是並存。
	local svc="$(probe_binary_service "$bin")"
	local old_unit=""
	if [ -n "$svc" ] && [ "/etc/systemd/system/$svc" != "$UNIT_PATH" ]; then
		old_unit="$svc"
		warn "檢測到 $svc 正在運行該二進制"
		info "接管後將停用 $svc 並改用 $UNIT_PATH，避免兩個單元爭搶端口"
	elif [ -n "$svc" ]; then
		info "檢測到 $svc 正在運行該二進制，將就地重啟"
	fi

	local old_dir="$DIR"
	DIR="$src"
	write_unit || { DIR="$old_dir"; die "寫入 systemd 單元失敗"; }
	DIR="$old_dir"

	# .installed-version 讓 update/status 能識別版本；缺失時 update 會當作未知版本。
	# 手工編譯的產物無法確定對應哪個 Release，故用二進制內嵌版本號而非猜一個 tag。
	if [ -n "$ver" ]; then
		printf 'v%s' "$ver" >"$src/.installed-version"
		info "已寫入 $src/.installed-version = v$ver"
	else
		warn "跳過 .installed-version（版本未知），update 將提示無法比對"
	fi

	if [ -n "$old_unit" ]; then
		systemctl stop "$old_unit" 2>/dev/null || true
		systemctl disable "$old_unit" 2>/dev/null || true
		info "已停用舊單元 $old_unit"
	fi

	systemctl daemon-reload 2>/dev/null || true
	if confirm "現在啟動服務？"; then
		systemctl restart zcode2api.service 2>/dev/null \
			|| systemctl start zcode2api.service 2>/dev/null \
			|| warn "服務啟動失敗，請手動執行: systemctl start zcode2api"
		ok "服務已啟動"
	fi

	echo
	if [ "$in_place" = "true" ]; then
		ok "已納管，後續可直接執行: sudo $SELF_BASENAME update"
	else
		warn "注意：管理腳本的預設目錄仍是 $old_dir"
		info "後續 update/status 需指定目錄: sudo $SELF_BASENAME update --dir $src"
	fi
	echo
}

bin_status() {
	echo
	printf '%s二進制安裝%s\n' "$C_BOLD" "$C_RST"
	hr
	if [ -x "$DIR/zcode2api" ]; then
		printf '  目錄       %s\n' "$DIR"
		printf '  版本       %s\n' "$(installed_version | grep . || echo '(未知)')"
		local bin_info
		bin_info="$(stat -c '%s 字節  %y' "$DIR/zcode2api" 2>/dev/null | cut -d. -f1)"
		printf '  二進制     %s\n' "${bin_info:-(未知)}"
	else
		printf '  %s未安裝%s（目錄: %s）\n' "$C_DIM" "$C_RST" "$DIR"
	fi

	if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT_PATH" ]; then
		local state
		state="$(systemctl is-active zcode2api.service 2>/dev/null || true)"
		printf '  服務       %s\n' "${state:-unknown}"
		printf '  開機自啟   %s\n' "$(systemctl is-enabled zcode2api.service 2>/dev/null || echo '-')"
	fi
	if [ -f "$DIR/.env" ]; then
		printf '  監聽       %s\n' "$(grep -E '^ZCODE_(HOST|PORT)=' "$DIR/.env" 2>/dev/null | tr '\n' ' ' || echo '-')"
	fi
	if [ -d "$DIR/data" ]; then
		printf '  數據       %s\n' "$(du -sh "$DIR/data" 2>/dev/null | cut -f1 || echo '-')"
	fi
	if [ -d "$DIR/browser" ]; then
		printf '  瀏覽器緩存 %s\n' "$(du -sh "$DIR/browser" 2>/dev/null | cut -f1 || echo '-')"
	fi
	echo
	printf '%sDocker 部署%s\n' "$C_BOLD" "$C_RST"
	hr
	if command -v docker >/dev/null 2>&1; then
		printf '  Docker     %s\n' "$(docker --version 2>/dev/null | head -1)"
		if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx zcode2api; then
			printf '  容器       %s\n' "$(docker ps -a --filter 'name=^/zcode2api$' --format '{{.Status}}' 2>/dev/null)"
		else
			printf '  容器       %s未創建%s\n' "$C_DIM" "$C_RST"
		fi
		if [ -d "$DOCKER_DIR/.git" ]; then
			printf '  構建目錄   %s\n' "$DOCKER_DIR"
		fi
	else
		printf '  %s未安裝 Docker%s\n' "$C_DIM" "$C_RST"
	fi
	echo
}

# ── Docker：安裝 / 更新 / 卸載 ──────────────────────────────────────────────
# 說明：Docker 路徑爲參考實現，未經驗證。腳本會明確提示並要求確認。
docker_preflight() {
	require_root
	command -v docker >/dev/null 2>&1 || die "未找到 docker。請先安裝 Docker Engine"
	if docker compose version >/dev/null 2>&1; then
		DC=(docker compose)
	elif command -v docker-compose >/dev/null 2>&1; then
		DC=(docker-compose)
	else
		die "未找到 docker compose（v2 插件或 v1 獨立版）"
	fi
}

docker_ensure_source() {
	# 優先用當前倉庫；否則克隆到 DOCKER_DIR
	if [ -f "$REPO_ROOT/Dockerfile" ] && [ -f "$REPO_ROOT/docker-compose.yml" ]; then
		DOCKER_SRC="$REPO_ROOT"
		info "使用當前倉庫: $DOCKER_SRC"
		return 0
	fi
	DOCKER_SRC="$DOCKER_DIR"
	if [ -d "$DOCKER_SRC/.git" ]; then
		info "更新已有克隆 $DOCKER_SRC"
		git -C "$DOCKER_SRC" pull --ff-only || warn "git pull 失敗，沿用現有代碼"
	else
		command -v git >/dev/null 2>&1 || die "需要 git 以克隆倉庫（或在倉庫內運行本腳本）"
		info "克隆 $REPO_URL 到 $DOCKER_SRC"
		mkdir -p "$(dirname "$DOCKER_SRC")"
		git clone --depth 1 "$REPO_URL" "$DOCKER_SRC" || die "克隆失敗"
	fi

	# 克隆/pull 後必須確認構建上下文完整，否則 compose 會在缺失文件上失敗
	[ -f "$DOCKER_SRC/docker-compose.yml" ] \
		|| die "構建上下文缺少 docker-compose.yml: $DOCKER_SRC"
	[ -f "$DOCKER_SRC/Dockerfile" ] \
		|| die "構建上下文缺少 Dockerfile: $DOCKER_SRC"
}

docker_warn_unverified() {
	echo
	warn "注意：Docker 方案爲參考實現，從未經 docker build 驗證，不保證可用。"
	warn "若構建或運行失敗，建議改用二進制方案：sudo $SELF_DIR/manage.sh install"
	echo
}

docker_install() {
	docker_preflight
	docker_warn_unverified
	confirm "確認繼續 Docker 安裝？" || { info "已取消"; return 0; }
	docker_ensure_source
	info "構建並啟動容器（首次構建可能較久）"
	( cd "$DOCKER_SRC" && "${DC[@]}" up -d --build ) || die "docker compose up 失敗"
	echo
	ok "Docker 部署完成"
	info "查看日誌獲取後台密碼與網關密鑰: cd $DOCKER_SRC && ${DC[*]} logs -f"
}

docker_update() {
	docker_preflight
	docker_ensure_source
	confirm "將重新構建鏡像並重啟容器（數據卷保留）。繼續？" || { info "已取消"; return 0; }
	info "重新構建並啟動"
	( cd "$DOCKER_SRC" && "${DC[@]}" up -d --build --force-recreate ) || die "更新失敗"
	ok "Docker 更新完成"
}

docker_uninstall() {
	docker_preflight
	# 卸載不應強制克隆倉庫：優先用當前倉庫或已有克隆，找不到則用 docker 直連操作。
	DOCKER_SRC=""
	if [ -f "$REPO_ROOT/docker-compose.yml" ]; then
		DOCKER_SRC="$REPO_ROOT"
	elif [ -f "$DOCKER_DIR/docker-compose.yml" ]; then
		DOCKER_SRC="$DOCKER_DIR"
	fi

	local extra=""
	if [ "$DOCKER_VOLUMES" = "true" ]; then
		extra="-v"
		warn "將同時刪除數據卷（賬號庫與瀏覽器緩存）"
	else
		info "數據卷會保留（--volumes 可一併刪除）"
	fi
	confirm "確認停止並移除容器？" || { info "已取消"; return 0; }

	if [ -n "$DOCKER_SRC" ]; then
		( cd "$DOCKER_SRC" && "${DC[@]}" down $extra ) || die "docker compose down 失敗"
	else
		# 找不到 compose 文件：直接操作容器與卷
		info "未找到 compose 文件，直接移除容器"
		docker rm -f zcode2api >/dev/null 2>&1 || warn "容器 zcode2api 不存在或已移除"
		if [ "$DOCKER_VOLUMES" = "true" ]; then
			for v in zcode2api-data zcode2api-browser; do
				docker volume rm "$v" >/dev/null 2>&1 || true
			done
		fi
	fi
	ok "Docker 卸載完成"
	[ -n "$extra" ] && info "數據卷已刪除" || info "數據卷已保留，可用 docker volume ls 查看"
}

# ── 交互菜單 ────────────────────────────────────────────────────────────────
menu() {
	require_linux
	while true; do
		echo
		printf '%s╔══════════════════════════════════════════════════════════╗%s\n' "$C_BOLD" "$C_RST"
		printf '%s║        zcode2api 管理腳本（Linux）                        ║%s\n' "$C_BOLD" "$C_RST"
		printf '%s╚══════════════════════════════════════════════════════════╝%s\n' "$C_BOLD" "$C_RST"

		local bin_state="未安裝"
		[ -x "$DIR/zcode2api" ] && bin_state="已安裝 $(installed_version | grep . || echo '')"
		local svc_state=""
		if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT_PATH" ]; then
			svc_state=" [$(systemctl is-active zcode2api.service 2>/dev/null || echo unknown)]"
		fi
		local dk_state="Docker 未安裝"
		command -v docker >/dev/null 2>&1 && dk_state="Docker 已安裝"

		printf '  二進制: %s%s\n' "$bin_state" "$svc_state"
		printf '  %s\n' "$dk_state"
		echo
		printf '  %s1)%s 安裝二進制          %s5)%s Docker 安裝（未驗證）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s2)%s 更新二進制          %s6)%s Docker 更新\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s3)%s 卸載二進制          %s7)%s Docker 卸載\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s4)%s 查看狀態            %s8)%s 服務控制（啟停/重啟/日誌）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s9)%s 掃描已有安裝        %s10)%s 遷移到標準目錄\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s0)%s 退出\n' "$C_CYAN" "$C_RST"
		echo
		local choice
		printf '請選擇 [0-10]: '
		read -r choice || { echo; break; }
		case "$choice" in
			1) menu_install ;;
			2) bin_update ;;
			3) menu_uninstall ;;
			4) bin_status ;;
			5) docker_install ;;
			6) docker_update ;;
			7) menu_docker_uninstall ;;
			8) menu_service ;;
			9) bin_scan ;;
			10) menu_migrate ;;
			0|"") break ;;
			*) warn "無效選擇: $choice" ;;
		esac
		echo
		printf '按回車返回菜單...'
		read -r _ || break
	done
}

menu_install() {
	require_root
	echo
	printf '安裝方式:  %s1)%s 從 Releases 下載    %s2)%s 從源碼構建（需 Go）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
	local mode
	printf '請選擇 [1]: '
	read -r mode || return 0
	case "$mode" in
		2) SOURCE="local" ;;
		*) SOURCE="release" ;;
	esac

	printf '監聽端口 [%s]: ' "$PORT"
	local p; read -r p || true
	[ -n "$p" ] && PORT="$p"
	validate_port

	printf '監聽地址 [%s]（反向代理後填 127.0.0.1）: ' "$HOST"
	local h; read -r h || true
	[ -n "$h" ] && HOST="$h"
	validate_host

	printf '運行賬號 [root]: '
	local u; read -r u || true
	[ -n "$u" ] && RUN_USER="$u"

	if confirm "啟用驗證碼瀏覽器自動求解（會安裝瀏覽器依賴）？"; then
		ENABLE_BROWSER="true"
	else
		ENABLE_BROWSER="false"
	fi
	if [ "$ENABLE_BROWSER" = "true" ]; then
		# 預設預下載：否則新機首次 JWT 請求需等待下載（約 200MB），
		# 期間該請求會因驗證碼不可用而失敗。
		confirm "現在預下載補丁 Chromium（約 200MB）？" || PREFETCH_BROWSER="false"
	fi
	echo
	bin_install
}

menu_uninstall() {
	require_root
	echo
	printf '  %s1)%s 僅移除服務與二進制（保留數據）\n' "$C_CYAN" "$C_RST"
	printf '  %s2)%s 全部刪除（含賬號庫與瀏覽器緩存）\n' "$C_CYAN" "$C_RST"
	printf '  %s0)%s 返回\n' "$C_CYAN" "$C_RST"
	local c
	printf '請選擇 [0]: '
	read -r c || return 0
	case "$c" in
		1) PURGE="false"; bin_uninstall ;;
		2)
			PURGE="true"
			warn "此操作將永久刪除 $DIR（含賬號憑證）"
			confirm "確認全部刪除？" && bin_uninstall || info "已取消"
			;;
		*) info "已取消" ;;
	esac
}

menu_docker_uninstall() {
	docker_preflight
	echo
	printf '  %s1)%s 移除容器（保留數據卷）\n' "$C_CYAN" "$C_RST"
	printf '  %s2)%s 移除容器並刪除數據卷\n' "$C_CYAN" "$C_RST"
	printf '  %s0)%s 返回\n' "$C_CYAN" "$C_RST"
	local c
	printf '請選擇 [0]: '
	read -r c || return 0
	case "$c" in
		1) DOCKER_VOLUMES="false"; docker_uninstall ;;
		2) DOCKER_VOLUMES="true"; docker_uninstall ;;
		*) info "已取消" ;;
	esac
}

menu_service() {
	require_root
	echo
	printf '  %s1)%s 啟動    %s2)%s 停止    %s3)%s 重啟    %s4)%s 實時日誌\n' \
		"$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
	local c
	printf '請選擇 [0]: '
	read -r c || return 0
	case "$c" in
		1) systemctl start zcode2api.service && ok "已啟動" ;;
		2) systemctl stop zcode2api.service && ok "已停止" ;;
		3) systemctl restart zcode2api.service && ok "已重啟" ;;
		4) info "按 Ctrl+C 退出日誌"; journalctl -u zcode2api -f ;;
		*) info "已取消" ;;
	esac
}

# ── 用法與分發 ──────────────────────────────────────────────────────────────
usage() {
	cat <<EOF
${C_BOLD}zcode2api 管理腳本（Linux）${C_RST}

用法:
  sudo $0                    交互式菜單
  sudo $0 <命令> [選項]      非交互執行

命令:
  install            安裝二進制（+ systemd 服務）
  update             更新二進制（比對 Release 版本）
  uninstall          卸載二進制
  status             查看安裝狀態
  show-keys          顯示後台密碼與網關 API Key
  scan               掃描 /opt 下已有的本程序安裝（含手工編譯的）
  adopt              把手工部署的目錄納入管理（--dir 指定）
  migrate            接管 + 部署最新 + 遷移數據（--dir 指定源目錄）
  docker-install     Docker 安裝（參考實現，未驗證）
  docker-update      Docker 更新
  docker-uninstall   Docker 卸載
  help               顯示本說明

選項:
  --dir DIR           安裝目錄（預設 $DIR）
  --port PORT         監聽端口（預設 $PORT）
  --host ADDR         監聽地址（預設 $HOST；反向代理後建議 127.0.0.1）
  --user USER         以該賬號運行（預設 root；不存在則自動創建）
  --local             從本機源碼構建（需 Go 工具鏈）
  --version TAG       指定 Release 標籤（如 v1.2.3），預設取最新
  --no-browser        不安裝驗證碼瀏覽器依賴（改用後台人工回填）
  --no-prefetch-browser  安裝時不預下載補丁 Chromium（預設會下載，約 200MB）
  --no-deps           跳過系統依賴安裝
  --purge             卸載時連同數據目錄一併刪除
  --keep-user         卸載時保留系統賬號
  --volumes           Docker 卸載時連同數據卷一併刪除
  -y, --yes           所有確認自動回答 yes（非交互場景）
  -h, --help          顯示本說明

示例:
  sudo $0 install --port 3010 --user zcode
  sudo $0 update -y
  sudo $0 show-keys
  sudo $0 scan
  sudo $0 adopt --dir /opt/zcode2api-custom
  sudo $0 migrate --dir /opt/zcode2api-custom
  sudo $0 uninstall --purge
  sudo $0 docker-install
EOF
}

parse_args() {
	CMD="${1:-}"
	if [ -n "$CMD" ]; then shift; fi
	while [ $# -gt 0 ]; do
		case "$1" in
			--dir) ADOPT_DIR="${2:?--dir 需要路徑}"; shift 2 ;;
			--docker-dir) DOCKER_DIR="${2:?--docker-dir 需要路徑}"; shift 2 ;;
			--port) PORT="${2:?--port 需要端口}"; shift 2 ;;
			--host) HOST="${2:?--host 需要地址}"; shift 2 ;;
			--user) RUN_USER="${2:?--user 需要賬號名}"; shift 2 ;;
			--version) VERSION="${2:?--version 需要標籤}"; shift 2 ;;
			--local) SOURCE="local"; shift ;;
			--no-browser) ENABLE_BROWSER="false"; shift ;;
			--prefetch-browser) PREFETCH_BROWSER="true"; shift ;;
			--no-prefetch-browser) PREFETCH_BROWSER="false"; shift ;;
			--no-deps) WITH_DEPS="false"; shift ;;
			--purge) PURGE="true"; shift ;;
			--keep-user) KEEP_USER="true"; shift ;;
			--volumes) DOCKER_VOLUMES="true"; shift ;;
			-y|--yes) ASSUME_YES="true"; shift ;;
			-h|--help) usage; exit 0 ;;
			*) die "未知參數: $1（help 查看用法）" ;;
		esac
	done
}

main() {
	parse_args "$@"

	# --dir 的語義隨命令而變：adopt / migrate 指的是「要操作的既有目錄」，
	# 其餘命令指的是「本腳本的安裝目錄」。
	# 不能對 adopt / migrate 也賦給 DIR：它們內部靠 src = DIR 判斷源是否已是標準目錄，
	# 一旦 DIR 被改成源目錄，就會誤判爲原地升級而放棄搬遷。
	case "${CMD:-}" in
		adopt|migrate) ;;
		*) [ -n "$ADOPT_DIR" ] && DIR="$ADOPT_DIR" ;;
	esac

	case "${CMD:-}" in
		"")           menu ;;
		install)      require_linux; bin_install ;;
		update)       require_linux; bin_update ;;
		uninstall)    require_linux; bin_uninstall ;;
		status)       require_linux; bin_status ;;
		show-keys)    require_linux; bin_show_keys ;;
		scan)         require_linux; bin_scan ;;
		adopt)        require_linux; bin_adopt ;;
		migrate)      require_linux; bin_migrate ;;
		docker-install)   require_linux; docker_install ;;
		docker-update)    require_linux; docker_update ;;
		docker-uninstall) require_linux; docker_uninstall ;;
		help|-h|--help)   usage ;;
		*) usage; die "未知命令: $CMD" ;;
	esac
}

main "$@"
