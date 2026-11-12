package lane

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"zenui/internal/zen/chprov"
)

// QuotaExpiry is what a channel's quota endpoint says about **when** its balance
// stops being usable. It is the only input to cross-channel 选型 ordering
// (见 aggregate.Rank 的「临期升序」), so the three outcomes are kept apart
// instead of being folded into one number:
//
//	Known  拿到了真实到期时刻，ExpiryMS 有效
//	Never  可用，但只有永久/长期积分 —— 参与排序，排在有临期的之后，**可以当选**
//	（零值）本次不可用，或拿不到任何额度信息 —— 调用方按「不可用」剔除
//
// ⚠️ 零值 **不是** Never。把「读不到」写成 Never 会让一个额度可能早就烧光的渠道
// 进入轮换，且失败是静默的（请求打过去才 429）。两条路径的判据必须分开写。
type QuotaExpiry struct {
	Known    bool
	Never    bool
	ExpiryMS int64
	// Failed 表示**这次读取失败了**（端点 5xx / 超时 / sidecar 未响应），
	// 与「读到了，但这份额度不可用」不是同一件事。
	//
	// ⚠️ 必须分开：把一个偶发的 502 折成「不可用」，等于让一次网络抖动把某家渠道
	// 从轮换里**静默删掉** —— 面板上它还在，候选表里却永远轮不到它，而没有任何
	// 日志能解释为什么。本项目的口径是：读失败 = 临期未知，排在所有已知之后，
	// 仍可作为最后一站；读到不可用 = 剔除。
	Failed bool
}

// Unusable 是「本次不可用」的零值，读起来比 struct{} 明确。
func (QuotaExpiry) Unusable() QuotaExpiry { return QuotaExpiry{} }

// neverExpiryYear 之上的一切时刻都按「永久」处理。
//
// 上游常用 9999-12-31 当「永久」哨兵（qoder 的 userQuota.expiresAt 就是），
// 直接读它会让面板渲染出「约 2900000 天后作废」的荒谬文案，而排序上它又会
// 排到所有临期之后 —— 结果正确、文案荒谬。故在入口处就折算成 Never。
const neverExpiryYear = 9000

// expiryKeys 是各渠道额度端点里表示「何时作废」的字段名，按可信度排序。
// 只登记**实测见过**的名字；猜一个字段名会把它读成 0（不可用），那比不读更糟：
// 渠道本来能选，却被静默剔除。
var expiryKeys = []string{
	"deductionEndTime", "deductionEndAt",
	"expiresAt", "expireAt", "expireTime", "expires_at", "expire_at_ms",
	"validUntil", "endTime", "quotaExpiresAt",
}

// findExpiryMS 在任意深度的对象树里找第一个「像到期时刻」的字段。
//
// 只往下走 **map**，不遍历数组：额度响应里数组多是逐笔明细，取第一条明细的
// 到期时刻冒充「这份额度的到期」是编造。字符串与数字都收（上游两种都用过），
// 字符串按几种常见布局解析，解析不出就**跳过**而不是当成 0。
func findExpiryMS(v any, depth int) (int64, bool) {
	if depth <= 0 {
		return 0, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, k := range expiryKeys {
		if raw, has := m[k]; has {
			if ms, ok := parseExpiryValue(raw); ok {
				return ms, true
			}
		}
	}
	for _, child := range m {
		if ms, ok := findExpiryMS(child, depth-1); ok {
			return ms, true
		}
	}
	return 0, false
}

// parseExpiryValue 把字段值折成毫秒：数字按秒/毫秒自适应（<1e12 视为秒），
// 字符串按 RFC3339、几个常见本地布局、以及纯数字串依次尝试。
func parseExpiryValue(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		return normalizeExpiryMS(int64(v))
	case int64:
		return normalizeExpiryMS(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return normalizeExpiryMS(n)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeExpiryMS(n)
		}
		for _, layout := range []string{
			time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05",
			"2006-01-02T15:04:05.000Z07:00", "2006-01-02",
		} {
			if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
				return t.UnixMilli(), true
			}
		}
	}
	return 0, false
}

// normalizeExpiryMS 统一到毫秒，并挡掉哨兵与明显无效的值。
func normalizeExpiryMS(ms int64) (int64, bool) {
	if ms <= 0 {
		return 0, false
	}
	if ms < 1e12 {
		ms *= 1000 // 上游写的是秒
	}
	// 100 年之后一律视为哨兵：没有任何真实额度会写 2126 年之后。
	if ms/1000 > time.Now().AddDate(100, 0, 0).Unix() {
		return 0, false
	}
	return ms, true
}

// expiryFromPayload 读一份额度响应：先找真实到期时刻（Known），找不到但响应里
// **确实有余额**才退成 Never，两者都没有则按不可用。
//
// 「有余额」这条判据不能省：一个 200 但字段全空的响应如果退成 Never，这个渠道
// 会被当成「永久可用」排进轮换，而它可能根本没登录。
func expiryFromPayload(payload map[string]any) QuotaExpiry {
	if payload == nil {
		return QuotaExpiry{}
	}
	if ms, ok := findExpiryMS(payload, 4); ok {
		return QuotaExpiry{Known: true, ExpiryMS: ms}
	}
	if hasBalance(payload, 4) {
		return QuotaExpiry{Never: true}
	}
	return QuotaExpiry{}
}

// hasBalance 报告响应树里有没有一个「余额类」数字字段（正数或 0）。
func hasBalance(v any, depth int) bool {
	if depth <= 0 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"credit", "credits", "balance", "remainCredit", "totalCredit", "point", "points", "quota", "remain", "left"} {
			if f, ok := toNum(t[k]); ok && f >= 0 {
				return true
			}
		}
		for _, child := range t {
			if hasBalance(child, depth-1) {
				return true
			}
		}
	case []any:
		// 数组里只认**元素本身就是余额**的形态，不取某一条冒充总额。
		for _, e := range t {
			if f, ok := toNum(e); ok && f >= 0 {
				return true
			}
		}
	}
	return false
}

// --- WorkBuddy ---------------------------------------------------------

// WorkBuddyExpiry 读计费资源包的到期时刻。这是本项目两个**真有临期来源**的
// 渠道之一（另一个是 qoder sidecar）。
//
// 未登录 / 停用 → 零值（不可用）：没有账号的渠道不该参与轮换。
// 资源包带 deductionEndTime → Known；只有余额、没有时间字段 → Never（长期积分）。
func (l *Lane) WorkBuddyExpiry(ctx context.Context) (QuotaExpiry, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return QuotaExpiry{}, errors.New("WorkBuddy 账号未登录或已停用")
	}
	// wbGet honours ctx now (NewRequestWithContext), so cancellation really
	// aborts the socket. The old goroutine + select only abandoned the caller
	// while a Background-context request kept running for another 15s.
	code, body, getErr := wbGet(ctx, wbUserResourcePath, acct)
	if getErr != nil {
		return QuotaExpiry{}, getErr
	}
	if code != 200 {
		return QuotaExpiry{}, errors.New("积分查询返回 HTTP " + strconv.Itoa(code))
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return QuotaExpiry{}, errors.New("积分响应无法解析")
	}
	q := chprov.ParseQuota("workbuddy", body)
	if q.Failed {
		return QuotaExpiry{Failed: true}, errors.New(q.Detail)
	}
	return QuotaExpiry{Known: q.Known, Never: q.Never, ExpiryMS: q.ExpiryMS}, nil
}

// --- Qoder sidecar -----------------------------------------------------

// QoderExpiry 读本地 sidecar 的额度端点。
//
// 两种真实形态：
//
//	userQuota.expiresAt 是一个**真实时刻**（非哨兵）→ Known
//	userQuota.expiresAt 是 9999-12-31（永久哨兵）→ 折算成 Never（见 normalizeExpiryMS）
//
// 拿不到（sidecar 未运行 / 未登录 / 字段不在）→ 零值（不可用），绝不用「猜一个
// 明天零点」去填：上游没有承诺过每日重置的具体时刻，编一个就是把未知当可用。
func QoderExpiry(ctx context.Context, get func(string) (int, []byte, error)) (QuotaExpiry, error) {
	code, body, err := get("/quota")
	if err != nil {
		return QuotaExpiry{}, err
	}
	if code == 401 {
		return QuotaExpiry{}, errors.New("sidecar 的额度 token 已过期")
	}
	if code != 200 {
		return QuotaExpiry{}, errors.New("额度端点返回 HTTP " + strconv.Itoa(code))
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return QuotaExpiry{}, errors.New("额度响应无法解析")
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = payload
	}
	raw, _ := json.Marshal(data)
	q := chprov.ParseQuota("qoder", raw)
	if q.Failed {
		return QuotaExpiry{Failed: true}, errors.New(q.Detail)
	}
	return QuotaExpiry{Known: q.Known, Never: q.Never, ExpiryMS: q.ExpiryMS}, nil
}
