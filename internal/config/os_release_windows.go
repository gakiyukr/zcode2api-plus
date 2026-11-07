//go:build windows

// OsRelease 的 Windows 探测实现。
package config

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// probeOsRelease 对齐 Node os.release() 的 Windows 形态：NT 版本号（10.0.26300）。
func probeOsRelease() string {
	v := windows.RtlGetVersion()
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
