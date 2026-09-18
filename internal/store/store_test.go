package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// openAt 在指定路径打开存储（隔离环境变量，测试结束恢复并关闭）。
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = path
	config.AdminKeyEnv = ""
	config.GatewayKeyEnv = ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})
	s, err := New()
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestStore(t *testing.T) *Store {
	return openAt(t, filepath.Join(t.TempDir(), "accounts.db"))
}

func TestBootstrapGeneratesKeys(t *testing.T) {
	s := newTestStore(t)
	if s.AdminKey() == "" || s.AdminKey() == "zcode" {
		t.Fatalf("应随机生成后台密码: %q", s.AdminKey())
	}
	if s.GeneratedAdminKey != s.AdminKey() {
		t.Fatalf("生成标记应记录: %q vs %q", s.GeneratedAdminKey, s.AdminKey())
	}
	if !strings.HasPrefix(s.GatewayKey(), "sk-") {
		t.Fatalf("网关密钥应 sk- 前缀: %q", s.GatewayKey())
	}
	if s.GeneratedGatewayKey != s.GatewayKey() {
		t.Fatalf("网关生成标记应记录")
	}
}

func TestLegacyAdminKeyRotated(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "zcode"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("gateway_key", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New()
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if s2.AdminKey() == "" || s2.AdminKey() == "zcode" {
		t.Fatalf("历史默认密码应强制轮换: %q", s2.AdminKey())
	}
	if s2.GeneratedAdminKey != s2.AdminKey() {
		t.Fatal("轮换应标记为随机生成")
	}
	if s2.GatewayKey() == "" || s2.GeneratedGatewayKey != s2.GatewayKey() {
		t.Fatal("空网关密钥应重新生成")
	}
}

func TestEnvKeysRespected(t *testing.T) {
	dir := t.TempDir()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = filepath.Join(dir, "accounts.db")
	config.AdminKeyEnv = "env-admin-key"
	config.GatewayKeyEnv = "sk-env-gateway"
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})

	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.AdminKey() != "env-admin-key" || s.GeneratedAdminKey != "" {
		t.Fatalf("环境变量应生效且不标记生成: %q %q", s.AdminKey(), s.GeneratedAdminKey)
	}
	if s.GatewayKey() != "sk-env-gateway" || s.GeneratedGatewayKey != "" {
		t.Fatalf("网关环境变量应生效: %q %q", s.GatewayKey(), s.GeneratedGatewayKey)
	}
}

func TestCustomKeysPreservedAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "my-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("gateway_key", "sk-my-gw"); err != nil {
		t.Fatal(err)
	}
	path := config.DBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 在同一路径重开：自定义密钥不受影响，也不触发轮换
	oldDB := config.DBPath
	config.DBPath = path
	t.Cleanup(func() { config.DBPath = oldDB })
	s2, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if s2.AdminKey() != "my-secret" || s2.GatewayKey() != "sk-my-gw" {
		t.Fatalf("自定义密钥应保留: %q %q", s2.AdminKey(), s2.GatewayKey())
	}
	if s2.GeneratedAdminKey != "" || s2.GeneratedGatewayKey != "" {
		t.Fatal("不应重新生成")
	}
}

func TestAccountCRUDAndDedup(t *testing.T) {
	s := newTestStore(t)
	acc1, err := s.AddAccount(model.ProviderZai, "main", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	acc2, err := s.AddAccount(model.ProviderZai, "dup", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	if acc1.ID != acc2.ID {
		t.Fatal("重复 token 应返回既有账号")
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("应只有 1 个账号: %d", got)
	}
	if _, err := s.AddAccount("other", "x", "k"); err == nil {
		t.Fatal("未知 provider 应报错")
	}

	if ok, err := s.RemoveAccount(model.ProviderZai, "main"); !ok || err != nil {
		t.Fatalf("删除失败: %v %v", ok, err)
	}
	if ok, _ := s.RemoveAccount(model.ProviderZai, "main"); ok {
		t.Fatal("重复删除应返回 false")
	}
}

func TestSetEnabled(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "acc", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, false); !ok || err != nil {
		t.Fatalf("禁用失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("禁用后状态错误: %+v", got)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("启用失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || !got.Enabled {
		t.Fatalf("启用后状态错误: %+v", got)
	}
}

func TestSelectRotationAndModelFilter(t *testing.T) {
	s := newTestStore(t)
	a1, _ := s.AddAccount(model.ProviderZai, "a1", "h1.p.s3")
	a2, _ := s.AddAccount(model.ProviderZai, "a2", "h2.p.s3")
	for _, a := range []*model.Account{a1, a2} {
		s.Update(a.Provider, a.ID, func(x *model.Account) {
			x.Quota = map[string]map[string]any{
				"GLM-5.3": {"remaining": float64(10), "model": "GLM-5.3"},
			}
		})
	}

	first := s.Select("zai", nil, "GLM-5.3")
	second := s.Select("zai", nil, "GLM-5.3")
	if first == nil || second == nil || first.ID == second.ID {
		t.Fatalf("两个可用账号应轮询交替: %v %v", first, second)
	}

	// skip 已试过的账号
	third := s.Select("zai", map[string]bool{first.ID: true, second.ID: true}, "GLM-5.3")
	if third != nil {
		t.Fatalf("全部跳过后应无可用账号: %v", third)
	}

	// 无快照的账号作为 unknown 后备
	a3, _ := s.AddAccount(model.ProviderZai, "a3", "h3.p.s3")
	got := s.Select("zai", map[string]bool{first.ID: true, second.ID: true}, "GLM-5.3")
	if got == nil || got.ID != a3.ID {
		t.Fatalf("available 空时应回退 unknown: %v", got)
	}

	// 模型额度耗尽的账号被排除：必须多次轮询都选不到它。
	// 只断言一次是不够的——rotation 游标恰好指向 a1 时，即使过滤逻辑失效
	// 也会「通过」，那种断言无法保护这段逻辑。
	s.Update(a2.Provider, a2.ID, func(x *model.Account) {
		x.Quota["GLM-5.3"]["remaining"] = float64(0)
	})
	for range 6 {
		got = s.Select("zai", nil, "GLM-5.3")
		if got == nil {
			t.Fatal("应选到 a1")
		}
		if got.ID == a2.ID {
			t.Fatalf("耗尽账号不应被选中: %v", got.ID)
		}
	}
	// 且耗尽账号不在 available 池 → available=[a1]；skip a1 后回退 unknown [a3]
	got = s.Select("zai", map[string]bool{a1.ID: true}, "GLM-5.3")
	if got == nil || got.ID != a3.ID {
		t.Fatalf("skip available 后应回退 unknown: %v", got)
	}
}

func TestSelectPromoAccountsFirst(t *testing.T) {
	s := newTestStore(t)
	promo1, _ := s.AddAccount(model.ProviderZai, "promo1", "h1.p.s3")
	promo2, _ := s.AddAccount(model.ProviderZai, "promo2", "h2.p.s3")
	plain, _ := s.AddAccount(model.ProviderZai, "plain", "h3.p.s3")
	// promo1/2 持有未耗尽的一次性优惠；plain 只有每日额度。
	for _, a := range []*model.Account{promo1, promo2} {
		s.Update(a.Provider, a.ID, func(x *model.Account) {
			x.Quota = map[string]map[string]any{
				"GLM-5.3": {"remaining": float64(500), "period": "one_time", "model": "GLM-5.3"},
			}
		})
	}
	s.Update(plain.Provider, plain.ID, func(x *model.Account) {
		x.Quota = map[string]map[string]any{
			"GLM-5.3": {"remaining": float64(10), "period": "daily", "model": "GLM-5.3"},
		}
	})

	// 优惠组内轮询，绝不落到 plain。
	seen := map[string]bool{}
	for range 6 {
		acc := s.Select("zai", nil, "GLM-5.3")
		if acc == nil {
			t.Fatal("应选到账号")
		}
		if acc.ID == plain.ID {
			t.Fatal("优惠组未耗尽时不应选中普通账号")
		}
		seen[acc.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("优惠组内应两个账号轮询: %v", seen)
	}

	// 优惠全部耗尽 → 回落普通账号。
	for _, a := range []*model.Account{promo1, promo2} {
		s.Update(a.Provider, a.ID, func(x *model.Account) {
			x.Quota["GLM-5.3"]["remaining"] = float64(0)
		})
	}
	if acc := s.Select("zai", nil, "GLM-5.3"); acc == nil || acc.ID != plain.ID {
		t.Fatalf("优惠耗尽后应选中普通账号: %v", acc)
	}
}

// Update 的 fn 由调用方提供，一旦 panic 必须仍释放锁——否则整个 Store
// 会永久死锁（所有请求都经过 Select/Update）。本测试在 panic 后继续调用
// Store 方法，若锁未释放会直接卡死（由 -timeout 兜底）。
func TestUpdatePanicReleasesLock(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "boom", "h.p.s4")
	if err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fn panic 应向外传播，不得被吞掉")
			}
		}()
		s.Update(acc.Provider, acc.ID, func(*model.Account) {
			panic("boom")
		})
	}()

	// 锁必须已释放：下面的调用若阻塞即说明死锁
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Update(acc.Provider, acc.ID, func(a *model.Account) {
			a.UseCount = 7
		}); err != nil {
			t.Errorf("panic 后 Update 应正常工作: %v", err)
		} else if got := s.Find(acc.Provider, acc.ID); got.UseCount != 7 {
			t.Errorf("use_count 应已更新: %d", got.UseCount)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 后锁未释放，Store 已死锁")
	}
}

func TestUpdateDeletedAccountRejected(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(model.ProviderZai, "ghost", "h.p.s3")
	if ok, _ := s.RemoveAccount(model.ProviderZai, acc.ID); !ok {
		t.Fatal("删除失败")
	}
	// 模拟后台流长期持有旧 ID、删除后回写：Update 必须拒绝而非复活账号
	if err := s.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.UseCount = 999
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("对已删除账号的 Update 应返回 ErrNotFound: %v", err)
	}
	if s.Find(model.ProviderZai, acc.ID) != nil {
		t.Fatal("已删除账号不得复活")
	}
}

func TestArchiveForcesDisabled(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(model.ProviderZai, "arc", "h.p.s3")
	if ok, err := s.SetArchived(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("归档失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.ArchivedAt == nil || got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("归档应强制停用: archived=%v status=%s enabled=%v", got.ArchivedAt, got.Status, got.Enabled)
	}
	if got.IsSelectable(time.Now()) {
		t.Fatal("归档账号不可被调度")
	}

	// 恢复后仍是停用状态，需手动启用
	if ok, err := s.SetArchived(model.ProviderZai, acc.ID, false); !ok || err != nil {
		t.Fatalf("恢复失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ArchivedAt != nil || got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("恢复后应保持停用: archived=%v status=%s enabled=%v", got.ArchivedAt, got.Status, got.Enabled)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("启用失败: %v %v", ok, err)
	}
	if got = s.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive {
		t.Fatalf("手动启用后应恢复 active: %s", got.Status)
	}
}

func TestExportImportRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s1 := openAt(t, filepath.Join(dir, "a.db"))
	acc, err := s1.AddAccount(model.ProviderZai, "exp", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	s1.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.SetDisabledModels([]string{"glm-4.7"})
	})
	payload := s1.Export()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Version != 1 {
		t.Fatalf("导出版本应为 1")
	}

	s2 := openAt(t, filepath.Join(dir, "b.db"))
	var parsed ImportPayload
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	count, err := s2.ImportAccounts(parsed)
	if err != nil || count != 1 {
		t.Fatalf("导入失败: count=%d err=%v", count, err)
	}
	got := s2.Find(model.ProviderZai, "exp")
	if got == nil || got.Secret() != "header.payload.sig" {
		t.Fatalf("导入账号凭证不符: %v", got)
	}
	if len(got.DisabledModels) != 1 || got.DisabledModels[0] != "glm-4.7" {
		t.Fatalf("停用模型应随导入保留: %v", got.DisabledModels)
	}
}

func TestProxyProfiles(t *testing.T) {
	s := newTestStore(t)
	p, err := s.AddProxyProfile("", "socks5://127.0.0.1:1080", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "代理-1" || p.URL != "socks5://127.0.0.1:1080" {
		t.Fatalf("默认命名/URL 不符: %+v", p)
	}
	if _, err := s.AddProxyProfile("代理-1", "http://x:1", true); err == nil {
		t.Fatal("重名应报错")
	}
	if _, err := s.AddProxyProfile("bad", "ftp://x:1", true); err == nil {
		t.Fatal("非法协议应报错")
	}

	up, err := s.UpdateProxyProfile(p.ID, "line-2", "http://1.2.3.4:8080", true)
	if err != nil || up.Name != "line-2" || up.URL != "http://1.2.3.4:8080" {
		t.Fatalf("更新失败: %+v %v", up, err)
	}

	acc, _ := s.AddAccount(model.ProviderZai, "acc", "header.payload.sig")
	if ok, err := s.AssignProxyProfile(acc.ID, p.ID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.ProxyID == nil || *got.ProxyID != p.ID || got.ProxyURL == nil || *got.ProxyURL != "http://1.2.3.4:8080" {
		t.Fatalf("账号代理指派不符: %+v", got)
	}

	// 更新线路 URL 应同步到已指派账号
	if _, err := s.UpdateProxyProfile(p.ID, "", "http://5.6.7.8:9090", true); err != nil {
		t.Fatal(err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ProxyURL == nil || *got.ProxyURL != "http://5.6.7.8:9090" {
		t.Fatalf("线路更新应同步账号: %v", got.ProxyURL)
	}

	if ok, err := s.DeleteProxyProfile(p.ID); !ok || err != nil {
		t.Fatalf("删除失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ProxyID != nil || got.ProxyURL != nil {
		t.Fatalf("删除线路后账号应恢复直连: %+v", got)
	}

	if ok, err := s.AssignProxyProfile("nonexistent", ""); ok || err != nil {
		t.Fatalf("账号不存在应返回 false,nil: %v %v", ok, err)
	}
	if _, err := s.AssignProxyProfile(acc.ID, "proxy-nope"); err == nil {
		t.Fatal("指派不存在的线路应报错")
	}
}

// Select/ListAccounts/Find 返回深拷贝，调用方在锁外的字段读写不与
// Store 内部状态竞争；状态更新一律经 Update 在锁内完成。
// 本测试用高并发验证该契约（需 -race 才能判定）。
func TestSelectAndMutateConcurrently(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "acc", "h.p.s")
	if err != nil {
		t.Fatal(err)
	}
	s.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"remaining": float64(10), "model": "GLM-5.3"},
		}
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧：模拟并发请求选号 + 遍历账号列表
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if got := s.Select(model.ProviderZai, nil, "GLM-5.3"); got != nil {
					// 锁外读副本字段：不应与写侧竞争
					_ = got.Status
					_ = got.UseCount
				}
				for _, a := range s.ListAccounts(model.ProviderZai) {
					_ = a.Status
				}
			}
		}
	}()

	// 写侧：模拟引擎标记账号状态（经 Update 在锁内完成）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			s.Update(acc.Provider, acc.ID, func(a *model.Account) {
				msg := "冷却中"
				a.Status = model.StatusCooling
				a.LastError = &msg
				a.UseCount++
				a.FailCount++
			})
			s.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.Status = model.StatusActive
			})
		}
		close(stop)
	}()

	wg.Wait()
}

// 落库失败时不得改动内存：否则账号在本进程消失、重启后又从 DB 回来。
//
// 删除常被用来撤销可疑或外泄的凭证，「显示已删除、实际还在」是安全相关的
// 静默失败。原实现先改内存再落库，失败时内存已不可逆。
func TestRemoveAccountKeepsMemoryOnPersistFailure(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "victim", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	if s.Find(model.ProviderZai, acc.ID) == nil {
		t.Fatal("前置条件：账号应存在")
	}

	// 关掉底层连接，让 DELETE 必然失败
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RemoveAccount(model.ProviderZai, acc.ID)
	if err == nil {
		t.Fatal("落库失败应返回错误")
	}
	if ok {
		t.Fatal("失败时不应报告已删除")
	}
	// 关键断言：内存状态必须与 DB 保持一致，账号仍在
	if s.Find(model.ProviderZai, acc.ID) == nil {
		t.Fatal("落库失败后账号不应从内存消失（会与 DB 分叉）")
	}
}

// AddAccount 落库失败时不得留下内存账号。
//
// 与 RemoveAccount 对称：反过来会让账号在本次进程里可用、重启后消失，而调用方
// 收到错误以为没建成——「界面显示已保存、重启后回滚」的镜像版本。
func TestAddAccountKeepsMemoryCleanOnPersistFailure(t *testing.T) {
	s := newTestStore(t)
	// 关掉底层连接，让 INSERT 必然失败
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}

	acc, err := s.AddAccount(model.ProviderZai, "ghost", "header.payload.signature")
	if err == nil {
		t.Fatal("落库失败应返回错误")
	}
	if acc != nil {
		t.Fatal("失败时不应返回账号")
	}
	if s.Find(model.ProviderZai, "ghost") != nil {
		t.Fatal("落库失败后内存不应留下账号（会与 DB 分叉）")
	}
}
