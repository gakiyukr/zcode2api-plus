// browserdl 单测：httptest 假下载源 + 临时生成的 Ed25519 密钥签发 SHA256SUMS，
// 全程离线验证下载 → 验签 → 哈希 → 解包链路；篡改用例确保校验不被静默绕过。
package captcha

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCloakSource 假下载源：内置私钥签发的 SHA256SUMS 与压缩包。
type fakeCloakSource struct {
	srv      *httptest.Server
	sums     []byte
	sig      []byte
	archive  []byte
	archiveN string
}

func newFakeSource(t *testing.T, files map[string]string) *fakeCloakSource {
	t.Helper()
	// 打包假 tar.gz（根目录放 files）。
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()

	// 临时 Ed25519 密钥签发 SHA256SUMS（覆盖包级公钥变量）。
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fakePubKey := base64.StdEncoding.EncodeToString(pub)
	oldPub := cloakSigningPubkey
	cloakSigningPubkey = &fakePubKey
	t.Cleanup(func() { cloakSigningPubkey = oldPub })

	sumLines := hex.EncodeToString(func() []byte { s := sha256.Sum256(archive); return s[:] }()) + "  " + fakeArchiveName + "\n"
	sums := []byte(sumLines)
	// 真实 .sig 形态：base64 编码的签名文本。
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n")

	s := &fakeCloakSource{sums: sums, sig: sig, archive: archive, archiveN: fakeArchiveName}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write(s.sums)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sig"):
			_, _ = w.Write(s.sig)
		case strings.HasSuffix(r.URL.Path, "/"+s.archiveN):
			_, _ = w.Write(s.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

const (
	fakeArchiveName = "cloakbrowser-fake.tar.gz"
	fakeVersion     = "9.9.9.9.9"
)

func setupFakeEnv(t *testing.T, src *fakeCloakSource) {
	t.Helper()
	oldBase, oldCache := os.Getenv("CLOAKBROWSER_DOWNLOAD_URL"), os.Getenv("CLOAKBROWSER_CACHE_DIR")
	if err := os.Setenv("CLOAKBROWSER_DOWNLOAD_URL", src.srv.URL); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if err := os.Setenv("CLOAKBROWSER_CACHE_DIR", cache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("CLOAKBROWSER_DOWNLOAD_URL", oldBase)
		_ = os.Setenv("CLOAKBROWSER_CACHE_DIR", oldCache)
	})
}

func forcePlatform(t *testing.T) {
	// 单测只验证下载链路本身，不依赖真实平台 tag 与版本常量。
	t.Helper()
}

func TestEnsureBrowserBinaryDownloadsAndExtracts(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{executableName(): "#!/bin/sh\necho fake\n", "lib.so": "lib"})
	setupFakeEnv(t, src)

	// 直接驱动内部链路（与平台解耦：手工指定版本与包名）。
	dir := cloakBinaryDir(fakeVersion)
	version, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName)
	if err != nil {
		t.Fatalf("下载链路应成功: %v", err)
	}
	if version != fakeVersion {
		t.Fatalf("版本不符: %s", version)
	}
	bin := filepath.Join(dir, executableName())
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		t.Fatalf("chrome 应已安装: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatal("chrome 应有可执行位")
	}
	data, _ := os.ReadFile(bin)
	if !strings.Contains(string(data), "echo fake") {
		t.Fatalf("内容不符: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib.so")); err != nil {
		t.Fatalf("附属文件应解包: %v", err)
	}

	// 幂等：第二次直接命中本地，不发请求。
	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err != nil {
		t.Fatalf("二次调用应直接返回: %v", err)
	}
}

func TestEnsureRejectsTamperedArchive(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{"chrome": "clean"})
	setupFakeEnv(t, src)
	// 篡改压缩包内容（SHA256SUMS 不再匹配）。
	src.archive = append(src.archive, byte(0x00))

	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err == nil {
		t.Fatal("哈希不符应失败")
	} else if !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("应报哈希错误: %v", err)
	}
}

func TestEnsureRejectsBadSignature(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{"chrome": "clean"})
	setupFakeEnv(t, src)
	// 伪造签名（另一把密钥）。
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	src.sig = ed25519.Sign(other, src.sums)

	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err == nil {
		t.Fatal("签名校验失败应报错")
	} else if !strings.Contains(err.Error(), "签名") {
		t.Fatalf("应报签名错误: %v", err)
	}
	// 校验失败不得落盘半成品。
	if _, err := os.Stat(cloakBinaryDir(fakeVersion)); !os.IsNotExist(err) {
		t.Fatal("失败时不应安装目录")
	}
}

// macOS 的 Chromium.app bundle 以符号链接组织 Framework：
// 解包必须保留 symlink，否则解出的安装不可用。
func TestExtractTarGzPreservesSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		hdr  *tar.Header
		body string
	}{
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/", Typeflag: tar.TypeDir, Mode: 0o755}, ""},
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/Chromium", Mode: 0o755, Size: 4}, "bin\n"},
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/Current", Typeflag: tar.TypeSymlink, Linkname: "Chromium", Mode: 0o777}, ""},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTarGz(buf.Bytes(), dest); err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	link := filepath.Join(dest, "Chromium.app", "Contents", "MacOS", "Current")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("符号链接应存在: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("应保留为符号链接而非普通文件: %v", info.Mode())
	}
	if target, _ := os.Readlink(link); target != "Chromium" {
		t.Fatalf("链接目标不符: %q", target)
	}
}

// 逃逸解包目录的符号链接必须被拒绝（压缩包投毒防护）。
func TestExtractTarGzRejectsEscapingSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("逃逸目录的符号链接应被拒绝")
	}
}

// 逃逸解包目录的硬链接必须被拒绝（与符号链接同级的压缩包投毒防护）。
//
// 硬链接分支曾只做 filepath.Clean 而缺少逃逸校验：filepath.Join 会把
// "../../etc/passwd" Clean 成解包目录之外的路径，使 os.ReadFile 读到宿主
// 任意文件并写进安装目录——与同函数中 Name / TypeSymlink 的防护不一致。
func TestExtractTarGzRejectsEscapingHardlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "evil", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd", Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	// 必须因「路径非法」被拒，而不是因源文件恰好读不到——后者在源路径存在时
	// 就会放行（例如宿主的 /etc/passwd），测试也就抓不到这个缺口。
	err := extractTarGz(buf.Bytes(), t.TempDir())
	if err == nil {
		t.Fatal("逃逸目录的硬链接应被拒绝")
	}
	if !strings.Contains(err.Error(), "非法硬链接") {
		t.Fatalf("应以路径非法为由拒绝，实际: %v", err)
	}
}

// 解包目录内的硬链接仍应正常复制内容（不能因防护而破坏合法包）。
func TestExtractTarGzAllowsInternalHardlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("binary-content")
	if err := tw.WriteHeader(&tar.Header{Name: "dir/src", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "dir/link", Typeflag: tar.TypeLink, Linkname: "dir/src", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTarGz(buf.Bytes(), dest); err != nil {
		t.Fatalf("包内硬链接应被允许: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dir", "link"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("硬链接内容不符: %q", got)
	}
}

// 等待下载锁必须可被调用方 ctx 取消。
//
// 首次下载约 200MB、上限 downloadTimeout（10 分钟），而调用方的启动超时通常
// 只有 90s。锁若是普通 sync.Mutex，调用方放弃后槽位 goroutine 仍被扣住，
// cm.Close() 也取消不掉。
func TestEnsureVersionDownloadLockIsCancellable(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{executableName(): "fake-binary"})
	setupFakeEnv(t, src)

	// 先占住下载锁，模拟另一个 worker 正在下载
	if err := lockDownload(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer unlockDownload()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ensureVersion(ctx, fakeVersion, fakeArchiveName)
		done <- err
	}()

	// 给等待方一点时间进入 lockDownload，再取消
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("等待下载锁应返回 ctx 取消，实际: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待下载锁不可取消（调用方放弃后仍被扣住）")
	}
}
