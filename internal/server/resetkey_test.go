package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// resetKeyResp 是 POST /v1/admin/accounts/{key}/reset_key 的响应。
type resetKeyResp struct {
	Account struct {
		ID    string  `json:"id"`
		Key   string  `json:"key"` // 掩码后的
		Name  string  `json:"name"`
		Quota float64 `json:"quota"`
		Used  float64 `json:"used"`
	} `json:"account"`
	Key       string `json:"key"`
	Handle    string `json:"handle"`
	OldHandle string `json:"old_handle"`
	Notice    string `json:"notice"`
}

// TestAdminResetKeyEndToEnd：手填与随机两条路、旧 Key 立刻失效、配额保留、
// 公共账号拒绝、冲突与未知账号的报错。
func TestAdminResetKeyEndToEnd(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	adm := userHdr(env.adminKey)

	// 先建一个账号（配额 10，用掉一点，验证重置只换钥匙）
	w := doJSON(h, http.MethodPost, "/v1/admin/accounts",
		`{"key":"vl_resetme","name":"待重置","quota":10}`, adm)
	if w.Code != http.StatusCreated {
		t.Fatalf("建号失败：%d %s", w.Code, w.Body.String())
	}
	var created struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	handle := created.Account.ID
	if handle == "" {
		t.Fatal("建号响应里没有句柄")
	}
	if _, err := env.store.Consume("vl_resetme", 2.5, "测试消耗"); err != nil {
		t.Fatal(err)
	}

	// 1) 不传 key = 随机生成
	w = doJSON(h, http.MethodPost, "/v1/admin/accounts/"+handle+"/reset_key", `{}`, adm)
	if w.Code != http.StatusOK {
		t.Fatalf("随机重置失败：%d %s", w.Code, w.Body.String())
	}
	var got resetKeyResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Key, "vl_") || len(got.Key) < 20 {
		t.Errorf("随机 Key 形态不对：%q", got.Key)
	}
	if got.Handle == "" || got.Handle == handle || got.OldHandle != handle {
		t.Errorf("句柄应随 Key 派生并变化：new=%q old=%q 请求时=%q", got.Handle, got.OldHandle, handle)
	}
	if !strings.Contains(got.Notice, "只会出现这一次") {
		t.Errorf("必须提醒明文只出现一次：%q", got.Notice)
	}
	if got.Account.Key == got.Key {
		t.Error("账号视图里的 key 必须是掩码，不能回明文")
	}
	if got.Account.Quota != 7.5 || got.Account.Used != 2.5 {
		t.Errorf("配额与用量应保留：quota=%v used=%v", got.Account.Quota, got.Account.Used)
	}

	// 2) 旧 Key 立刻失效，新 Key 立刻可用
	if w := do(h, http.MethodGet, "/v1/usage", userHdr("vl_resetme")); w.Code == http.StatusOK {
		t.Errorf("旧 Key 不该还能用：%d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/usage", userHdr(got.Key)); w.Code != http.StatusOK {
		t.Errorf("新 Key 应可用：%d %s", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodGet, "/v1/usage", userHdr(got.OldHandle)); w.Code == http.StatusOK {
		t.Error("旧句柄不该还能当凭据用")
	}

	// 3) 手填 Key：自动补 vl_ 前缀
	w = doJSON(h, http.MethodPost, "/v1/admin/accounts/"+got.Handle+"/reset_key",
		`{"key":"manual_key"}`, adm)
	if w.Code != http.StatusOK {
		t.Fatalf("手填重置失败：%d %s", w.Code, w.Body.String())
	}
	var manual resetKeyResp
	if err := json.Unmarshal(w.Body.Bytes(), &manual); err != nil {
		t.Fatal(err)
	}
	if manual.Key != "vl_manual_key" {
		t.Errorf("手填 Key 应补 vl_ 前缀：%q", manual.Key)
	}
	if !strings.Contains(manual.Notice, "vl_") {
		t.Errorf("补了前缀就该如实告知：%q", manual.Notice)
	}

	// 4) 冲突：换成别人已经占用的 Key
	w = doJSON(h, http.MethodPost, "/v1/admin/accounts/"+manual.Handle+"/reset_key",
		`{"key":"`+env.userKey+`"}`, adm)
	if w.Code != http.StatusBadRequest {
		t.Errorf("占用他人 Key 应 400，得到 %d（%s）", w.Code, w.Body.String())
	}

	// 5) 公共账号：Key 来自配置，拒绝
	pub, ok := env.store.Get(env.srv.cfg.PublicKey)
	if !ok {
		t.Fatalf("测试环境里应有公共账号 %s", env.srv.cfg.PublicKey)
	}
	w = doJSON(h, http.MethodPost, "/v1/admin/accounts/"+pub.ID+"/reset_key", `{}`, adm)
	if w.Code != http.StatusBadRequest {
		t.Errorf("公共账号应拒绝重置，得到 %d（%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "VIDLINK_PUBLIC_KEY") {
		t.Errorf("拒绝时要说清原因与替代做法：%s", w.Body.String())
	}

	// 6) 未知账号 → 404；不带管理 Key → 403
	if w := doJSON(h, http.MethodPost, "/v1/admin/accounts/acc_0000000000000000/reset_key", `{}`, adm); w.Code != http.StatusNotFound {
		t.Errorf("未知账号应 404，得到 %d", w.Code)
	}
	if w := doJSON(h, http.MethodPost, "/v1/admin/accounts/"+manual.Handle+"/reset_key", `{}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("没带管理 Key 应 403，得到 %d", w.Code)
	}

	// 7) 账本里留了一笔"重置 Key"，且写明旧句柄（否则历史流水就找不到入口了）
	w = do(h, http.MethodGet, "/v1/admin/ledger?account="+manual.Handle+"&type=set", adm)
	if w.Code != http.StatusOK {
		t.Fatalf("查流水失败：%d %s", w.Code, w.Body.String())
	}
	var led struct {
		Entries []struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &led); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range led.Entries {
		if e.Type == "set" && strings.Contains(e.Detail, "重置 Key") && strings.Contains(e.Detail, manual.OldHandle) {
			found = true
		}
	}
	if !found {
		t.Errorf("流水里应有一笔写明旧句柄的重置记录：%+v", led.Entries)
	}
}

// TestResetKeyMovesBillsToNewAccount：重置之后，按**新 Key** 查自己的流水，
// 必须能看到重置之前的创建与消耗（账单跟着账号走）。
func TestResetKeyMovesBillsToNewAccount(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	adm := userHdr(env.adminKey)

	w := doJSON(h, http.MethodPost, "/v1/admin/accounts",
		`{"key":"vl_billme","name":"账单归属","quota":50}`, adm)
	if w.Code != http.StatusCreated {
		t.Fatalf("建号失败：%d %s", w.Code, w.Body.String())
	}
	var created struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// 重置前消耗一笔，并确认它出现在自己的流水里
	if _, err := env.store.Consume("vl_billme", 3, "重置前的消耗"); err != nil {
		t.Fatal(err)
	}

	w = doJSON(h, http.MethodPost, "/v1/admin/accounts/"+created.Account.ID+"/reset_key", `{}`, adm)
	if w.Code != http.StatusOK {
		t.Fatalf("重置失败：%d %s", w.Code, w.Body.String())
	}
	var got resetKeyResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Notice, "历史账单") {
		t.Errorf("notice 应说明账单归属：%q", got.Notice)
	}

	// 新 Key 查自己的流水：应看到"重置前的消耗"与创建记录
	w = do(h, http.MethodGet, "/v1/ledger?limit=50", userHdr(got.Key))
	if w.Code != http.StatusOK {
		t.Fatalf("查流水失败：%d %s", w.Code, w.Body.String())
	}
	var led struct {
		Entries []struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
		} `json:"entries"`
		Totals map[string]struct {
			Count int64   `json:"count"`
			Units float64 `json:"units"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &led); err != nil {
		t.Fatal(err)
	}
	var sawCreate, sawOldConsume bool
	for _, e := range led.Entries {
		if e.Type == "create" {
			sawCreate = true
		}
		if e.Type == "consume" && strings.Contains(e.Detail, "重置前的消耗") {
			sawOldConsume = true
		}
	}
	if !sawCreate || !sawOldConsume {
		t.Errorf("新 Key 名下应能看到重置前的流水：create=%v 旧消耗=%v（%+v）", sawCreate, sawOldConsume, led.Entries)
	}
	if got := led.Totals["consume"].Units; got != -3 {
		t.Errorf("累计消耗应跨重置合并：%v，应为 -3", got)
	}

	// 管理面按新句柄查也一样
	w = do(h, http.MethodGet, "/v1/admin/ledger?account="+got.Handle+"&limit=50", adm)
	if w.Code != http.StatusOK {
		t.Fatalf("管理面查流水失败：%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "重置前的消耗") {
		t.Error("管理面按新句柄也应看到重置前的流水")
	}
}
