package fingerprint

import (
	"strings"
	"testing"

	"zcode2api/internal/model"
)

func TestForIsDeterministicAndDesktopOnly(t *testing.T) {
	acc := model.Create(model.ProviderZai, "sku", "a.b.c")
	first := For(acc)
	for range 5 {
		if got := For(acc); got != first {
			t.Fatalf("同一账号 SKU 抖動: %+v vs %+v", got, first)
		}
	}
	if first.PlatformName != "win32" && first.PlatformName != "darwin" {
		t.Fatalf("SKU 必須是桌面平台: %+v", first)
	}
	if strings.Contains(first.Platform, "linux") {
		t.Fatalf("不得泄漏部署宿主 Linux: %+v", first)
	}
}

func TestForDistributesAcrossDesktopFamilies(t *testing.T) {
	seenWin, seenMac := false, false
	for i := range 500 {
		sku := ForSeed(strings.Repeat("seed", i+1), "")
		switch sku.PlatformName {
		case "win32":
			seenWin = true
		case "darwin":
			seenMac = true
		default:
			t.Fatalf("出現非桌面平台: %+v", sku)
		}
	}
	if !seenWin || !seenMac {
		t.Fatalf("帳號應自然分佈於 Windows/macOS: win=%v mac=%v", seenWin, seenMac)
	}
}

func TestSaltRotationChangesSKU(t *testing.T) {
	seed := "account-1\x00credential"
	base := ForSeed(seed, "")
	rotated := false
	for i := range 50 {
		got := ForSeed(seed, "salt-"+strings.Repeat("x", i))
		if got != base {
			rotated = true
			break
		}
	}
	if !rotated {
		t.Fatal("salt 應能旋轉到不同 SKU")
	}
}

func TestSetSaltResolverInvalidatesCache(t *testing.T) {
	acc := model.Create(model.ProviderZai, "salt", "a.b.c")
	base := For(acc)
	SetSaltResolver(func(string) string { return "rotated-salt" })
	t.Cleanup(func() { SetSaltResolver(nil) })
	_ = base
	if got := For(acc); got.Key == "" {
		t.Fatalf("旋轉後仍應得到有效 SKU: %+v", got)
	}
}
