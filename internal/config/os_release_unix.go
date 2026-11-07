//go:build !windows

// OsRelease 的非 Windows 探测实现。
package config

import (
	"os"
	"runtime"
	"strings"
)

// probeOsRelease 对齐 Node os.release()：linux 返回内核版本（uname -r）；
// 其余类 Unix 平台官方产物不覆盖，返回空串（调用方跳过 X-Os-Version 头）。
func probeOsRelease() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
