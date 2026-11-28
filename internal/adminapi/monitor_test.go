package adminapi

import (
	"net/http"
	"testing"

	"zcode2api/internal/model"
)

func TestHandleUsage_ExcludesArchived(t *testing.T) {
	mux, st, _ := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "tok-live\ntok-arch",
	})
	ids := added["ids"].([]any)
	liveID := str(t, ids[0])
	archID := str(t, ids[1])

	_ = st.Update(model.ProviderZai, liveID, func(a *model.Account) {
		a.UseCount = 5
		a.TotalInputTokens = 100
	})
	_ = st.Update(model.ProviderZai, archID, func(a *model.Account) {
		a.UseCount = 99
		a.TotalInputTokens = 9999
	})
	if _, err := st.SetArchived(model.ProviderZai, archID, true); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/usage", nil)
	if code != http.StatusOK {
		t.Fatalf("handleUsage 應回 200，實際: %d %v", code, body)
	}
	ranking := body["ranking"].([]any)
	if len(ranking) != 1 {
		t.Fatalf("已歸檔帳號不應出現在用量排行中，預期 1 筆，實際 %d 筆: %v", len(ranking), ranking)
	}
	row := ranking[0].(map[string]any)
	if num(t, row["requests"]) != 5 {
		t.Fatalf("排行應僅含活躍帳號，實際: %v", row)
	}
	if str(t, row["id"]) != liveID {
		t.Fatalf("排行項應包含 id=%s，實際: %v", liveID, row)
	}
}

func TestUpdateAccount_DisabledCredentialChange(t *testing.T) {
	mux, st, _ := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "old-tok-1",
	})
	id := str(t, added["ids"].([]any)[0])
	if _, err := st.SetEnabled(model.ProviderZai, id, false); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+id, map[string]any{
		"token": "new-header.new-payload.new-sig",
	})
	if code != http.StatusOK {
		t.Fatalf("更新憑證應回 200，實際: %d %v", code, body)
	}
	got := st.FindAny(id)
	if got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("已停用帳號更換憑證不得被刷回 active，實際 status=%s enabled=%v", got.Status, got.Enabled)
	}
}
