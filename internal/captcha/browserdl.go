// cloakbrowser Chromium 二进制自动下载：新机器无需 Python 预下载，发现链
// 落空时自动拉取补丁二进制到 CLOAKBROWSER_CACHE_DIR（对齐 cloakbrowser
// download.py 的 URL 拼装与校验语义：SHA256SUMS + Ed25519 签名，篡改即失败
// 不静默回退；主站不可达回退 GitHub Releases）。
package captcha

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"zcode2api/internal/web"
)

const (
	// 与 cloakbrowser 0.5.10 的 PLATFORM_CHROMIUM_VERSIONS 对齐。
	cloakVersionLinuxX64    = "146.0.7680.177.5"
	cloakVersionLinuxARM64  = "146.0.7680.177.3"
	cloakVersionDarwin      = "145.0.7632.109.2"
	cloakVersionWindowsX64  = "146.0.7680.177.5"
	cloakDownloadBase       = "https://cloakbrowser.dev"
	cloakGitHubDownloadBase = "https://github.com/CloakHQ/cloakbrowser/releases/download"
	downloadTimeout         = 10 * time.Minute
)

// cloakSigningPubkeyDefault cloakbrowser config.BINARY_SIGNING_PUBKEYS
// （Ed25519，base64 raw 32B）；var 便于测试注入临时密钥。
var (
	cloakSigningPubkeyDefault = "MKFKwIhUcKWq5xTuNA0Ovg99njcDEcEJvmWYYhApvaU="
	cloakSigningPubkey        = &cloakSigningPubkeyDefault
)

// downloadMutex 串行化并发下载（多 worker 同时冷启动只下载一次）。
//
// 用 channel 而非 sync.Mutex：等待方需要能被调用方的 ctx 取消。首次下载
// 约 200MB、上限 downloadTimeout（10 分钟），而调用方的启动超时通常只有
// 90s——不可取消的等待会让槽位 goroutine 在调用方早已放弃后继续被扣住，
// cm.Close() 也取消不掉。
var downloadMutex = make(chan struct{}, 1)

// lockDownload 获取下载锁，等待可被 ctx 取消。
func lockDownload(ctx context.Context) error {
	select {
	case downloadMutex <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlockDownload() { <-downloadMutex }

// cloakBinaryDir 版本对应的安装目录（对齐 cloakbrowser get_binary_dir）。
func cloakBinaryDir(version string) string {
	cacheDir := strings.TrimSpace(os.Getenv("CLOAKBROWSER_CACHE_DIR"))
	if cacheDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cacheDir = filepath.Join(home, ".cloakbrowser")
		} else {
			cacheDir = ".cloakbrowser"
		}
	}
	return filepath.Join(cacheDir, "chromium-"+version)
}

// platformTag 返回 cloakbrowser 平台标签（不支持的平台报错，对齐 check_platform_available）。
func platformTag() (string, error) {
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "amd64" {
			return "linux-x64", nil
		}
		if runtime.GOARCH == "arm64" {
			return "linux-arm64", nil
		}
	case "darwin":
		if runtime.GOARCH == "arm64" || runtime.GOARCH == "amd64" {
			return "darwin-" + runtime.GOARCH, nil
		}
	case "windows":
		if runtime.GOARCH == "amd64" {
			return "windows-x64", nil
		}
	}
	return "", fmt.Errorf("平台 %s/%s 无预构建 Chromium", runtime.GOOS, runtime.GOARCH)
}

// cloakChromiumVersion 当前平台对应的补丁 Chromium 版本。
func cloakChromiumVersion() (string, error) {
	tag, err := platformTag()
	if err != nil {
		return "", err
	}
	switch tag {
	case "linux-x64":
		return cloakVersionLinuxX64, nil
	case "linux-arm64":
		return cloakVersionLinuxARM64, nil
	case "darwin-arm64", "darwin-x64":
		return cloakVersionDarwin, nil
	default:
		return cloakVersionWindowsX64, nil
	}
}

// downloadBaseURL 主站下载源（CLOAKBROWSER_DOWNLOAD_URL 可覆盖，对齐 cloakbrowser）。
func downloadBaseURL() string {
	if u := strings.TrimSpace(os.Getenv("CLOAKBROWSER_DOWNLOAD_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return cloakDownloadBase
}

// fetchBytes 拉取 URL，非 200 报错。
func fetchBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, url)
	}
	return io.ReadAll(res.Body)
}

// verifySHA256SUMS 校验 SHA256SUMS 的 Ed25519 签名并返回摘要表。
// 实测 .sig 文件为 base64 编码的签名（88 字节文本 = 64 字节原始签名），
// 兼容直接存原始字节的形态；签名无效视作篡改信号：直接失败，绝不降级
// 使用未校验的哈希表。
func verifySHA256SUMS(sums, sigFile []byte) (map[string]string, error) {
	pub, err := base64.StdEncoding.DecodeString(*cloakSigningPubkey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("内置校验公钥非法")
	}
	sig, b64Err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if b64Err != nil || len(sig) != ed25519.SignatureSize {
		sig = sigFile // 回退：按原始字节签名处理
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), sums, sig) {
		return nil, fmt.Errorf("SHA256SUMS 签名校验失败（下载源被篡改或密钥已轮换）")
	}
	table := make(map[string]string)
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		table[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	return table, nil
}

// EnsureBrowserBinary 保证缓存目录中存在补丁 Chromium，返回其版本号。
// 已存在直接返回；否则下载 → 验签 → 校验哈希 → 原子解包。
func EnsureBrowserBinary(ctx context.Context) (string, error) {
	version, err := cloakChromiumVersion()
	if err != nil {
		return "", err
	}
	tag, err := platformTag()
	if err != nil {
		return "", err
	}
	return ensureVersion(ctx, version, "cloakbrowser-"+tag+archiveExt())
}

// ensureVersion 指定版本与包名的下载安装链路（测试可注入假源）。
func ensureVersion(ctx context.Context, version, archiveName string) (string, error) {
	if err := lockDownload(ctx); err != nil {
		return "", err
	}
	defer unlockDownload()

	dir := cloakBinaryDir(version)
	if info, err := os.Stat(filepath.Join(dir, executableName())); err == nil && !info.IsDir() {
		return version, nil
	}

	web.Ok("captcha", "本地未发现补丁 Chromium，开始自动下载: v"+version)
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	client := &http.Client{Timeout: 0} // 总超时由 ctx 控制

	// 下载校验文件（主站 → GitHub 兜底）。
	sums, sig, err := fetchSumsWithFallback(ctx, client, version)
	if err != nil {
		return "", fmt.Errorf("校验文件下载失败: %w", err)
	}
	table, err := verifySHA256SUMS(sums, sig)
	if err != nil {
		return "", err
	}
	want, ok := table[archiveName]
	if !ok || want == "" {
		return "", fmt.Errorf("SHA256SUMS 中无 %s 的记录", archiveName)
	}

	// 下载压缩包（主站 → GitHub 兜底），校验 SHA256。
	archive, err := downloadArchiveWithFallback(ctx, client, version, archiveName)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("压缩包 SHA256 不符（预期 %s）", want)
	}

	// 原子解包：先写临时目录再改名，失败不留半成品。
	tmp := dir + ".tmp-" + fmt.Sprint(time.Now().UnixNano())
	if err := extractArchive(archive, archiveName, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("解包失败: %w", err)
	}
	if info, err := os.Stat(filepath.Join(tmp, executableName())); err != nil || info.IsDir() {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("压缩包内未找到 %s", executableName())
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("安装失败: %w", err)
	}
	web.Ok("captcha", "补丁 Chromium 就绪: "+dir)
	return version, nil
}

// fetchSumsWithFallback 下载 SHA256SUMS + 签名（主站 → GitHub）。
func fetchSumsWithFallback(ctx context.Context, client *http.Client, version string) (sums, sig []byte, err error) {
	for _, base := range []string{downloadBaseURL(), cloakGitHubDownloadBase} {
		sums, err = fetchBytes(ctx, client, base+"/chromium-v"+version+"/SHA256SUMS")
		if err != nil {
			continue
		}
		sig, err = fetchBytes(ctx, client, base+"/chromium-v"+version+"/SHA256SUMS.sig")
		if err == nil {
			return sums, sig, nil
		}
	}
	return nil, nil, err
}

// downloadArchiveWithFallback 下载压缩包到内存（主站 → GitHub 兜底）。
// 二进制约 200MB，一次性驻留内存可接受（下载本就是低频一次性操作）。
func downloadArchiveWithFallback(ctx context.Context, client *http.Client, version, archiveName string) ([]byte, error) {
	var lastErr error
	for _, base := range []string{downloadBaseURL(), cloakGitHubDownloadBase} {
		url := base + "/chromium-v" + version + "/" + archiveName
		data, err := fetchBytes(ctx, client, url)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("压缩包下载失败: %w", lastErr)
}

// archiveExt 压缩包后缀（对齐 get_archive_ext）。
func archiveExt() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// extractArchive 解包到 dest（tar.gz 保权限位；zip 统一 0o755）。
func extractArchive(archive []byte, archiveName, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(archiveName, ".zip") {
		return extractZip(archive, dest)
	}
	return extractTarGz(archive, dest)
}

func extractTarGz(archive []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("压缩包含非法路径: %s", hdr.Name)
		}
		target := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			if err := writeExecFile(target, tr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// macOS 的 Chromium.app bundle 以符号链接组织 Framework；
			// 丢弃它们会解出不可用的安装。链接目标须留在解包目录内。
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			link := filepath.Clean(hdr.Linkname)
			if filepath.IsAbs(link) || strings.HasPrefix(link, "..") {
				return fmt.Errorf("压缩包含非法符号链接: %s -> %s", hdr.Name, hdr.Linkname)
			}
			_ = os.Remove(target) // 覆盖既有条目（重复解包时）
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("创建符号链接失败 %s: %w", hdr.Name, err)
			}
		case tar.TypeLink:
			// 硬链接：解包内相对路径，直接复制内容而非建链（跨设备安全）
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Linkname 必须与 Name 一样做逃逸校验：filepath.Join 会 Clean 掉
			// ".."，../../../../etc/passwd 会解析成解包目录之外的路径，
			// 使 os.ReadFile 读到宿主任意文件并写进安装目录。
			link := filepath.Clean(hdr.Linkname)
			if filepath.IsAbs(link) || strings.HasPrefix(link, "..") {
				return fmt.Errorf("压缩包含非法硬链接: %s -> %s", hdr.Name, hdr.Linkname)
			}
			src := filepath.Join(dest, link)
			data, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("硬链接源不可读 %s: %w", hdr.Name, err)
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return err
			}
		default:
			// 其余类型（FIFO、设备节点等）不应出现在发行包中，记录而非静默丢弃
			web.Warn("captcha", fmt.Sprintf("压缩包含忽略的条目类型 %d: %s", hdr.Typeflag, hdr.Name))
		}
	}
}

func extractZip(archive []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("压缩包含非法路径: %s", f.Name)
		}
		target := filepath.Join(dest, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeExecFile(target, rc, 0o755)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeExecFile 写文件并保证可执行位（Chromium 二进制需要）。
func writeExecFile(target string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o111)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
