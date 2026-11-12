package chprov

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 本文件是「额度读取」：按描述表发一次请求，把各家**各不相同**的报文归一成
// 一个 Quota。归一的口径写在 chprov.go 的 Quota 注释里（四态严格互斥）。
//
// # 为什么每家的解析要单独一个函数
//
// 29 家的报文形状没有共性：zcode 给桶数组、cline 给一个微美元标量、trae 给
// 套餐列表、officeace 给订阅分组、loomy 给「永久 + 每日」两池。"揉成一个通用
// 深搜"在这里做不到诚实 —— 深搜找到的第一个像余额的字段，在 officeace 上会
// 是某一组的 used（已用量），在 trae 上会是某一包的 limit。
//
// 通用深搜因此**只保留一件事**：找「最早作废时刻」（MinExpiry）。余额与分桶
// 一律逐家解析，解析不出就报「读不到」，绝不猜。
//
// # 失败三种，不许压成一种
//
//	401/403       凭据失效 → 零值（这家在修好之前发不出去，剔除是对的）
//	其它非 2xx    上游/网络问题 → Failed（这次读不到，仍可作为最后一站）
//	200 但形状不对  → Failed + 具体哪一步对不上（排查要能定位到字段）
//
// ⚠️ 401 判零值不是「宁可少一个」的保守，而是**准确**：拿一个已被上游拒绝的
// 凭据去轮换，每一站都必然失败，把它留在链上只是把失败推给用户。

// ReadQuota 读一家渠道的额度。acct 为空时直接报「未登录」——没账号的渠道不该
// 参与轮换，而这条判据必须在发请求之前，否则每次面板刷新都会白打一次上游。
func ReadQuota(ctx context.Context, id string, acct Account) Quota {
	spec, ok := SpecOf(id)
	if !ok {
		return Fail("渠道 " + id + " 没有登记在本表里")
	}
	if acct.Empty() {
		return Fail("未登录（没有可用凭据）")
	}
	if IsNativeChannel(id) {
		for _, key := range QuotaExtraKeys(id) {
			if acct.Field(key) == "" {
				return Fail("凭据缺少 " + key)
			}
		}
		p, status, failure := checkinRequest(ctx, "GET", spec.Quota.URL(acct), rewardHeaders(id, acct), "")
		if failure != nil {
			return Fail("额度请求失败，请检查网络、代理或登录会话")
		}
		if failure = rewardEnvelope(id, p, status); failure != nil {
			return Fail(failure.Message)
		}
		return newRewardQuota(spec, acct, p)
	}
	if id == "qoder" || id == "qodercn" {
		// Read THIS account, rather than the currently selected sidecar login.
		spec.Quota = QuotaSpec{Method: "GET", BaseURL: qoderOpenAPIBase, Path: "/api/v2/quota/usage", Auth: AuthBearer}
	}
	if spec.Tier == TierNone || spec.Quota.Path == "" {
		// 上游根本没有额度端点：这不是失败，是「没有到期概念」。
		// 按永久参与 —— 用户要的是「这家也在轮换里」，代价是耗尽后失败一次。
		return Quota{Never: true, Detail: "上游没有额度端点，按永久额度参与轮换"}
	}
	if spec.Tier == TierExpiry {
		// 华为云签名要求 AK/SK 齐全；缺一半时发出去的请求必然 401，提前说清楚。
		if spec.Quota.Auth == AuthHuaweiAKSK &&
			(acct.Field("access_key_id") == "" || acct.Field("secret_access_key") == "") {
			return Fail("凭据缺少 AK/SK")
		}
		if spec.Quota.Auth == AuthLoomySign && acct.Field("access_key") == "" {
			return Fail("凭据缺少讯飞签名用的 access_key / secret_key")
		}
	}

	headers, err := authHeaders(spec.Quota, acct)
	if err != nil {
		return Fail(err.Error())
	}
	url := spec.Quota.URL(acct)
	var code int
	var body []byte
	switch strings.ToUpper(spec.Quota.Method) {
	case "", "GET":
		code, body, err = HTTPGet(ctx, url, headers)
	case "POST":
		code, body, err = HTTPPost(ctx, url, headers, spec.Quota.Body)
	default:
		return Fail("未支持的请求方法 " + spec.Quota.Method)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return Fail("额度端点超时")
		}
		// http.Client errors quote the full request URL, and AuthQuery
		// channels put accessToken= right into its query. Never surface it
		// to the dashboard or the log.
		return Fail("额度请求失败（网络错误，原始地址已隐藏）")
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// 凭据被上游拒绝：这一家现在发不出请求，剔除是对的。但**必须留下原因** ——
		// 否则面板只显示「上游说不可用」，而用户要去做的其实是「重新登录」，
		// 与「额度真的用光了」是两件完全不同的事。
		return Quota{Detail: "凭据被拒绝（" + statusText(code, body) + "）：这一家需要重新登录"}
	case code < 200 || code >= 300:
		return Fail(statusText(code, body))
	}
	return parseQuota(spec, acct, body)
}

// authHeaders 按 AuthStyle 拼请求头。
func authHeaders(s QuotaSpec, a Account) (map[string]string, error) {
	h := map[string]string{}
	if s.Headers != nil {
		for k, v := range s.Headers(a) {
			h[k] = v
		}
	}
	switch s.Auth {
	case AuthBearer, "":
		if a.AccessToken != "" {
			h["authorization"] = "Bearer " + a.AccessToken
		}
	case AuthQuery:
		// token 在 query 里（accio），不额外带头。
	case AuthCookie:
		cookie := a.Field("cookie")
		if cookie == "" {
			return nil, errors.New("凭据缺少登录会话 Cookie")
		}
		h["cookie"] = cookie
	case AuthHuaweiAKSK:
		if err := signHuawei(h, s.Method, s.URL(a), s, a); err != nil {
			return nil, err
		}
	case AuthHuaweiV11:
		if err := signHuaweiV11(h, s.Method, s.URL(a), s, a); err != nil {
			return nil, err
		}
	case AuthLoomySign:
		if err := signLoomy(h, s.Method, s.Path, s, a); err != nil {
			return nil, err
		}
	case AuthCustom:
		// 各家 Header 构造函数里已写好凭据（trae 的 Cloud-IDE-JWT、zcode 的 Bearer）。
	}
	return h, nil
}

// --- 逐家报文解析 -------------------------------------------------------------

// parseQuota 按渠道形状归一。handler 缺席时走通用深搜（只找最早作废时刻）。
func parseQuota(spec Spec, acct Account, body []byte) Quota {
	if len(strings.TrimSpace(string(body))) == 0 {
		return Fail("额度端返回空响应体")
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return Fail("额度响应无法解析：" + short(body))
	}
	if h, ok := shapeHandlers[spec.ID]; ok {
		return h(spec, acct, payload)
	}
	return genericQuota(spec, payload)
}

// ParseQuota shares the exact channel balance/expiry rules with legacy lane
// status readers; keeping two parsers previously accepted empty resource packs.
func ParseQuota(id string, body []byte) Quota {
	spec, ok := SpecOf(id)
	if !ok {
		return Fail("未登记渠道: " + id)
	}
	return parseQuota(spec, Account{}, body)
}

// genericQuota 是通用判据：先找最早作废时刻，再判有没有余额。
func genericQuota(spec Spec, payload any) Quota {
	if HasBalance(payload, 5) {
		if ms, ok := FindExpiry(payload, 5); ok {
			return Quota{Known: true, ExpiryMS: ms}
		}
		// 有余额但没有到期时刻：这家是 TierExpiry 却这次没下发时间字段，
		// 按永久参与（不是剔除）。剔除会让一次上游改字段把它静默从轮换里拿走。
		return Quota{Never: true, Detail: "报文里有余额，但没有到期时刻"}
	}
	if spec.Tier == TierNone {
		return Quota{Never: true}
	}
	return Unusable()
}

// numeric 读一个「可能是数字、也可能是字符串」的标量（上游两种都用过）。
func numeric(v any) (float64, bool) { return toNum(v) }

func qoderQuota(_ Spec, _ Account, payload any) Quota {
	root, _ := flatten(payload).(map[string]any)
	if root == nil {
		return Fail("额度响应不是对象")
	}
	if root["isQuotaExceeded"] == true || root["is_exceeded"] == true {
		return Unusable()
	}
	plan := objField(root, "userQuota")
	addon := objField(root, "addOnQuota")
	if plan == nil && addon == nil {
		// Embedded sidecar /quota uses the normalized Quota structure.
		if _, ok := numeric(root["remaining"]); !ok {
			return Fail("响应里没有 userQuota / addOnQuota")
		}
		plan = root
	}
	q := Quota{}
	var total float64
	for _, item := range []struct {
		quota  map[string]any
		expiry any
	}{
		{plan, root["expiresAt"]}, {addon, nil},
	} {
		remaining, ok := numeric(item.quota["remaining"])
		if !ok {
			t, tok := numeric(item.quota["total"])
			u, uok := numeric(item.quota["used"])
			if !tok || !uok {
				continue
			}
			remaining = t - u
		}
		if remaining <= 0 {
			continue
		}
		raw := item.expiry
		if plan != nil && item.quota != nil && raw == nil {
			raw = item.quota["reset_at_ms"]
		}
		if ms, ok := ParseExpiryValue(raw); ok {
			if ms <= time.Now().UnixMilli() {
				continue
			}
			if !q.Known || ms < q.ExpiryMS {
				q.Known, q.ExpiryMS = true, ms
			}
		}
		total += remaining
	}
	if total <= 0 {
		return Unusable()
	}
	q.Remaining = strconv.FormatFloat(total, 'f', -1, 64)
	q.Never = !q.Known
	return q
}

// objField 从 map 里取一个子对象。
func objField(v any, key string) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	child, _ := m[key].(map[string]any)
	return child
}

// arrField 从 map 里取一个数组。
func arrField(v any, key string) []any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	child, _ := m[key].([]any)
	return child
}

// flatten 在常见信封里下钻一层 data / result。
func flatten(v any) any {
	for _, key := range []string{"data", "result", "payload"} {
		if child := objField(v, key); child != nil {
			return child
		}
	}
	return v
}

// shapeHandlers 是逐家形状。缺席的渠道走 genericQuota。
//
// ⚠️ genericQuota 的深搜只认「像到期时刻的键名」，而 buddy 系的余额是
// **双层嵌套 + PascalCase**（data.Response.Data.Accounts[]，包内字段
// CycleCapacityRemain / DeductionEndTime）。深搜能找到 DeductionEndTime（键名在表里），
// 但它会退化成「随便抓到第一个包」—— 所以要逐包解析、取**有效包**里最早的那个。
var shapeHandlers = map[string]func(Spec, Account, any) Quota{
	"astudio":      newRewardQuota,
	"autoclaw":     newRewardQuota,
	"autoclawintl": newRewardQuota,
	"magicrew":     newRewardQuota,
	"magicrewintl": newRewardQuota,
	"qoder":        qoderQuota,
	"qodercn":      qoderQuota,
	// zcode：data.balances[] 各带 expires_at（秒）。企业版不下发额度数字，
	// 此时按**永久**（不是 0 —— 0 是「已用光」的语义）。
	"zcode": func(_ Spec, _ Account, payload any) Quota {
		root, _ := payload.(map[string]any)
		if root == nil {
			return Fail("额度响应不是对象")
		}
		if code, ok := numeric(root["code"]); ok && code != 0 {
			return Fail("上游返回业务码 " + strconv.Itoa(int(code)))
		}
		data := objField(root, "data")
		if data == nil {
			return Fail("响应里没有 data")
		}
		if mode, _ := data["displayMode"].(string); mode == "enterprise" {
			return Quota{Never: true, Detail: "企业版不下发额度数字（只有外链），按永久额度参与"}
		}
		buckets, _ := data["balances"].([]any)
		q := Quota{}
		var remain float64
		for _, b := range buckets {
			rec, ok := b.(map[string]any)
			if !ok {
				continue
			}
			balance, hasBalance := numeric(rec["available_units"])
			if !hasBalance {
				balance, hasBalance = numeric(rec["remaining_units"])
			}
			if !hasBalance || balance <= 0 {
				continue
			}
			if raw, has := rec["expires_at"]; has {
				if ms, ok := ParseExpiryValue(raw); ok {
					if ms <= time.Now().UnixMilli() {
						continue
					}
					if !q.Known || ms < q.ExpiryMS {
						q.Known, q.ExpiryMS = true, ms
					}
				}
			}
			remain += balance
		}
		if remain > 0 {
			q.Remaining = strconv.FormatFloat(remain, 'f', -1, 64) + " token"
		}
		if !q.Known && remain > 0 {
			q.Never = true
			q.Detail = "有余额但没有到期时刻"
		}
		if !q.Known && !q.Never {
			// 空桶数组是**上游明确说「没有额度」**（0 是「已用光」的语义），
			// 但要吃写成一句能读懂的话：只给零值的话面板显示「上游说不可用」，
			// 用户分不清是没钱、没订阅，还是 token 过期。
			root, _ := payload.(map[string]any)
			_ = root
			return Quota{Detail: "余额报文里没有任何额度桶（没订阅或已用光）"}
		}
		return q
	},

	// buddy 系（workbuddy / CodeBuddy）：data.Response.Data.Accounts[] 逐包。
	//
	// 判据（与参考实现逐条对上）：
	//   - 包是否**有效**：Status != 3，且 ExpiredTime 未过，且 DeductionEndTime 未过；
	//   - 余额取 **CycleCapacityRemain**（本周期口径），不是 CapacityRemain（终身口径）；
	//   - 有效包里最早的 DeductionEndTime 就是这份额度的临期时刻。
	//
	// ⚠️ 失效包的余额服务端照样下发，并进总额会让数字虚高（实测某账号因此从
	// 155.67 变成 655.67）—— 故只累加有效包。
	"workbuddy": buddyQuota,
	"buddy":     buddyQuota,

	// cline：data.balance 是聚合标量（micro-USD），没有到期时刻。
	"cline": func(_ Spec, _ Account, payload any) Quota {
		root, _ := payload.(map[string]any)
		data := objField(root, "data")
		if data == nil {
			return Fail("响应里没有 data")
		}
		bal, ok := numeric(data["balance"])
		if !ok {
			return Fail("响应里没有 balance 字段")
		}
		if bal <= 0 {
			return Unusable()
		}
		return Quota{Never: true, Remaining: "$" + strconv.FormatFloat(bal/100000, 'f', 2, 64),
			Detail: "余额是聚合标量，没有到期时刻"}
	},

	// raccoon：available_points 是核心字段；分项 reward/daily/monthly/topup 逐桶。
	//
	// ⚠️ 缺 available_points 即形状不对（不编造数字）：这一家的余额是多来源聚合，
	// 拿某一项冒充总额会把「还有 300」说成「还有 0」。
	"raccoon": func(_ Spec, _ Account, payload any) Quota {
		root, _ := payload.(map[string]any)
		if root == nil {
			return Fail("额度响应不是对象")
		}
		d := objField(root, "data")
		if d == nil {
			d = root
		}
		if v, ok := numeric(root["code"]); ok && v != 0 {
			return Quota{Detail: "上游业务码 " + strconv.Itoa(int(v)) + "：" + field(root, "message")}
		}
		avail, ok := numeric(d["available_points"])
		if !ok {
			return Fail("响应里没有 available_points")
		}
		if avail <= 0 {
			return Unusable()
		}
		// 这一家的余额没有到期时刻（服务端按日自动发放 daily_grant，但报文不下发
		// 「哪天发」）：按永久参与，**不**猜次日零点。
		q := Quota{Never: true, Remaining: strconv.FormatFloat(avail, 'f', -1, 64) + " 积分"}
		parts := []string{}
		for _, k := range []string{"reward_points", "daily_points", "monthly_points", "topup_points"} {
			if v, ok := numeric(d[k]); ok && v > 0 {
				parts = append(parts, k+" "+strconv.FormatFloat(v, 'f', -1, 64))
			}
		}
		if len(parts) > 0 {
			q.Detail = "分项：" + strings.Join(parts, " · ") + "（没有到期时刻）"
		} else {
			q.Detail = "没有到期时刻"
		}
		return q
	},

	// loomy：两池。每日赠送按自然日重置 ⇒ 折算成次日零点（这是**唯一**允许
	// 用自然日口径的一家：它的报文里 dailyCycleDate 明说了「当日」）。
	"loomy": func(_ Spec, _ Account, payload any) Quota {
		data, ok := flatten(payload).(map[string]any)
		if !ok {
			return Fail("额度响应不是对象")
		}
		permanent, hasPerm := numeric(data["balance"])
		daily, hasDaily := numeric(data["dailyBalance"])
		q := Quota{}
		if hasDaily && daily > 0 {
			q.Known = true
			q.ExpiryMS = NextDayStartMS(time.Now())
			q.Detail = "每日赠送额度当日 24:00 作废"
		}
		parts := []string{}
		if hasPerm {
			parts = append(parts, "永久 "+strconv.FormatFloat(permanent, 'f', -1, 64))
		}
		if hasDaily {
			parts = append(parts, "每日 "+strconv.FormatFloat(daily, 'f', -1, 64))
		}
		if len(parts) > 0 {
			q.Remaining = strings.Join(parts, " · ")
		}
		if q.Known {
			return q
		}
		if hasPerm && permanent > 0 {
			return Quota{Never: true, Remaining: q.Remaining, Detail: "只有永久积分"}
		}
		if len(parts) == 0 {
			return Fail("两池都读不到（balance / dailyBalance 均缺失）")
		}
		return Unusable()
	},

	// gemini：配额按模型分桶，没有任何到期字段 ⇒ 永久。
	"gemini": func(_ Spec, _ Account, payload any) Quota {
		root, _ := payload.(map[string]any)
		if root == nil {
			return Fail("额度响应不是对象")
		}
		// 未授权时上游不给桶，只有 error；这时如实报「未登录」。
		if e := objField(root, "error"); e != nil {
			if msg, _ := e["message"].(string); msg != "" {
				return Unusable()
			}
		}
		return Quota{Never: true, Detail: "配额按模型分桶，报文里没有到期字段"}
	},

	// CodeArts only assigns the package end date to positive basic credits.
	"codearts": func(_ Spec, _ Account, payload any) Quota {
		root, _ := flatten(payload).(map[string]any)
		if root == nil {
			return Fail("额度响应不是对象")
		}
		metrics, ok := root["metrics"].([]any)
		if !ok {
			return Fail("响应里没有 metrics")
		}
		q := Quota{}
		var total float64
		for _, item := range metrics {
			rec, _ := item.(map[string]any)
			name, _ := rec["name"].(string)
			if name != "usageBasicPackageCredit" && name != "usageBonusPackageCredit" && name != "usageOnDemandPackageCredit" {
				continue
			}
			remain, ok := numeric(rec["package_credit_remain"])
			if !ok || remain <= 0 {
				continue
			}
			if name == "usageBasicPackageCredit" {
				if ms, ok := ParseExpiryValue(root["end_date"]); ok {
					if ms <= time.Now().UnixMilli() {
						continue
					}
					q.Known, q.ExpiryMS = true, ms
				}
			}
			total += remain
		}
		if total <= 0 {
			return Unusable()
		}
		q.Remaining = strconv.FormatFloat(total, 'f', -1, 64) + " 积分"
		q.Never = !q.Known
		return q
	},

	"trae": func(_ Spec, _ Account, payload any) Quota {
		root, _ := flatten(payload).(map[string]any)
		list, ok := root["user_entitlement_pack_list"].([]any)
		if !ok {
			return Fail("响应里没有 user_entitlement_pack_list")
		}
		q := Quota{}
		var total float64
		for _, item := range list {
			entry, _ := item.(map[string]any)
			base := objField(entry, "entitlement_base_info")
			quota := objField(base, "quota")
			remaining, ok := numeric(quota["credits_remaining"])
			if !ok {
				limit, hasLimit := numeric(quota["credits_limit"])
				used, hasUsed := numeric(objField(entry, "usage")["credits_amount"])
				if !hasLimit || !hasUsed {
					continue
				}
				remaining = limit - used
			}
			if remaining <= 0 {
				continue
			}
			ms, dated := ParseExpiryValue(entry["expire_time"])
			if !dated {
				ms, dated = ParseExpiryValue(quota["expire_time"])
			}
			if dated {
				if ms <= time.Now().UnixMilli() {
					continue
				}
				if !q.Known || ms < q.ExpiryMS {
					q.Known, q.ExpiryMS = true, ms
				}
			}
			total += remaining
		}
		if total <= 0 {
			return Unusable()
		}
		q.Remaining = strconv.FormatFloat(total, 'f', -1, 64) + " 积分"
		q.Never = !q.Known
		return q
	},

	"officeace": officeaceQuota,
}

// --- 未接线的通用兜底说明 -----------------------------------------------------

// ChannelQuotaDetail 是给面板的一句话：这家用的是哪条判据、这次读到什么。
func ChannelQuotaDetail(spec Spec, q Quota) string {
	switch {
	case q.Failed:
		return q.Detail
	case q.Known:
		return "读到真实到期时刻（" + strconv.FormatInt(q.ExpiryMS, 10) + "）"
	case q.Never:
		if q.Detail != "" {
			return q.Detail
		}
		return "只有永久/长期额度"
	default:
		if q.Detail != "" {
			return q.Detail
		}
		return "上游说这份额度不可用"
	}
}

// buddyQuota 解析腾讯 buddy 系（workbuddy / CodeBuddy）的资源包。
//
// ⚠️ 结构是 **data.Response.Data.Accounts[]**（两层嵌套），这是该接口最容易
// 解析错的地方：只下钻一层会拿到一个没有 Accounts 的对象，然后一切判成不可用，
// 而响应其实是 200。
func buddyQuota(_ Spec, _ Account, payload any) Quota {
	root, _ := payload.(map[string]any)
	if root == nil {
		return Fail("额度响应不是对象")
	}
	if code, ok := numeric(root["code"]); ok && code != 0 {
		return Fail("上游业务码 " + strconv.Itoa(int(code)))
	}
	data := objField(root, "data")
	if data == nil {
		return Fail("响应里没有 data（可能返回了 HTML 错误页）")
	}
	resp := objField(data, "Response")
	if resp == nil {
		// 首字母大小写两种形态都试：上游改过这个键的大小写。
		resp = objField(data, "response")
	}
	if resp == nil {
		return Fail("响应里没有 Response（两层嵌套的第一层没对上）")
	}
	inner := objField(resp, "Data")
	if inner == nil {
		inner = objField(resp, "data")
	}
	if inner == nil {
		return Fail("响应里没有 Data（两层嵌套的第二层没对上）")
	}
	accounts, _ := inner["Accounts"].([]any)
	if len(accounts) == 0 {
		accounts, _ = inner["accounts"].([]any)
	}
	if len(accounts) == 0 {
		return Fail("响应里没有 Accounts 数组")
	}
	now := time.Now().UnixMilli()
	q := Quota{}
	var remain float64
	active := 0
	for _, item := range accounts {
		rec, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Status == 3 是服务端标记「已过期」（实测某账号 5 个包里两个带这个值）。
		if st, ok := numeric(rec["Status"]); ok && st == 3 {
			continue
		}
		expired := int64(0)
		if raw, has := rec["ExpiredTime"]; has {
			if ms, ok := ParseExpiryValue(raw); ok {
				expired = ms
			}
		}
		deduction := int64(0)
		if raw, has := rec["DeductionEndTime"]; has {
			if ms, ok := ParseExpiryValue(raw); ok {
				deduction = ms
			}
		}
		if expired > 0 && now >= expired {
			continue
		}
		if deduction > 0 && now >= deduction {
			continue
		}
		balance, hasBalance := numeric(rec["CycleCapacityRemain"])
		if !hasBalance {
			balance, hasBalance = numeric(rec["CapacityRemain"])
		}
		if !hasBalance || balance <= 0 {
			continue
		}
		active++
		remain += balance
		// 有扣费截止时刻的包才是「临期」的；只有 ExpiredTime 的包按它算。
		clock := deduction
		if clock == 0 {
			clock = expired
		}
		if clock > 0 && (!q.Known || clock < q.ExpiryMS) {
			q.Known, q.ExpiryMS = true, clock
		}
	}
	if remain > 0 {
		q.Remaining = strconv.FormatFloat(remain, 'f', -1, 64) + " 积分"
	}
	if q.Known {
		return q
	}
	if active > 0 {
		// 有有效包但一个都没有扣费截止时刻：按「永久」参与（不是剔除）。
		q.Never = true
		q.Detail = "有有效资源包，但没有下发扣费截止时刻"
		return q
	}
	// 一个有效包都没有：额度确实用光了（0 是「已用光」的语义，这里也确实是）。
	q.Detail = "没有有效资源包（都已过期或已扣费终止）"
	return q
}

func officeaceQuota(_ Spec, _ Account, payload any) Quota {
	root, _ := flatten(payload).(map[string]any)
	if root == nil {
		return Fail("额度响应不是对象")
	}
	groups, hasGroups := root["creditGroups"].([]any)
	if !hasGroups {
		for _, item := range arrField(root, "skus") {
			sku, _ := item.(map[string]any)
			code, _ := sku["sku_code"].(string)
			for _, item := range arrField(sku, "quotas") {
				rec, _ := item.(map[string]any)
				if rec["sku_attr_code"] != "officeace_points" {
					continue
				}
				total, tOK := numeric(rec["sku_value"])
				used, uOK := numeric(rec["current_value"])
				if !tOK || !uOK {
					continue
				}
				group := map[string]any{"remaining": total - used}
				if !strings.HasSuffix(code, ".trial") {
					group["expiresAt"] = rec["expired_time"]
				}
				groups = append(groups, group)
			}
		}
		for _, item := range arrField(root, "bonus_skus") {
			rec, _ := item.(map[string]any)
			total, tOK := numeric(rec["points"])
			used, uOK := numeric(rec["current_value"])
			if tOK && uOK {
				groups = append(groups, map[string]any{"remaining": total - used, "expiresAt": rec["expired_time"]})
			}
		}
	}
	q := Quota{}
	var total float64
	for _, item := range groups {
		rec, _ := item.(map[string]any)
		remaining, ok := numeric(rec["remaining"])
		if !ok {
			remaining, ok = numeric(rec["remainingCredits"])
		}
		if !ok || remaining <= 0 || rec["expired"] == true {
			continue
		}
		if ms, ok := ParseExpiryValue(rec["expiresAt"]); ok {
			if ms <= time.Now().UnixMilli() {
				continue
			}
			if !q.Known || ms < q.ExpiryMS {
				q.Known, q.ExpiryMS = true, ms
			}
		}
		total += remaining
	}
	if total <= 0 {
		return Unusable()
	}
	q.Remaining = strconv.FormatFloat(total, 'f', -1, 64) + " 积分"
	q.Never = !q.Known
	return q
}
