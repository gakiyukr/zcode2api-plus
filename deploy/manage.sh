#!/usr/bin/env bash
# zcode2api Linux 管理脚本：二进制 / Docker 的安装、更新、卸载与状态查看。
#
# 交互式菜单（无参数运行时）：
#   sudo ./deploy/manage.sh
#
# 非交互子命令（供脚本/CI 调用）：
#   sudo ./deploy/manage.sh install          # 二进制安装
#   sudo ./deploy/manage.sh update           # 二进制更新（比对版本）
#   sudo ./deploy/manage.sh uninstall        # 二进制卸载
#   sudo ./deploy/manage.sh status           # 查看安装状态
#   sudo ./deploy/manage.sh docker-install   # Docker 安装
#   sudo ./deploy/manage.sh docker-update    # Docker 更新
#   sudo ./deploy/manage.sh docker-uninstall # Docker 卸载
#
# 仅支持 Linux。Windows / macOS 请直接使用 Releases 二进制。
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
# 预下载补丁 Chromium：默认开启，避免新机首次请求等待数分钟。
# 用 --no-prefetch-browser 关闭（或 --no-browser 一并跳过验证码浏览器）。
PREFETCH_BROWSER="true"
PURGE="false"
KEEP_USER="false"
ADOPT_DIR=""
DOCKER_VOLUMES="false"
ASSUME_YES="false"

SELF_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname -- "$SELF_DIR")"
SELF_BASENAME="$(basename -- "${BASH_SOURCE[0]}")"
# 扫描根目录：手工部署常见于 /opt 下的自建目录，故以 /opt 为界递归查找。
SCAN_ROOT="${SCAN_ROOT:-/opt}"

# ── 输出 ────────────────────────────────────────────────────────────────────
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

# ── 环境检查 ────────────────────────────────────────────────────────────────
require_root() {
	[ "$(id -u)" -eq 0 ] || die "需要 root 权限。请用: sudo $0 $*"
}

require_linux() {
	[ "$(uname -s)" = "Linux" ] || die "本脚本仅支持 Linux（当前: $(uname -s)）"
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "不支持的架构: $(uname -m)（仅提供 amd64 / arm64）" ;;
	esac
}

detect_pkg_mgr() {
	PKG_MGR=""
	for m in apt-get dnf yum pacman apk; do
		if command -v "$m" >/dev/null 2>&1; then PKG_MGR="$m"; break; fi
	done
}

# validate_port 校验 $PORT 合法性（交互与非交互路径共用）。
validate_port() {
	case "$PORT" in
		''|*[!0-9]*) die "端口必须是数字: $PORT" ;;
	esac
	if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
		die "端口超出范围（1-65535）: $PORT"
	fi
}

# validate_host 校验 $HOST：接受 IP 字面量或空（表示全部接口）。
# 反向代理部署应设为 127.0.0.1，只监听回环。
validate_host() {
	[ -z "$HOST" ] && return 0
	case "$HOST" in
		0.0.0.0|::|127.0.0.1|::1) return 0 ;;
	esac
	# 其余情况要求是合法 IP 字面量（不做 DNS 解析，避免启动期依赖网络）
	if ! printf '%s' "$HOST" | grep -qE '^[0-9a-fA-F:.]+$'; then
		die "ZCODE_HOST 必须是 IP 字面量（如 127.0.0.1、0.0.0.0、::）: $HOST"
	fi
}

# ── 系统依赖 ────────────────────────────────────────────────────────────────
# 验证码浏览器（cloakbrowser 补丁 Chromium）所需的共享库。
# 清单由 ldd 对官方 linux-x64 二进制实测得出；按发行版命名差异列出别名，
# 安装前逐个探测存在性，避免因 t64 后缀等命名变化整体失败。
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
	info "安装系统依赖: $1"
	case "$PKG_MGR" in
		apt-get) DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $1 ;;
		dnf) dnf install -y $1 ;;
		yum) yum install -y $1 ;;
		pacman) pacman -S --noconfirm --needed $1 ;;
		apk) apk add --no-cache $1 ;;
	esac
}

install_base_deps() {
	[ "$WITH_DEPS" = "true" ] || { info "跳过系统依赖（--no-deps）"; return 0; }
	[ -n "$PKG_MGR" ] || { warn "未识别包管理器，请手动安装 curl 与 tar"; return 0; }
	local need=""
	for c in curl tar; do
		command -v "$c" >/dev/null 2>&1 || need="$need $c"
	done
	pkg_install "$(filter_available "$need")"
}

install_browser_deps() {
	[ "$ENABLE_BROWSER" = "true" ] || return 0
	[ "$WITH_DEPS" = "true" ] || { info "跳过浏览器依赖（--no-deps）"; return 0; }
	[ -n "$PKG_MGR" ] || { warn "未识别包管理器，请手动安装验证码浏览器依赖"; return 0; }
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
	[ -n "$resolved" ] || { warn "浏览器依赖清单在本发行版上无法解析，请手动安装"; return 0; }
	pkg_install "$resolved"
}

# ── 版本与下载 ──────────────────────────────────────────────────────────────
# human_size 把字节数格式化为人类可读形式（纯 awk，避免依赖 numfmt，
# 后者属于 coreutils 但部分精简镜像会裁掉）。
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
	# || true：API 限流或网络故障时 curl 返回非 0，set -e + pipefail 会让脚本在此
	# 直接退出，连调用方的错误提示都来不及打印——表现为「查询最新 Release」后
	# 无声终止。查询失败应返回空串，由调用方决定如何提示。
	curl -fsSL --connect-timeout 15 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true
}

# build_local 从源码构建到 $DIR/zcode2api。
# 返回非 0 表示失败（调用方决定回滚或退出）——同 fetch_release，不可在此 die。
build_local() {
	command -v go >/dev/null 2>&1 || { warn "--local 需要 Go 工具链（未找到 go）"; return 1; }
	[ -f "$REPO_ROOT/go.mod" ] || { warn "--local 需在仓库内执行（未找到 $REPO_ROOT/go.mod）"; return 1; }
	info "从源码构建（CGO_ENABLED=0，静态）"
	( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
		go build -trimpath -ldflags="-s -w" -o "$DIR/zcode2api" ./cmd/zcode2api ) || { warn "构建失败"; return 1; }
	chmod 0755 "$DIR/zcode2api" || return 1
	return 0
}

# fetch_release 下载指定 tag 到 $DIR/zcode2api。
# 返回非 0 表示失败（调用方决定回滚或退出）——不可在此 die，
# 否则 bin_update 的回滚与重启逻辑永远不会执行，服务会停在停止状态。
fetch_release() {
	local tag="$1"
	local url="https://github.com/$REPO/releases/download/$tag/zcode2api-linux-$ARCH"
	info "下载 $url"
	if ! curl -fL --retry 3 --connect-timeout 15 --progress-bar -o "$DIR/zcode2api" "$url"; then
		warn "下载失败: $url"
		warn "若该版本尚未发布产物，请改用 --local 从源码构建"
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
		info "使用服务模板 $src"
		cat "$src" >"$tmp"
	else
		info "使用内建服务模板"
		cat >"$tmp" <<'UNIT_EOF'
[Unit]
Description=zcode2api - Z.AI ZCode Coding Plan 网关
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
		"$tmp" >"$dst" || die "写入服务单元失败: $dst（权限不足？）"

	if ! grep -q '^ExecStart=' "$dst"; then
		die "服务单元内容异常（缺少 ExecStart）: $dst"
	fi

	# 安装在 /home 或 /root 下时，ProtectHome=read-only 会使服务无法写入数据目录，
	# 此时移除该加固项（保留其余沙箱限制）。
	case "$DIR" in
		/home/*|/root/*|/root)
			sed -i '/^ProtectHome=/d' "$dst"
			warn "安装目录位于家目录下，已移除 systemd 的 ProtectHome 限制以允许写入"
			;;
	esac

	chmod 0644 "$dst"
	eval "$cleanup"
	ok "已安装 systemd 服务 $dst"
}

write_env_file() {
	if [ -f "$DIR/.env" ]; then
		info "保留既有 $DIR/.env"
		return 0
	fi
	cat >"$DIR/.env" <<EOF
# zcode2api 环境配置（由 manage.sh 生成，可自行编辑后 systemctl restart zcode2api）
# ZCODE_HOST=127.0.0.1 时仅监听本机回环，适合放在反向代理之后
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
		info "建立系统账号 $RUN_USER"
		useradd --system --create-home --home-dir "$DIR" --shell /usr/sbin/nologin "$RUN_USER" \
			|| die "建立账号 $RUN_USER 失败"
		RUN_GROUP="$(id -gn "$RUN_USER")"
	fi
	[ -n "$RUN_GROUP" ] || die "无法确定账号 $RUN_USER 的主组"
}

print_keys_hint() {
	local ip
	ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
	[ -n "$ip" ] || ip="<本机IP>"
	if systemctl is-active --quiet zcode2api.service 2>/dev/null; then
		ok "服务运行中: http://$ip:$PORT/admin/login"
		local key_line
		key_line="$(journalctl -u zcode2api --since "-2min" --no-pager 2>/dev/null \
			| grep -E "初始后台密码|网关 API Key" | tail -2 || true)"
		if [ -n "$key_line" ]; then
			echo
			warn "以下密钥仅在首次启动时显示，请立即保存："
			printf '%s\n' "$key_line" | sed 's/^/    /'
		else
			echo
			info "密钥已存在于数据库中（非首次启动）。遗忘可在后台「系统设置」查看。"
		fi
	else
		warn "服务未处于运行状态，请执行: journalctl -u zcode2api -n 50"
	fi
}

# ── 二进制：安装 / 更新 / 卸载 / 状态 ───────────────────────────────────────
bin_install() {
	require_root
	detect_arch
	detect_pkg_mgr
	resolve_run_user
	validate_port
	validate_host
	info "安装目录: $DIR    运行账号: $RUN_USER    架构: linux/$ARCH"
	install_base_deps
	install_browser_deps

	mkdir -p "$DIR/data"
	if [ "$RUN_USER" != "root" ]; then
		if ! chown -R "$RUN_USER:$RUN_GROUP" "$DIR" 2>/dev/null; then
			warn "无法变更 $DIR 属主为 $RUN_USER:$RUN_GROUP，请手动确认该账号有写入权限"
		fi
	fi

	if [ "$SOURCE" = "local" ]; then
		build_local || die "安装失败：从源码构建未成功"
	else
		local tag="$VERSION"
		if [ -z "$tag" ]; then
			info "查询最新 Release"
			tag="$(latest_tag)"
			[ -n "$tag" ] || die "未找到任何 Release 产物。请改用 --local 从源码构建，或用 --version 指定标签"
		fi
		fetch_release "$tag" || die "安装失败：无法下载 $tag。请改用 --local 从源码构建，或用 --version 指定其他标签"
		printf '%s' "$tag" >"$DIR/.installed-version"
		ok "已安装版本 $tag"
	fi

	write_env_file
	write_unit

	if [ "$PREFETCH_BROWSER" = "true" ] && [ "$ENABLE_BROWSER" = "true" ]; then
		info "预下载补丁 Chromium（约 200MB，可能需要数分钟）"
		if ( cd "$DIR" && set -a && . "$DIR/.env" && set +a && \
			timeout 1800 "$DIR/zcode2api" prefetch-browser ); then
			ok "浏览器预下载完成"
		else
			warn "预下载未成功；服务首次使用时会自动重试"
		fi
	fi

	systemctl daemon-reload || die "systemctl daemon-reload 失败（本机可能未使用 systemd）"
	systemctl enable --now zcode2api.service || die "启动服务失败，请查看: journalctl -u zcode2api -n 50"
	sleep 2
	echo
	ok "安装完成"
	print_summary
	print_keys_hint
}

bin_update() {
	require_root
	detect_arch
	[ -x "$DIR/zcode2api" ] || die "未检测到已安装的二进制（$DIR/zcode2api）。请先执行安装"

	local current target
	current="$(installed_version)"
	[ -n "$current" ] || current="(未知)"

	if [ "$SOURCE" = "local" ]; then
		info "当前版本: $current；从源码重新构建"
		confirm "确认更新？" || { info "已取消"; return 0; }
		cp -f "$DIR/zcode2api" "$DIR/zcode2api.bak" 2>/dev/null || true
		systemctl stop zcode2api.service 2>/dev/null || true
		if ! build_local; then
			warn "构建失败，回滚到原二进制"
			[ -f "$DIR/zcode2api.bak" ] && mv -f "$DIR/zcode2api.bak" "$DIR/zcode2api"
			systemctl start zcode2api.service 2>/dev/null \
				|| warn "服务重启失败，请手动执行: systemctl start zcode2api"
			die "更新中止（已回滚到 $current）"
		fi
		rm -f "$DIR/zcode2api.bak"
		ok "已重新构建"
	else
		# --version 优先：GitHub API 未认证限流（每小时 60 次，共享出口 IP 极易撞上）
		# 时 latest_tag 会返回空并导致更新中止，显式指定标签可绕开该查询。
		target="$VERSION"
		if [ -n "$target" ]; then
			info "当前版本: $current；目标版本（--version 指定）: $target"
		else
			info "当前版本: $current；查询最新 Release"
			target="$(latest_tag)"
			[ -n "$target" ] || die "无法获取最新版本（网络问题或 GitHub API 限流）。请用 --version TAG 指定标签"
		fi

		if [ "$current" = "$target" ]; then
			ok "已是最新版本 $current"
			confirm "仍要重新下载并覆盖安装？" || return 0
		else
			info "可更新: $current → $target"
			confirm "确认更新到 $target？" || { info "已取消"; return 0; }
		fi

		# 备份当前二进制，下载失败可回滚
		cp -f "$DIR/zcode2api" "$DIR/zcode2api.bak" 2>/dev/null || true
		systemctl stop zcode2api.service 2>/dev/null || true

		if ! fetch_release "$target"; then
			warn "更新失败，回滚到原二进制"
			if [ -f "$DIR/zcode2api.bak" ]; then
				mv -f "$DIR/zcode2api.bak" "$DIR/zcode2api" || warn "回滚失败，请检查 $DIR/zcode2api"
			fi
			systemctl start zcode2api.service 2>/dev/null \
				|| warn "服务重启失败，请手动执行: systemctl start zcode2api"
			die "更新中止（已回滚到 $current）"
		fi
		printf '%s' "$target" >"$DIR/.installed-version"
		rm -f "$DIR/zcode2api.bak"
	fi

	systemctl daemon-reload 2>/dev/null || true
	systemctl start zcode2api.service || die "启动失败，请查看: journalctl -u zcode2api -n 50"
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
		if ! confirm "将移除服务与二进制，数据目录 $DIR/data 会保留。继续？"; then
			info "已取消"; return 0
		fi
	fi

	if [ -f "$unit" ]; then
		info "停止并禁用服务"
		systemctl disable --now zcode2api.service 2>/dev/null || warn "服务未在运行或取消失败"
		rm -f "$unit"
		systemctl daemon-reload 2>/dev/null || true
		ok "已移除 $unit"
	else
		info "未找到 $unit，跳过服务移除"
	fi

	if [ "$PURGE" = "true" ]; then
		if [ -d "$DIR" ]; then
			warn "删除 $DIR（含账号凭证与数据库）"
			rm -rf "$DIR"
			ok "已删除 $DIR"
		fi
	else
		info "保留数据目录 $DIR/data（--purge 可一并删除）"
		for f in zcode2api zcode2api.bak .installed-version .env; do
			[ -e "$DIR/$f" ] && rm -f "$DIR/$f"
		done
		[ -d "$DIR/browser" ] && info "保留浏览器缓存 $DIR/browser"
		ok "已移除二进制与配置"
	fi

	if [ -n "$run_user" ] && [ "$run_user" != "root" ] && [ "$KEEP_USER" != "true" ]; then
		if id "$run_user" >/dev/null 2>&1; then
			info "删除系统账号 $run_user"
			userdel "$run_user" 2>/dev/null || warn "删除账号 $run_user 失败，请手动处理"
		fi
	fi
	echo
	ok "卸载完成"
}

print_summary() {
	cat <<EOF

  服务状态   systemctl status zcode2api
  实时日志   journalctl -u zcode2api -f
  配置文件   $DIR/.env
  数据目录   $DIR/data
  管理脚本   sudo $SELF_DIR/manage.sh

EOF
}

# probe_binary_version 从二进制中提取版本串（AppVersion 常量，形如 2.0.3-go）。
# 手工编译或剥离符号的产物没有 .installed-version，只能从内容反推；
# 用 grep -a 而非 strings：前者所有 Linux 发行版都有，后者常在最小化系统里缺失。
probe_binary_version() {
	local bin="$1"
	[ -f "$bin" ] || return 0
	# || true：未匹配到版本串时 grep 返回 1，pipefail 会将其视为失败并终止调用方。
	grep -aoE '[0-9]+\.[0-9]+\.[0-9]+-go' "$bin" 2>/dev/null | sort -u | head -1 || true
}

# probe_binary_service 找出哪个 systemd 单元在运行给定的二进制。
probe_binary_service() {
	local bin="$1" unit exec_line
	command -v systemctl >/dev/null 2>&1 || return 0
	# 命令替换放进 for 的 word list 时退出码会被忽略，但 WSL 等无 systemd 环境下
	# 仍可能因空列表产生意外行为，故显式容错。
	for unit in $(systemctl list-units --type=service --state=running --no-legend --plain 2>/dev/null | awk '{print $1}' || true); do
		exec_line="$(systemctl show -p ExecStart --value "$unit" 2>/dev/null || true)"
		case "$exec_line" in
			*"$bin"*) printf '%s' "$unit"; return 0 ;;
		esac
	done
}

# probe_running_pid 找出直接以该路径运行的进程（无 systemd 的手工部署）。
probe_running_pid() {
	local bin="$1"
	command -v pgrep >/dev/null 2>&1 || return 0
	# 同理：进程不在跑时 pgrep 返回 1，不能让它冒泡成脚本错误。
	pgrep -f "^${bin}( |$)" 2>/dev/null | head -1 || true
}

# scan_report_one 输出单个候选项的详情。
# 返回 0 表示这是个可接管的部署目录（含可执行文件），非 0 表示只是散落文件。
scan_report_one() {
	local bin="$1" dir ver size running svc pid mark

	dir="$(dirname "$bin")"
	ver="$(probe_binary_version "$bin")"
	size="$(stat -c '%s' "$bin" 2>/dev/null || echo 0)"
	svc="$(probe_binary_service "$bin")"
	pid="$(probe_running_pid "$bin")"

	if [ -n "$svc" ]; then
		running="运行中（$svc）"
	elif [ -n "$pid" ]; then
		running="运行中（PID $pid，无 systemd 单元）"
	else
		running="未运行"
	fi

	mark=" "
	[ "$dir" = "$DIR" ] && mark="*"

	printf '  %s %s\n' "$mark" "$bin"
	printf '      版本       %s\n' "${ver:-(未知，无 .installed-version)}"
	printf '      大小       %s\n' "$(human_size "$size")"
	printf '      状态       %s\n' "$running"

	# 关联资产：决定这是不是一个完整部署，以及能否安全接管
	local assets="" a
	for a in .env data browser .installed-version; do
		[ -e "$dir/$a" ] && assets="$assets $a"
	done
	[ -n "$assets" ] && printf '      关联       %s\n' "$assets"

	if [ -x "$dir/zcode2api" ] && [ "$dir" != "$DIR" ]; then
		printf '      %s可接管%s    sudo %s adopt --dir %s\n' "$C_DIM" "$C_RST" "$SELF_BASENAME" "$dir"
	fi
	echo
	return 0
}

# bin_scan 在 /opt 下寻找本程序的既有安装。
# 存在的理由：手工编译部署的实例没有 .installed-version，也没有 systemd 单元，
# 管理脚本的固定 $DIR 看不见它，导致 update/status 报「未安装」。
bin_scan() {
	require_root
	local found=0 bin

	echo
	printf '%s扫描既有安装%s\n' "$C_BOLD" "$C_RST"
	hr
	info "扫描路径: $DIR 及 $SCAN_ROOT 下的同名可执行文件"

	# 先查标准位置：这是脚本自己的安装目录
	if [ -x "$DIR/zcode2api" ]; then
		found=1
		scan_report_one "$DIR/zcode2api"
	fi

	# 再递归找散落的同名可执行文件。
	# 只认 basename 恰为 zcode2api 的文件：zcode2api.bak / .old 这类是备份，
	# 与主程序同目录，逐个详报只会把同一部署刷屏。
	# -x 过滤可执行位，避免把 data/ 里的缓存文件也报出来。
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
		printf '%s  同目录下的其他文件（备份等，未详列）:%s\n' "$C_DIM" "$C_RST"
		sed 's/^/    /' "$extras_file"
		echo
	fi
	rm -f "$extras_file"

	if [ "$found" -eq 0 ]; then
		warn "未找到本程序的任何安装"
		echo
		info "可执行文件可能不在 $SCAN_ROOT，或名称不同。手动确认:"
		printf '  find / -name "zcode2api*" -type f -executable 2>/dev/null\n'
		echo
		return 0
	fi

	printf '%s* = 管理脚本当前使用的目录%s\n' "$C_DIM" "$C_RST"
	echo
	info "接管一个散落的部署（写入 .installed-version、建 systemd 单元）:"
	printf '  sudo %s adopt --dir <目录>\n' "$SELF_BASENAME"
	echo
}

# count_accounts 读取某个数据目录下的账号总数，用于迁移前后校验。
# 直接查 SQLite 而不是调用二进制的 accounts 子命令：后者需要启动整套配置
# （密钥、验证码浏览器等），而校验只关心行数。
count_accounts() {
	local data_dir="$1" db="$1/accounts.db"
	[ -f "$db" ] || { printf '0'; return 0; }
	command -v sqlite3 >/dev/null 2>&1 || return 0
	sqlite3 "$db" 'SELECT COUNT(*) FROM accounts;' 2>/dev/null || printf '0'
}

# sqlite_backup 用 SQLite 自己的备份机制导出一致快照。
# 不能只 cp accounts.db：库以 WAL 模式运行，最近的写入还在 accounts.db-wal 里，
# 只拷主文件会静默丢掉最新数据。
snapshot_db() {
	local src_dir="$1" dst_file="$2" src_db="$1/accounts.db"
	[ -f "$src_db" ] || return 1
	if command -v sqlite3 >/dev/null 2>&1; then
		sqlite3 "$src_db" ".backup '$dst_file'" 2>/dev/null && return 0
	fi
	# 无 sqlite3 时退化为整目录拷贝（含 -wal/-shm），由调用方确保服务已停。
	cp -f "$src_db" "$dst_file" || return 1
	[ -f "$src_db-wal" ] && cp -f "$src_db-wal" "$dst_file-wal" 2>/dev/null
	[ -f "$src_db-shm" ] && cp -f "$src_db-shm" "$dst_file-shm" 2>/dev/null
	return 0
}

# migrate_pick_target 决定迁移目标目录。
# 目标始终是标准目录 $DIR：迁移的意义就是把散落部署收拢到标准位置。
# 若源本身就是 $DIR，则原地升级（无处分可搬）。
# 若 $DIR 已被别的部署占用，加后缀——两个不同实例不该被合并到同一目录。
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

# menu_migrate 交互式迁移：扫描候选 → 让用户选一个 → 走 bin_migrate。
# 菜单里不能要求用户先手打路径，否则等于把扫描结果白报了。
menu_migrate() {
	require_root

	local -a cands=()
	local bin
	# 候选来自 scan 的同一判据：basename 恰为 zcode2api 且可执行
	while IFS= read -r bin; do
		[ -n "$bin" ] || continue
		[ -x "$bin" ] || continue
		[ "$(basename "$bin")" = "zcode2api" ] || continue
		[ "$(dirname "$bin")" = "$DIR" ] && continue
		cands+=("$(dirname "$bin")")
	done < <(find "$SCAN_ROOT" -maxdepth 4 -type f -name 'zcode2api' 2>/dev/null | sort)

	echo
	printf '%s迁移既有部署%s\n' "$C_BOLD" "$C_RST"
	hr

	if [ "${#cands[@]}" -eq 0 ]; then
		warn "未在 $SCAN_ROOT 下找到可迁移的部署（已排除标准目录 $DIR）"
		info "若部署在别处，用命令行指定: sudo $SELF_BASENAME migrate --dir <目录>"
		echo
		return 0
	fi

	local i
	for i in "${!cands[@]}"; do
		bin="${cands[$i]}/zcode2api"
		printf '  %s%d)%s %s\n' "$C_CYAN" "$((i + 1))" "$C_RST" "${cands[$i]}"
		printf '       版本 %s   %s\n' \
			"$(probe_binary_version "$bin" || echo '?')" \
			"$(probe_binary_service "$bin" || echo '未运行')"
	done
	printf '  %s0)%s 取消\n' "$C_CYAN" "$C_RST"
	echo

	local pick
	printf '请选择要迁移的部署: '
	read -r pick || return 0
	case "$pick" in
		0|"") info "已取消"; return 0 ;;
	esac
	if ! [[ "$pick" =~ ^[0-9]+$ ]] || [ "$pick" -lt 1 ] || [ "$pick" -gt "${#cands[@]}" ]; then
		warn "无效选择: $pick"
		return 0
	fi

	ADOPT_DIR="${cands[$((pick - 1))]}"
	bin_migrate
}

# bin_migrate 把既有部署迁到标准目录并升级到最新 Release。
# 与 adopt 的区别：adopt 只补管理元数据、不动二进制；migrate 会真正部署新版本，
# 因此每一步都可回滚，且旧目录默认保留。
bin_migrate() {
	require_root
	detect_arch

	local src="${ADOPT_DIR:-}"
	[ -n "$src" ] || die "需要 --dir 指定源部署目录，例如: sudo $SELF_BASENAME migrate --dir /opt/zcode2api-custom"
	src="$(cd "$src" 2>/dev/null && pwd)" || die "目录不存在: ${ADOPT_DIR}"
	[ -f "$src/zcode2api" ] || die "$src 下没有 zcode2api 可执行文件"

	local target
	target="$(migrate_pick_target "$src")"
	local target_is_src="false"
	[ "$target" = "$src" ] && target_is_src="true"

	echo
	printf '%s迁移既有部署%s\n' "$C_BOLD" "$C_RST"
	hr
	printf '  源目录     %s\n' "$src"
	printf '  目标目录   %s\n' "$target"
	printf '  源版本     %s\n' "$(probe_binary_version "$src/zcode2api" || echo '(未知)')"
	printf '  运行状态   %s\n' "$(probe_binary_service "$src/zcode2api" || echo '未运行')"
	echo

	# 目标版本：优先 --version，否则查最新 Release（可能因 API 限流失败）
	local tag="$VERSION"
	if [ -z "$tag" ]; then
		info "查询最新 Release"
		tag="$(latest_tag)"
		if [ -z "$tag" ]; then
			die "无法获取最新版本（网络问题或 GitHub API 限流）。请用 --version TAG 指定标签"
		fi
	fi
	printf '  目标版本   %s\n' "$tag"
	echo

	confirm "确认迁移？" || { info "已取消"; return 0; }

	# 1) 停止旧服务。必须在拷贝数据前停：WAL 模式下运行中的写入会让快照不一致。
	local svc
	svc="$(probe_binary_service "$src/zcode2api" || true)"
	local stopped_unit=""
	if [ -n "$svc" ]; then
		info "停止服务 $svc"
		systemctl stop "$svc" 2>/dev/null || true
		stopped_unit="$svc"
	fi
	# 也可能有无 systemd 的裸进程
	local pid
	pid="$(probe_running_pid "$src/zcode2api" || true)"
	if [ -n "$pid" ]; then
		warn "发现直接运行的进程 PID $pid，正在停止"
		kill "$pid" 2>/dev/null || true
		sleep 2
		kill -9 "$pid" 2>/dev/null || true
	fi

	# 2) 记录源数据基线，供迁移后校验
	local src_accounts=""
	if [ -f "$src/data/accounts.db" ]; then
		src_accounts="$(count_accounts "$src/data")"
		info "源数据账号数: ${src_accounts:-未知}"
	fi

	# 3) 准备目标目录
	if [ "$target_is_src" = "false" ]; then
		mkdir -p "$target/data" || die "无法创建 $target"
	fi

	# 4) 部署新二进制。先备份，失败可回滚。
	local bak="$target/zcode2api.pre-migrate"
	cp -f "$target/zcode2api" "$bak" 2>/dev/null || true
	local dl_dir="$target"
	if [ "$target_is_src" = "false" ]; then
		info "下载 $tag 到 $target"
	else
		info "下载 $tag（原地升级）"
	fi
	local url="https://github.com/$REPO/releases/download/$tag/zcode2api-linux-$ARCH"
	if ! curl -fL --retry 3 --connect-timeout 15 --progress-bar -o "$target/zcode2api.new" "$url"; then
		warn "下载失败: $url"
		[ -f "$bak" ] && mv -f "$bak" "$target/zcode2api"
		warn "迁移中止（二进制已回滚）"
		[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
		die "下载 $tag 失败"
	fi
	chmod 0755 "$target/zcode2api.new" || die "设置可执行权限失败"

	# 5) 迁移数据。目标已存在同名库时先留副本，避免覆盖掉目标自己的数据。
	if [ "$target_is_src" = "false" ] && [ -d "$src/data" ]; then
		if [ -f "$target/data/accounts.db" ]; then
			warn "$target/data 已有 accounts.db，保留为 accounts.db.pre-migrate"
			mv -f "$target/data/accounts.db" "$target/data/accounts.db.pre-migrate" 2>/dev/null || true
		fi
		info "复制数据 $src/data → $target/data"
		if ! cp -a "$src/data/." "$target/data/" 2>/dev/null; then
			warn "数据复制失败"
			rm -f "$target/zcode2api.new"
			[ -f "$bak" ] && mv -f "$bak" "$target/zcode2api"
			[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
			die "迁移中止（二进制已回滚，数据未改动）"
		fi
	fi

	# 6) 校验账号数。数量不符说明快照不完整，宁可停下让人检查。
	if [ -n "$src_accounts" ]; then
		local dst_accounts
		dst_accounts="$(count_accounts "$target/data")"
		if [ "$dst_accounts" != "$src_accounts" ]; then
			warn "账号数不一致: 源 $src_accounts → 目标 $dst_accounts"
			warn "数据可能未完整迁移，已保留新二进制于 $target/zcode2api.new 供检查"
			[ -n "$stopped_unit" ] && systemctl start "$stopped_unit" 2>/dev/null
			die "迁移校验失败，未切换"
		fi
		ok "账号数校验通过: $dst_accounts"
	fi

	# 7) 切换二进制
	mv -f "$target/zcode2api.new" "$target/zcode2api" || die "替换二进制失败"
	ok "已部署 $tag"

	# 8) 迁移 .env（保留源配置：端口、密钥等都在里面）
	if [ "$target_is_src" = "false" ] && [ -f "$src/.env" ]; then
		if [ -f "$target/.env" ]; then
			cp -f "$target/.env" "$target/.env.pre-migrate" 2>/dev/null || true
		fi
		cp -f "$src/.env" "$target/.env" && chmod 0600 "$target/.env"
		info "已迁移 .env"
	fi

	printf '%s' "$tag" >"$target/.installed-version"

	# 9) 写单元并启动。DIR 临时指向 target 以复用单元模板。
	local old_dir="$DIR"
	DIR="$target"
	write_unit || { DIR="$old_dir"; die "写入 systemd 单元失败"; }
	DIR="$old_dir"

	if [ -n "$stopped_unit" ] && [ "$stopped_unit" != "zcode2api.service" ]; then
		systemctl disable "$stopped_unit" 2>/dev/null || true
		info "已停用旧单元 $stopped_unit"
	fi
	systemctl daemon-reload 2>/dev/null || true
	systemctl restart zcode2api.service 2>/dev/null \
		|| systemctl start zcode2api.service 2>/dev/null \
		|| warn "服务启动失败，请手动执行: systemctl start zcode2api"
	ok "服务已启动"

	# 10) 清理旧目录。仅在数据已确认迁移且目标不是源目录时进行。
	if [ "$target_is_src" = "false" ]; then
		echo
		if confirm "删除旧目录 $src？（数据已迁移到 $target）"; then
			rm -rf "$src" && ok "已删除 $src"
		else
			info "保留 $src"
			warn "旧目录仍在，两个目录都含 data/；确认新部署正常后可手动删除"
		fi
	fi

	echo
	ok "迁移完成（$tag）"
	printf '  目录   %s\n' "$target"
	printf '  管理   sudo %s status --dir %s\n' "$SELF_BASENAME" "$target"
	echo
}

# bin_adopt 把一个手工部署的目录纳入管理。
# 只补管理所需的元数据与单元，不动二进制本身——避免覆盖用户自行编译的产物。
bin_adopt() {
	require_root
	detect_arch

	local src="${ADOPT_DIR:-}"
	[ -n "$src" ] || die "需要 --dir 指定部署目录，例如: sudo $SELF_BASENAME adopt --dir /opt/zcode2api-custom"
	[ -d "$src" ] || die "目录不存在: $src"

	local bin="$src/zcode2api"
	[ -f "$bin" ] || die "$src 下没有 zcode2api 可执行文件"
	[ -x "$bin" ] || die "$bin 没有可执行权限，请先 chmod +x"
	src="$(cd "$src" && pwd)"

	if [ "$src" = "$DIR" ]; then
		info "$src 已是管理脚本的默认目录，无需接管"
		return 0
	fi

	local ver
	ver="$(probe_binary_version "$bin")"
	info "接管目录: $src"
	[ -n "$ver" ] && info "检测到版本: $ver" || warn "未能从二进制提取版本，接管后 update 将无法比对版本"

	confirm "确认接管 $src？" || { info "已取消"; return 0; }

	# 单元里的路径与用户需要指向新目录：临时切换 DIR 后复用现成的写入逻辑，
	# 而不是把单元模板再抄一份（抄一份就会有两处需要同步维护）。
	# 既有单元若与我们写入的不同名，接管后两个单元会同时拉起同一二进制、
	# 抢同一端口（后启动者 bind 失败）。故接管前先停用旧单元，而不是并存。
	local svc="$(probe_binary_service "$bin")"
	local old_unit=""
	if [ -n "$svc" ] && [ "/etc/systemd/system/$svc" != "$UNIT_PATH" ]; then
		old_unit="$svc"
		warn "检测到 $svc 正在运行该二进制"
		info "接管后将停用 $svc 并改用 $UNIT_PATH，避免两个单元争抢端口"
	elif [ -n "$svc" ]; then
		info "检测到 $svc 正在运行该二进制，将就地重启"
	fi

	local old_dir="$DIR"
	DIR="$src"
	write_unit || { DIR="$old_dir"; die "写入 systemd 单元失败"; }
	DIR="$old_dir"

	# .installed-version 让 update/status 能识别版本；缺失时 update 会当作未知版本。
	# 手工编译的产物无法确定对应哪个 Release，故用二进制内嵌版本号而非猜一个 tag。
	if [ -n "$ver" ]; then
		printf 'v%s' "$ver" >"$src/.installed-version"
		info "已写入 $src/.installed-version = v$ver"
	else
		warn "跳过 .installed-version（版本未知），update 将提示无法比对"
	fi

	if [ -n "$old_unit" ]; then
		systemctl stop "$old_unit" 2>/dev/null || true
		systemctl disable "$old_unit" 2>/dev/null || true
		info "已停用旧单元 $old_unit"
	fi

	systemctl daemon-reload 2>/dev/null || true
	if confirm "现在启动服务？"; then
		systemctl restart zcode2api.service 2>/dev/null \
			|| systemctl start zcode2api.service 2>/dev/null \
			|| warn "服务启动失败，请手动执行: systemctl start zcode2api"
		ok "服务已启动"
	fi

	echo
	warn "注意：管理脚本的默认目录仍是 $old_dir"
	info "后续 update/status 需指定目录: sudo $SELF_BASENAME update --dir $src"
	echo
}

bin_status() {
	echo
	printf '%s二进制安装%s\n' "$C_BOLD" "$C_RST"
	hr
	if [ -x "$DIR/zcode2api" ]; then
		printf '  目录       %s\n' "$DIR"
		printf '  版本       %s\n' "$(installed_version | grep . || echo '(未知)')"
		local bin_info
		bin_info="$(stat -c '%s 字节  %y' "$DIR/zcode2api" 2>/dev/null | cut -d. -f1)"
		printf '  二进制     %s\n' "${bin_info:-(未知)}"
	else
		printf '  %s未安装%s（目录: %s）\n' "$C_DIM" "$C_RST" "$DIR"
	fi

	if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT_PATH" ]; then
		local state
		state="$(systemctl is-active zcode2api.service 2>/dev/null || true)"
		printf '  服务       %s\n' "${state:-unknown}"
		printf '  开机自启   %s\n' "$(systemctl is-enabled zcode2api.service 2>/dev/null || echo '-')"
	fi
	if [ -f "$DIR/.env" ]; then
		printf '  监听       %s\n' "$(grep -E '^ZCODE_(HOST|PORT)=' "$DIR/.env" 2>/dev/null | tr '\n' ' ' || echo '-')"
	fi
	if [ -d "$DIR/data" ]; then
		printf '  数据       %s\n' "$(du -sh "$DIR/data" 2>/dev/null | cut -f1 || echo '-')"
	fi
	if [ -d "$DIR/browser" ]; then
		printf '  浏览器缓存 %s\n' "$(du -sh "$DIR/browser" 2>/dev/null | cut -f1 || echo '-')"
	fi
	echo
	printf '%sDocker 部署%s\n' "$C_BOLD" "$C_RST"
	hr
	if command -v docker >/dev/null 2>&1; then
		printf '  Docker     %s\n' "$(docker --version 2>/dev/null | head -1)"
		if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx zcode2api; then
			printf '  容器       %s\n' "$(docker ps -a --filter 'name=^/zcode2api$' --format '{{.Status}}' 2>/dev/null)"
		else
			printf '  容器       %s未创建%s\n' "$C_DIM" "$C_RST"
		fi
		if [ -d "$DOCKER_DIR/.git" ]; then
			printf '  构建目录   %s\n' "$DOCKER_DIR"
		fi
	else
		printf '  %s未安装 Docker%s\n' "$C_DIM" "$C_RST"
	fi
	echo
}

# ── Docker：安装 / 更新 / 卸载 ──────────────────────────────────────────────
# 说明：Docker 路径为参考实现，未经验证。脚本会明确提示并要求确认。
docker_preflight() {
	require_root
	command -v docker >/dev/null 2>&1 || die "未找到 docker。请先安装 Docker Engine"
	if docker compose version >/dev/null 2>&1; then
		DC=(docker compose)
	elif command -v docker-compose >/dev/null 2>&1; then
		DC=(docker-compose)
	else
		die "未找到 docker compose（v2 插件或 v1 独立版）"
	fi
}

docker_ensure_source() {
	# 优先用当前仓库；否则克隆到 DOCKER_DIR
	if [ -f "$REPO_ROOT/Dockerfile" ] && [ -f "$REPO_ROOT/docker-compose.yml" ]; then
		DOCKER_SRC="$REPO_ROOT"
		info "使用当前仓库: $DOCKER_SRC"
		return 0
	fi
	DOCKER_SRC="$DOCKER_DIR"
	if [ -d "$DOCKER_SRC/.git" ]; then
		info "更新已有克隆 $DOCKER_SRC"
		git -C "$DOCKER_SRC" pull --ff-only || warn "git pull 失败，沿用现有代码"
	else
		command -v git >/dev/null 2>&1 || die "需要 git 以克隆仓库（或在仓库内运行本脚本）"
		info "克隆 $REPO_URL 到 $DOCKER_SRC"
		mkdir -p "$(dirname "$DOCKER_SRC")"
		git clone --depth 1 "$REPO_URL" "$DOCKER_SRC" || die "克隆失败"
	fi

	# 克隆/pull 后必须确认构建上下文完整，否则 compose 会在缺失文件上失败
	[ -f "$DOCKER_SRC/docker-compose.yml" ] \
		|| die "构建上下文缺少 docker-compose.yml: $DOCKER_SRC"
	[ -f "$DOCKER_SRC/Dockerfile" ] \
		|| die "构建上下文缺少 Dockerfile: $DOCKER_SRC"
}

docker_warn_unverified() {
	echo
	warn "注意：Docker 方案为参考实现，从未经 docker build 验证，不保证可用。"
	warn "若构建或运行失败，建议改用二进制方案：sudo $SELF_DIR/manage.sh install"
	echo
}

docker_install() {
	docker_preflight
	docker_warn_unverified
	confirm "确认继续 Docker 安装？" || { info "已取消"; return 0; }
	docker_ensure_source
	info "构建并启动容器（首次构建可能较久）"
	( cd "$DOCKER_SRC" && "${DC[@]}" up -d --build ) || die "docker compose up 失败"
	echo
	ok "Docker 部署完成"
	info "查看日志获取后台密码与网关密钥: cd $DOCKER_SRC && ${DC[*]} logs -f"
}

docker_update() {
	docker_preflight
	docker_ensure_source
	confirm "将重新构建镜像并重启容器（数据卷保留）。继续？" || { info "已取消"; return 0; }
	info "重新构建并启动"
	( cd "$DOCKER_SRC" && "${DC[@]}" up -d --build --force-recreate ) || die "更新失败"
	ok "Docker 更新完成"
}

docker_uninstall() {
	docker_preflight
	# 卸载不应强制克隆仓库：优先用当前仓库或已有克隆，找不到则用 docker 直连操作。
	DOCKER_SRC=""
	if [ -f "$REPO_ROOT/docker-compose.yml" ]; then
		DOCKER_SRC="$REPO_ROOT"
	elif [ -f "$DOCKER_DIR/docker-compose.yml" ]; then
		DOCKER_SRC="$DOCKER_DIR"
	fi

	local extra=""
	if [ "$DOCKER_VOLUMES" = "true" ]; then
		extra="-v"
		warn "将同时删除数据卷（账号库与浏览器缓存）"
	else
		info "数据卷会保留（--volumes 可一并删除）"
	fi
	confirm "确认停止并移除容器？" || { info "已取消"; return 0; }

	if [ -n "$DOCKER_SRC" ]; then
		( cd "$DOCKER_SRC" && "${DC[@]}" down $extra ) || die "docker compose down 失败"
	else
		# 找不到 compose 文件：直接操作容器与卷
		info "未找到 compose 文件，直接移除容器"
		docker rm -f zcode2api >/dev/null 2>&1 || warn "容器 zcode2api 不存在或已移除"
		if [ "$DOCKER_VOLUMES" = "true" ]; then
			for v in zcode2api-data zcode2api-browser; do
				docker volume rm "$v" >/dev/null 2>&1 || true
			done
		fi
	fi
	ok "Docker 卸载完成"
	[ -n "$extra" ] && info "数据卷已删除" || info "数据卷已保留，可用 docker volume ls 查看"
}

# ── 交互菜单 ────────────────────────────────────────────────────────────────
menu() {
	require_linux
	while true; do
		echo
		printf '%s╔══════════════════════════════════════════════════════════╗%s\n' "$C_BOLD" "$C_RST"
		printf '%s║        zcode2api 管理脚本（Linux）                        ║%s\n' "$C_BOLD" "$C_RST"
		printf '%s╚══════════════════════════════════════════════════════════╝%s\n' "$C_BOLD" "$C_RST"

		local bin_state="未安装"
		[ -x "$DIR/zcode2api" ] && bin_state="已安装 $(installed_version | grep . || echo '')"
		local svc_state=""
		if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT_PATH" ]; then
			svc_state=" [$(systemctl is-active zcode2api.service 2>/dev/null || echo unknown)]"
		fi
		local dk_state="Docker 未安装"
		command -v docker >/dev/null 2>&1 && dk_state="Docker 已安装"

		printf '  二进制: %s%s\n' "$bin_state" "$svc_state"
		printf '  %s\n' "$dk_state"
		echo
		printf '  %s1)%s 安装二进制          %s5)%s Docker 安装（未验证）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s2)%s 更新二进制          %s6)%s Docker 更新\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s3)%s 卸载二进制          %s7)%s Docker 卸载\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s4)%s 查看状态            %s8)%s 服务控制（启停/重启/日志）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s9)%s 扫描已有安装        %s10)%s 迁移到标准目录\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
		printf '  %s0)%s 退出\n' "$C_CYAN" "$C_RST"
		echo
		local choice
		printf '请选择 [0-10]: '
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
			*) warn "无效选择: $choice" ;;
		esac
		echo
		printf '按回车返回菜单...'
		read -r _ || break
	done
}

menu_install() {
	require_root
	echo
	printf '安装方式:  %s1)%s 从 Releases 下载    %s2)%s 从源码构建（需 Go）\n' "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
	local mode
	printf '请选择 [1]: '
	read -r mode || return 0
	case "$mode" in
		2) SOURCE="local" ;;
		*) SOURCE="release" ;;
	esac

	printf '监听端口 [%s]: ' "$PORT"
	local p; read -r p || true
	[ -n "$p" ] && PORT="$p"
	validate_port

	printf '监听地址 [%s]（反向代理后填 127.0.0.1）: ' "$HOST"
	local h; read -r h || true
	[ -n "$h" ] && HOST="$h"
	validate_host

	printf '运行账号 [root]: '
	local u; read -r u || true
	[ -n "$u" ] && RUN_USER="$u"

	if confirm "启用验证码浏览器自动求解（会安装浏览器依赖）？"; then
		ENABLE_BROWSER="true"
	else
		ENABLE_BROWSER="false"
	fi
	if [ "$ENABLE_BROWSER" = "true" ]; then
		# 默认预下载：否则新机首次 JWT 请求需等待下载（约 200MB），
		# 期间该请求会因验证码不可用而失败。
		confirm "现在预下载补丁 Chromium（约 200MB）？" || PREFETCH_BROWSER="false"
	fi
	echo
	bin_install
}

menu_uninstall() {
	require_root
	echo
	printf '  %s1)%s 仅移除服务与二进制（保留数据）\n' "$C_CYAN" "$C_RST"
	printf '  %s2)%s 全部删除（含账号库与浏览器缓存）\n' "$C_CYAN" "$C_RST"
	printf '  %s0)%s 返回\n' "$C_CYAN" "$C_RST"
	local c
	printf '请选择 [0]: '
	read -r c || return 0
	case "$c" in
		1) PURGE="false"; bin_uninstall ;;
		2)
			PURGE="true"
			warn "此操作将永久删除 $DIR（含账号凭证）"
			confirm "确认全部删除？" && bin_uninstall || info "已取消"
			;;
		*) info "已取消" ;;
	esac
}

menu_docker_uninstall() {
	docker_preflight
	echo
	printf '  %s1)%s 移除容器（保留数据卷）\n' "$C_CYAN" "$C_RST"
	printf '  %s2)%s 移除容器并删除数据卷\n' "$C_CYAN" "$C_RST"
	printf '  %s0)%s 返回\n' "$C_CYAN" "$C_RST"
	local c
	printf '请选择 [0]: '
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
	printf '  %s1)%s 启动    %s2)%s 停止    %s3)%s 重启    %s4)%s 实时日志\n' \
		"$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST" "$C_CYAN" "$C_RST"
	local c
	printf '请选择 [0]: '
	read -r c || return 0
	case "$c" in
		1) systemctl start zcode2api.service && ok "已启动" ;;
		2) systemctl stop zcode2api.service && ok "已停止" ;;
		3) systemctl restart zcode2api.service && ok "已重启" ;;
		4) info "按 Ctrl+C 退出日志"; journalctl -u zcode2api -f ;;
		*) info "已取消" ;;
	esac
}

# ── 用法与分发 ──────────────────────────────────────────────────────────────
usage() {
	cat <<EOF
${C_BOLD}zcode2api 管理脚本（Linux）${C_RST}

用法:
  sudo $0                    交互式菜单
  sudo $0 <命令> [选项]      非交互执行

命令:
  install            安装二进制（+ systemd 服务）
  update             更新二进制（比对 Release 版本）
  uninstall          卸载二进制
  status             查看安装状态
  scan               扫描 /opt 下已有的本程序安装（含手工编译的）
  adopt              把手工部署的目录纳入管理（--dir 指定）
  migrate            接管 + 部署最新 + 迁移数据（--dir 指定源目录）
  docker-install     Docker 安装（参考实现，未验证）
  docker-update      Docker 更新
  docker-uninstall   Docker 卸载
  help               显示本说明

选项:
  --dir DIR           安装目录（默认 $DIR）
  --port PORT         监听端口（默认 $PORT）
  --host ADDR         监听地址（默认 $HOST；反向代理后建议 127.0.0.1）
  --user USER         以该账号运行（默认 root；不存在则自动创建）
  --local             从本机源码构建（需 Go 工具链）
  --version TAG       指定 Release 标签（如 v1.2.3），默认取最新
  --no-browser        不安装验证码浏览器依赖（改用后台人工回填）
  --no-prefetch-browser  安装时不预下载补丁 Chromium（默认会下载，约 200MB）
  --no-deps           跳过系统依赖安装
  --purge             卸载时连同数据目录一并删除
  --keep-user         卸载时保留系统账号
  --volumes           Docker 卸载时连同数据卷一并删除
  -y, --yes           所有确认自动回答 yes（非交互场景）
  -h, --help          显示本说明

示例:
  sudo $0 install --port 3010 --user zcode
  sudo $0 update -y
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
			--dir) ADOPT_DIR="${2:?--dir 需要路径}"; shift 2 ;;
			--docker-dir) DOCKER_DIR="${2:?--docker-dir 需要路径}"; shift 2 ;;
			--port) PORT="${2:?--port 需要端口}"; shift 2 ;;
			--host) HOST="${2:?--host 需要地址}"; shift 2 ;;
			--user) RUN_USER="${2:?--user 需要账号名}"; shift 2 ;;
			--version) VERSION="${2:?--version 需要标签}"; shift 2 ;;
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
			*) die "未知参数: $1（help 查看用法）" ;;
		esac
	done
}

main() {
	parse_args "$@"

	# --dir 的语义随命令而变：adopt / migrate 指的是「要操作的既有目录」，
	# 其余命令指的是「本脚本的安装目录」。
	# 不能对 adopt / migrate 也赋给 DIR：它们内部靠 src = DIR 判断源是否已是标准目录，
	# 一旦 DIR 被改成源目录，就会误判为原地升级而放弃搬迁。
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
