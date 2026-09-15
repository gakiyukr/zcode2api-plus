# zcode2api Docker 映像（Linux）。
#
# ⚠️ 未經驗證：本檔從未經 `docker build` 實測，不保證可構建或運行，請自行驗證與調整。
#
# 本映像**在本地/自有服務器上構建**，不依賴 CI 預編譯產物：
#   docker build -t zcode2api:latest .
#   docker build -t zcode2api:latest --build-arg PREFETCH_BROWSER=true .   # 預下載 Chromium
#
# 前端 dist 已入庫並由 go:embed 打包，構建階段無需 Node。

# ── 構建階段 ────────────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder

WORKDIR /src

# 先複製依賴清單，利用層緩存（依賴不變時不重複下載模組）
COPY go.mod go.sum ./
RUN go mod download

# 複製源碼（.dockerignore 已排除 data/、.git/ 等）
COPY . .

# 純靜態編譯：CGO 關閉後二進制不依賴 glibc，運行階段僅需 Chromium 的圖形庫
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-amd64}" \
    go build -trimpath -ldflags="-s -w" -o /out/zcode2api ./cmd/zcode2api

# ── 運行階段 ────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

LABEL org.opencontainers.image.title="zcode2api" \
      org.opencontainers.image.description="Z.AI ZCode Coding Plan → OpenAI/Anthropic 兼容网关" \
      org.opencontainers.image.source="https://github.com/gakiyukr/zcode2api-plus"

# 驗證碼瀏覽器（cloakbrowser 補丁 Chromium）所需的共享庫。
# 清單按 linux-x64 官方二進制的 ldd 實測結果整理；t64 後綴為 Ubuntu 24.04+ 命名，
# 本映像為 Debian bookworm（無 t64），故使用無後綴名。
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        tzdata \
        libasound2 \
        libatk1.0-0 \
        libatk-bridge2.0-0 \
        libatspi2.0-0 \
        libavahi-client3 \
        libavahi-common3 \
        libcairo2 \
        libcups2 \
        libdatrie1 \
        libdrm2 \
        libfontconfig1 \
        libfreetype6 \
        libfribidi0 \
        libgbm1 \
        libglib2.0-0 \
        libgraphite2-3 \
        libharfbuzz0b \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libpixman-1-0 \
        libpng16-16 \
        libthai0 \
        libx11-6 \
        libxau6 \
        libxcb1 \
        libxcb-render0 \
        libxcb-shm0 \
        libxcomposite1 \
        libxdamage1 \
        libxdmcp6 \
        libxext6 \
        libxfixes3 \
        libxi6 \
        libxkbcommon0 \
        libxrandr2 \
        libxrender1 \
        libvulkan1 \
        fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

# 容器內以 root 運行：Chromium 啟動參數已硬編碼 --no-sandbox（見 internal/captcha/solve.go），
# 無沙箱可失去；且可避免綁定掛載卷的 UID 不匹配問題（自架部署最常見的坑）。
WORKDIR /app

COPY --from=builder /out/zcode2api /app/zcode2api

ENV ZCODE_HOST=0.0.0.0 \
    ZCODE_PORT=3000 \
    ZCODE_DATA_DIR=/app/data \
    CLOAKBROWSER_CACHE_DIR=/app/browser

# 可選：構建時預下載補丁 Chromium（約 200MB，映像變大但首次啟動即用）。
# 不預下載時，首次啟動會自動下載到 CLOAKBROWSER_CACHE_DIR（需掛載持久卷，否則每次重建都重下）。
ARG PREFETCH_BROWSER=false
RUN if [ "$PREFETCH_BROWSER" = "true" ]; then \
        ZCODE_CAPTCHA_BROWSER=true ZCODE_CAPTCHA_BROWSER_BIN="" \
        /app/zcode2api accounts >/dev/null 2>&1 || \
        echo "warn: 預下載瀏覽器未成功，將於首次啟動時重試" >&2; \
    fi

VOLUME ["/app/data", "/app/browser"]

EXPOSE 3000

# 健康檢查：/meta 為無需鑑權的版本端點
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://127.0.0.1:${ZCODE_PORT}/meta || exit 1

CMD ["/app/zcode2api", "serve"]
