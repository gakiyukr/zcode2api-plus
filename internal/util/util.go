// Package util 收录跨包复用的无依赖工具函数。
//
// 这些函数原本在各包里各有一份逐字相同的私有副本（marshalJSON 四份、
// newUUID 三份、randomHex 三份……）。副本会各自演化：改动其中一处而漏掉
// 另一处，就会让同一份数据在不同路径上被序列化/截断成不同形态。
// 这里只放纯函数，不引用任何 internal 包，因此任何包都可以安全依赖它。
package util

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MarshalJSON 序列化 JSON，不转义 HTML 字符（与 Python 版 ensure_ascii 语义对齐），
// 并去掉 Encoder 追加的尾随换行。
//
// 用 Encoder 而非 json.Marshal 是为了 SetEscapeHTML(false)：后者会把
// <、>、& 转成 \u003c 等，与 Python 版输出不一致，导致字节级透传的契约失效。
func MarshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// NewUUID 生成 UUIDv4（不引入第三方依赖）。
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UUIDv5 以命名空間與種子派生穩定的 UUID v5 形態（SHA-256 截斷，無第三方依賴）。
func UUIDv5(namespace, seed string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + seed))
	u := sum[:16]
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// IsUUID 判斷字串是否為標準 36 字元連字號 UUID（8-4-4-4-12 十六進位）。
func IsUUID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// RandomHex 生成 n 字节的随机十六进制串（长度 2n）。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomTokenURLSafe 生成 n 字节随机数的 base64url 串（无填充）。
func RandomTokenURLSafe(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Truncate 截断字符串到最多 n 字节；未超长时原样返回。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// OrDefault 返回 s，为空时返回 fallback。
func OrDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// EpochSeconds 把时间转为 Unix 秒的浮点表示。
//
// 项目里的账号时间字段（created_at/last_used_at/cooling_until 等）统一用
// 这个形态存储，与 Python 版 time.time() 一致。
func EpochSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
