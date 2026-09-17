package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/core"
)

// 配额流水（账单）读取。
//
// 两个入口，共用一套视图：
//
//	GET /v1/ledger         用户读自己的
//	GET /v1/admin/ledger   管理员读指定账号的，或不带条件读总账单
//
// 都**不消耗配额**：对账这件事本身不该收费，否则"查一次账单少一点配额"
// 会让人不敢查。它们也不占按 Key 的解析闸门（同 /v1/usage）。

// ledgerEntry 是流水对外的形状。
//
// 与 account.Entry 的唯一区别是 Key 掩码：明文 Key 只存在于文件与内存里，
// 出去的一律是 `vl_a********3f7c` 这种形式（与账号视图一致）。
type ledgerEntry struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Key     string    `json:"key"`
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Units   float64   `json:"units"`
	Balance float64   `json:"balance"`
	Used    float64   `json:"used,omitempty"`
	Calls   int64     `json:"calls,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

func ledgerEntries(in []account.Entry) []ledgerEntry {
	out := make([]ledgerEntry, 0, len(in))
	for _, e := range in {
		out = append(out, ledgerEntry{
			Time: e.Time, Type: string(e.Type), Key: e.MaskedKey(), ID: e.ID, Name: e.Name,
			Units: e.Units, Balance: e.Balance, Used: e.Used, Calls: e.Calls, Detail: e.Detail,
		})
	}
	return out
}

// ledgerParseQuery 解析流水查询参数。
//
// 三个参数刻意做严格校验：`limit` 不是数字、`type` 不认识都直接 400。
// 静默忽略会让人以为"筛选生效了"，其实拿到的是没筛过的一份数据——
// 对账场景下这种错觉比报错贵得多。
func ledgerParseQuery(r *http.Request) (account.LedgerQuery, error) {
	q := account.LedgerQuery{}
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("limit 必须是正整数（上限 %d）", account.MaxLedgerLimit)
		}
		q.Limit = n
	}
	if v := strings.TrimSpace(r.URL.Query().Get("type")); v != "" {
		t := account.EntryType(v)
		if !t.Valid() {
			return q, fmt.Errorf("type 只能是 %s 之一", ledgerTypeList())
		}
		q.Type = t
	}
	return q, nil
}

func ledgerTypeList() string {
	names := make([]string, 0, len(account.AllEntryTypes))
	for _, t := range account.AllEntryTypes {
		names = append(names, string(t))
	}
	return strings.Join(names, " / ")
}

// ledgerTypesDoc 把每个类型的中文含义一并返回。
//
// 客户端要显示"使用 / 管理员增加 / …"这类文案，与其在前端各写一份
// （迟早会与后端漂移），不如接口自己说清楚。
func ledgerTypesDoc() map[string]string {
	return map[string]string{
		string(account.EntryConsume): "使用：解析或代理消耗配额（units 为负）",
		string(account.EntryAdd):     "管理员增加配额",
		string(account.EntryReduce):  "管理员减少配额",
		string(account.EntrySet):     "管理员设置：配额设为某值、账号倍率、停用/启用等",
		string(account.EntryCreate):  "创建账号",
		string(account.EntryDelete):  "删除账号",
	}
}

// handleLedger 返回**当前账号自己**的流水。GET /v1/ledger
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountFrom(r.Context())
	if !ok {
		writeError(w, core.Errf(core.KindForbidden, "", "auth", "缺少 API Key"))
		return
	}
	q, err := ledgerParseQuery(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	q.Key = acct.Key // 用户只能读自己的：路径里没有任何"读别人"的入口
	entries, totals := s.accounts.Ledger(q)
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":     "self",
		"account":   map[string]any{"id": acct.ID, "name": acct.Name, "key": acct.Masked()},
		"unit":      unitName,
		"entries":   ledgerEntries(entries),
		"totals":    totals,
		"types":     ledgerTypesDoc(),
		"limit_max": account.MaxLedgerLimit,
		"note":      "流水只保留最近若干条，汇总为全量口径；本接口不消耗配额",
	})
}

// handleAdminLedger 返回指定账号或全部账号的流水。GET /v1/admin/ledger
//
// 寻址方式与管理面其它接口一致：`?id=` 句柄或 `?key=` 明文 Key，
// 都不给就是总账单。
func (s *Server) handleAdminLedger(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	q, err := ledgerParseQuery(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}

	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	resp := map[string]any{
		"unit":      unitName,
		"types":     ledgerTypesDoc(),
		"limit_max": account.MaxLedgerLimit,
		"note": "流水只保留最近若干条，汇总为全量口径；本接口不消耗配额。" +
			"不带 id/key 即总账单",
	}
	if raw == "" {
		entries, totals := s.accounts.Ledger(q)
		resp["scope"] = "all"
		resp["entries"] = ledgerEntries(entries)
		resp["totals"] = totals
		writeJSON(w, http.StatusOK, resp)
		return
	}

	acct, ok := s.accounts.Resolve(raw)
	if !ok {
		writeError(w, core.NotFound("", "账号不存在"))
		return
	}
	q.Key = acct.Key
	entries, totals := s.accounts.Ledger(q)
	resp["scope"] = "account"
	resp["account"] = map[string]any{
		"id": acct.ID, "name": acct.Name, "key": acct.Masked(),
		"quota": acct.Quota, "used": acct.Used, "calls": acct.Calls,
		"multiplier": acct.Multiplier, "disabled": acct.Disabled,
	}
	resp["entries"] = ledgerEntries(entries)
	resp["totals"] = totals
	writeJSON(w, http.StatusOK, resp)
}
