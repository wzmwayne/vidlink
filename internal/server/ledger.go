package server

import (
	"errors"
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
	// 公共账号没有"自己的账单"：Key 是共享的，流水混着所有 IP 的调用，
	// 给出去既是隐私问题也没有对账意义。要账单就申请独立 Key。
	if acct.PublicAccount {
		writeError(w, core.Errf(core.KindForbidden, "", "auth",
			"公共 Key 不提供账单（它是共享账号，流水混有所有访客的调用）；"+
				"需要账单请向管理员申请独立 Key"))
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
// 寻址方式与管理面其它接口一致：`?account=` 句柄或明文 Key（`?id=` 同义），
// 都不给就是总账单。
//
// 这里**不用** `?key=`：那个参数名现在是"凭据"的意思（URL 里只接受签名），
// 而本参数是"把范围限定到哪个账号"的过滤器。两者同名会让
// `/v1/admin/ledger?key=<明文>` 既像认证又像过滤，是最容易埋坑的一类歧义。
func (s *Server) handleAdminLedger(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	q, err := ledgerParseQuery(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}

	raw := strings.TrimSpace(r.URL.Query().Get("account"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	resp := map[string]any{
		"unit":      unitName,
		"types":     ledgerTypesDoc(),
		"limit_max": account.MaxLedgerLimit,
		"note": "流水只保留最近若干条，汇总为全量口径；本接口不消耗配额。" +
			"不带 account/id 即总账单",
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

// handleCheckIn 执行每日签到：POST /v1/checkin
//
// 不消耗配额（它是来领配额的），也不占按 Key 的解析闸门。
// 结果用 200 + 字段表达，而不是把"今天已签到"当错误：客户端据此决定
// 显示"已签到，明天再来"还是"+25"，不需要解析错误码。
func (s *Server) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountFrom(r.Context())
	if !ok {
		writeError(w, core.Errf(core.KindForbidden, "", "auth", "缺少 API Key"))
		return
	}
	// 公共 Key 不需要签到：它的额度是"每 IP 每日"，与账本余额无关
	if acct.PublicAccount {
		writeError(w, core.Errf(core.KindForbidden, "", "auth",
			"公共 Key 不需要签到：它的额度是每 IP 每日自动给的；"+
				"需要每日签到领配额请向管理员申请独立 Key"))
		return
	}

	updated, res, err := s.accounts.CheckIn(acct.Key)
	if err != nil {
		switch {
		case errors.Is(err, account.ErrCheckInDisabled):
			writeError(w, core.Errf(core.KindBadInput, "", "checkin",
				"该账号未开放每日签到（需要管理员在账号上设置「每日签到额度」）"))
		case errors.Is(err, account.ErrDisabled):
			writeError(w, core.Errf(core.KindForbidden, "", "auth", "账号已停用"))
		case errors.Is(err, account.ErrNotFound):
			writeError(w, core.Errf(core.KindForbidden, "", "auth", "API Key 无效"))
		default:
			writeError(w, core.E(core.KindInternal, "", "checkin", "签到失败", err))
		}
		return
	}

	msg := fmt.Sprintf("签到成功：+%.4g %s，当前剩余 %.4g", res.Granted, unitName, res.Balance)
	switch {
	case res.Already:
		msg = fmt.Sprintf("今天已经签到过了，%s 后可再签", res.NextAt.Format("2006-01-02 15:04"))
	case res.AtCap:
		msg = fmt.Sprintf("余额已达「停止增加界限」，本次不增加，也不占用今天的签到机会"+
			"（当前 %.4g）", res.Balance)
	}
	if res.Granted > 0 {
		s.log.Info("每日签到", "request_id", requestID(r.Context()), "key", acct.Masked(),
			"granted", res.Granted, "balance", res.Balance, "cap", updated.GrantCap)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granted":            res.Granted,
		"balance":            res.Balance,
		"daily":              updated.DailyGrant,
		"cap":                updated.GrantCap,
		"already_checked_in": res.Already,
		"at_cap":             res.AtCap,
		"checked_in":         res.Granted > 0,
		"next_checkin_at":    res.NextAt,
		"unit":               unitName,
		"message":            msg,
	})
}
