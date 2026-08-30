# zcode2api-plus — Python(FastAPI) + Node(jsdom 无痕验证求解器) + cloakbrowser(真实浏览器运行时)
# 运行期同时需要 Python 与 Node：网关用 Python，验证码求解以 Node 子进程方式运行；
# cloakbrowser 提供真实 Chromium（与生产 Python 3.11 对齐；zcode-proxy 需 >=3.13 故不安装）。
FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    ZCODE_HOST=0.0.0.0 \
    ZCODE_PORT=3000 \
    ZCODE_DATA_DIR=/data \
    ZCODE_NODE_PATH=node \
    ZCODE_CAPTCHA_BROWSER=true \
    # cloakbrowser：浏览器二进制缓存到显式可写路径，禁止自动更新以免运行期意外换版本
    CLOAKBROWSER_CACHE_DIR=/opt/cloakbrowser-cache \
    CLOAKBROWSER_AUTO_UPDATE=false \
    CLOAKBROWSER_SUPPRESS_FONT_WARNING=1

WORKDIR /app

# ── Node.js（供无浏览器无痕验证求解器使用）──────────────────────────────────
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get purge -y curl gnupg \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# ── cloakbrowser（真实 Chromium）所需 Debian 运行库 ──────────────────────────
# 清单与隔离审计 temp/cloakbrowser-probe-a/Dockerfile 一致（Debian bookworm 已验证可启动）。
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation \
        libasound2 \
        libatk-bridge2.0-0 \
        libatk1.0-0 \
        libc6 \
        libcairo2 \
        libcups2 \
        libdbus-1-3 \
        libdrm2 \
        libexpat1 \
        libfontconfig1 \
        libgbm1 \
        libglib2.0-0 \
        libgtk-3-0 \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libx11-6 \
        libx11-xcb1 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
        xdg-utils \
    && rm -rf /var/lib/apt/lists/*

# ── Python 依赖（独立分层，便于缓存）────────────────────────────────────────
COPY requirements.txt ./
RUN pip install -r requirements.txt

# ── cloakbrowser：构建期预下载 Chromium 二进制到 CLOAKBROWSER_CACHE_DIR ──────
# 目录显式可写（root 属主 + 777），运行期可按需落盘；预下载使启动离线可用。
RUN mkdir -p /opt/cloakbrowser-cache && chmod 777 /opt/cloakbrowser-cache \
    && python -m cloakbrowser install

# ── 求解器 Node 依赖（独立分层）─────────────────────────────────────────────
COPY captcha_node/package.json captcha_node/package-lock.json ./captcha_node/
RUN cd captcha_node && npm ci --omit=dev

# ── 应用源码 ────────────────────────────────────────────────────────────────
COPY . .

# 账号 / 设置持久化目录（建议挂载到宿主机卷）
VOLUME ["/data"]
EXPOSE 3000

CMD ["python", "main.py", "serve"]
