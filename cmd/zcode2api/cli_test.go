package main

import "testing"

func TestCLI_ErrorExitCode(t *testing.T) {
	if code := runCLI("login", nil, nil); code != 1 {
		t.Fatalf("login 缺少參數應回傳退出碼 1，實際: %d", code)
	}
	if code := runCLI("unknown-cmd", nil, nil); code != 1 {
		t.Fatalf("未知子命令應回傳退出碼 1，實際: %d", code)
	}
	if code := runCLI("add-account", nil, nil); code != 1 {
		t.Fatalf("add-account 缺少參數應回傳退出碼 1，實際: %d", code)
	}
	if code := runCLI("set-admin-key", []string{"   "}, nil); code != 1 {
		t.Fatalf("set-admin-key 空白密鑰應回傳退出碼 1，實際: %d", code)
	}
	if code := runCLI("import", nil, nil); code != 1 {
		t.Fatalf("import 缺少檔名應回傳退出碼 1，實際: %d", code)
	}
}
