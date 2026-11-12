// Package chprov 是「渠道的授权获取 + 额度读取」的统一实现层。
//
// # 为什么要有这一层
//
// 渠道表（channels 包）只登记「这家叫什么、协议族、认证形态、参不参与轮换」，
// 全是编译期事实。真正要做的两件事——**怎么拿到授权**、**怎么读出这份额度何时
// 作废**——此前只有 workbuddy 与 qoder 有实现，其余 27 家一律参不了轮换：它们
// 在面板上看得见、直连可用，但排不进候选链。用户打开「聚合」面看到的是
// 「3 家参与轮换」，而目录里有 30 家 —— 缺的那 27 家不是「没额度」，是「没接线」。
//
// # 三层结构（与参考实现 dsh-providers-hub 同形）
//
//	registry.go  编译期描述表：每家的端点、请求头形态、解析器 id、登录形态
//	quota.go     额度读取：按描述表发请求 → 归一成 chprov.Quota
//	auth.go      授权获取：设备码 / PKCE 回环 / 状态轮询 / 微信扫码 / 短信 / 贴 Key
//
// 描述表是**唯一真源**：登录端点与额度端点写在同一条记录里，不存在「加了一处、
// 忘了另一处」的缝（参考实现为这种缝踩过七次同一个坑，症状恒定静默：模型层
// 看起来接好了，候选层却永远轮不到那一家，零报错零日志）。
//
// # 三条不许越过的线
//
//  1. **绝不编造数字**：拿不到额度就报「读不到」，不猜一个「明天零点重置」。
//     上游没承诺过的时刻写进排序，就是拿未知当可用。
//  2. **「读不到」不等于「没有额度」**：前者是网络/进程问题（Failed），后者是
//     上游明确说没有（Unusable）。两者的用户动作完全不同，压成一句话会让用户
//     按错误方向排查。
//  3. **凭据只从登录流或用户粘贴来**：本包不内嵌任何账号、不读别人的磁盘缓存。
//     参考实现的「本机浏览器会话 / 客户端 session 文件」那类来源一律不迁。
package chprov

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Account 是某渠道的一份可用凭据。store.Config 逐字段落盘；登录流写回、
// 额度读取与轮换共用同一个快照。
//
// ⚠️ Extra 是这个结构里唯一「形状随渠道变」的地方：zcode 要 device_mid、
// raccoon 要 device_id、astudio 要 cookie、codearts 要 AK/SK。统一成一个
// 强类型结构会逼着 29 家共用一套字段名，而上游字段名本来就各不相同，改名
// 等于把「哪家的哪个字段」这条事实从代码里抹掉。
type Account struct {
	ID          string            `json:"id,omitempty"`
	Nickname    string            `json:"nickname,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
	RefreshTok  string            `json:"refreshToken,omitempty"`
	ExpiresAtMS int64             `json:"expiresAtMs,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
	CreatedAtMS int64             `json:"createdAtMs,omitempty"`
	// Source 记这份凭据是哪条路径来的（device / pkce / poll / qr / sms / paste），
	// 排查「这个账号为什么不会自动续期」时第一眼看的就是它。
	Source string `json:"source,omitempty"`
}

// Accounts 是一家渠道下的一组凭据（多账号）。
//
// # 为什么是多条而不是一条
//
// 用户在腾讯 buddy 上有两个账号、在 trae 上主号加小号 —— 同一家渠道里
// 「一份凭据」永远不够：额度是按账号算的，一个号烧完还有另一个号。
// 此前这里只留一个槽位，症状是「导入读到 22 条、面板上只有 7 家」，
// 而那 15 条被静默覆盖，用户以为它们本来就不存在。
//
// ⚠️ 反序列化对**单条对象**兼容：老配置里 channelAccounts 的每个值是一个
// Account 对象，不是数组。读旧文件时若直接按数组解，整份配置会被判成损坏
// （loadJSON 会把 config.json 改名成 .corrupt-*），用户看到的是一夜之间全部失配。
type Accounts []Account

// UnmarshalJSON 兼容两种形态：一条对象（旧配置）或一个数组（新配置）。
func (as *Accounts) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || string(t) == "null" {
		*as = nil
		return nil
	}
	if t[0] == '[' {
		var list []Account
		if err := json.Unmarshal(t, &list); err != nil {
			return err
		}
		*as = list
		return nil
	}
	var one Account
	if err := json.Unmarshal(t, &one); err != nil {
		return err
	}
	*as = Accounts{one}
	return nil
}

// Usable 返回真正发得出请求的那几条（Empty 的不算）。
func (as Accounts) Usable() Accounts {
	out := make(Accounts, 0, len(as))
	for _, a := range as {
		if !a.Empty() {
			out = append(out, a)
		}
	}
	return out
}

// Key 是这条凭据在这个渠道里的稳定身份，用于「加一条时不要重复」「退出指定一条」。
//
// ⚠️ 它不能只用 AccessToken：刷新过一次令牌之后同一个账号会得到一个新 key，
// 于是面板上会出现两个「同一个账号」，用户的退出动作也只清掉其中一个。
// 优先用上游给的账号 id（uid / user_id / 昵称），令牌只作为最后兜底。
func (a Account) Key() string {
	if v := strings.TrimSpace(a.ID); v != "" {
		return "id:" + v
	}
	for _, k := range []string{"uid", "user_id", "account_id", "machine_id", "device_mid", "device_id"} {
		if v := a.Field(k); v != "" {
			return k + ":" + v
		}
	}
	if v := strings.TrimSpace(a.Nickname); v != "" {
		return "nick:" + v
	}
	// 兜底：令牌摘要的前 8 字节。够区分两台设备，又不在面板上回显令牌。
	sum := sha256.Sum256([]byte(a.AccessToken + "|" + a.Field("cookie") + "|" + a.Field("access_key_id")))
	return "tok:" + hex.EncodeToString(sum[:4])
}

// Label 是面板上这一条的名字。上游给了昵称就用它，没给就用身份 key 的短尾，
// 不留一片"账号 1 / 账号 2"的匿名编号：那种编号在两台机器上是同一串，
// 用户分不出该退哪一个。
func (a Account) Label() string {
	if v := strings.TrimSpace(a.Nickname); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.ID); v != "" {
		return v
	}
	key := a.Key()
	if i := strings.Index(key, ":"); i >= 0 {
		v := key[i+1:]
		if len(v) > 12 {
			v = v[:12] + "…"
		}
		return v
	}
	return key
}

// Upsert 把一条凭据放进集合：同 Key 的覆盖（换令牌不新增一行），否则追加。
// 返回值是「新增了没有」，面板据此说「已添加一个账号」还是「已更新」。
func (as *Accounts) Upsert(a Account) (added bool) {
	key := a.Key()
	for i := range *as {
		if (*as)[i].Key() == key {
			// 保留原来的创建时刻：这一行不是新账号，年龄不该被刷新。
			if a.CreatedAtMS == 0 {
				a.CreatedAtMS = (*as)[i].CreatedAtMS
			}
			(*as)[i] = a
			return false
		}
	}
	*as = append(*as, a)
	return true
}

// Remove 按 Key 去掉一条，返回去掉了几条。
func (as Accounts) Remove(key string) (Accounts, bool) {
	out := make(Accounts, 0, len(as))
	hit := false
	for _, a := range as {
		if a.Key() == key {
			hit = true
			continue
		}
		out = append(out, a)
	}
	return out, hit
}

// Empty 报告这份凭据是否根本没有可用的凭据材料。
//
// ⚠️ 必须把**每一条**准入材料都算上：codearts 是 AK/SK 三件套、loomy 是签名密钥、
// officeace 是 Cookie —— 它们**都没有** access_token。只判 token 与 cookie 会把
// 这些渠道判成「没登录」，而它们的额度端点其实完全能读（后果是那些渠道在候选表里
// 永远轮不到，且零日志）。
func (a Account) Empty() bool {
	if strings.TrimSpace(a.AccessToken) != "" {
		return false
	}
	for _, k := range []string{"cookie", "access_key_id", "access_key", "session"} {
		if strings.TrimSpace(a.Extra[k]) != "" {
			return false
		}
	}
	return true
}

// Field 读一个附加字段，缺失返回空串。
func (a Account) Field(k string) string {
	if a.Extra == nil {
		return ""
	}
	return strings.TrimSpace(a.Extra[k])
}

// Quota 是一次额度读取的结论。四种状态严格互斥：
//
//	Known   拿到了真实到期时刻（ExpiryMS 有效），参与排序，临期先烧
//	Never   可用但只有永久/长期额度 —— 参与排序，排在有临期之后，**可以当选**
//	Failed  这次**读不到**（5xx / 超时 / 未登录）—— 参与排序，排在所有已知之后
//	（零值）上游明确说这份额度不可用 —— 剔除
//
// ⚠️ Failed 与零值必须分开：把一次 502 折成「不可用」，等于让一次网络抖动把某家
// 渠道从轮换里**静默删掉**（面板上还在，候选表里永远轮不到，没有日志能解释）。
// 本项目的口径是：读失败 = 临期未知，仍可作为最后一站。
type Quota struct {
	Known    bool   `json:"known,omitempty"`
	Never    bool   `json:"never,omitempty"`
	Failed   bool   `json:"failed,omitempty"`
	ExpiryMS int64  `json:"expiryMs,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Remaining 是上游给的剩余量原文（"1,900 积分" / "42 次"）。它不进排序，
	// 只给面板显示 —— 没有它，用户看到「永久额度」也不知道还剩多少。
	Remaining string `json:"remaining,omitempty"`
}

// Unusable 是「上游明确说这份额度不可用」的零值。
func Unusable() Quota { return Quota{} }

// Fail 构造一次「读不到」。
func Fail(detail string) Quota { return Quota{Failed: true, Detail: detail} }

// OK 报告这份额度能不能当选（Known / Never 都算）。
func (q Quota) OK() bool { return q.Known || q.Never }

// --- HTTP -------------------------------------------------------------------

// HTTPTimeout 是单次额度读取的上限。取 8 秒：这是面板上逐账号串行的读数，
// 一家 30 秒会让「打开聚合面」变成几十秒（参考实现实测：31 个账号 × 30 秒
// 让冷启动到了 222 秒，用户报「配置一直加载不出来」）。
const HTTPTimeout = 8 * time.Second

// maxBody 是响应体上限。额度响应都是小 JSON，超出的部分一定是别的东西。
const maxBody = 1 << 20

// Client 是额度与登录共用的 HTTP 客户端。按渠道超时各自覆盖（登录轮询要等）。
var Client = &http.Client{Timeout: HTTPTimeout}

// HTTPGet 发一个带自定义头的 GET，返回状态码与响应体。
func HTTPGet(ctx context.Context, url string, headers map[string]string) (int, []byte, error) {
	return do(ctx, http.MethodGet, url, headers, nil)
}

// HTTPPost 发一个带自定义头的 POST。
func HTTPPost(ctx context.Context, url string, headers map[string]string, body any) (int, []byte, error) {
	var raw []byte
	switch v := body.(type) {
	case nil:
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return 0, nil, err
		}
		raw = b
	}
	return do(ctx, http.MethodPost, url, headers, raw)
}

func do(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		if strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// --- 到期时刻归一 -------------------------------------------------------------

// neverExpiryYear 之上的一切时刻都按「永久」处理。上游常用 9999-12-31 当永久
// 哨兵（qoder 的 userQuota.expiresAt 就是），直接读会让面板渲染出「约 2900000
// 天后作废」的荒谬文案。
const neverExpiryYear = 9000

// ExpiryKeys 是各渠道报文里表示「何时作废」的字段名，按可信度排序。
//
// ⚠️ 只登记**实测见过**的名字。猜一个字段名会把它读成 0（不可用），那比不读
// 更糟：渠道本来能选，却被静默剔除。
var ExpiryKeys = []string{
	// 腾讯 buddy 系（workbuddy / CodeBuddy）是 **PascalCase**，且余额在
	// data.Response.Data.Accounts[] 里。⚠️ 键名大小写敏感：只登记 camelCase 时
	// 这家会静默读成零值（面板显示「上游说不可用」），而凭据完全正常。
	"DeductionEndTime", "ExpiredTime", "CycleEndTime", "CycleStartTime",
	"deductionEndTime", "deductionEndAt", "deduction_end_time",
	"expiresAt", "expires_at", "expireAt", "expireTime", "expire_at_ms",
	"expiryMs", "validUntil", "endTime", "quotaExpiresAt",
	"expireTimeMs", "nextExpireTime", "wallet_expires_at", "effectiveAt",
}

// FindExpiry 在任意深度的对象树里找第一个「像到期时刻」的字段。
//
// 只往下走 map，不遍历数组：额度响应里的数组多是逐笔明细，取第一条明细的到期
// 时刻冒充「这份额度的到期」是编造。
func FindExpiry(v any, depth int) (int64, bool) {
	if depth <= 0 {
		return 0, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, k := range ExpiryKeys {
		if raw, has := m[k]; has {
			if ms, ok := ParseExpiryValue(raw); ok {
				return ms, true
			}
		}
	}
	for _, child := range m {
		if ms, ok := FindExpiry(child, depth-1); ok {
			return ms, true
		}
	}
	return 0, false
}

// ParseExpiryValue 把字段值折成毫秒：数字按秒/毫秒自适应（<1e12 视为秒），
// 字符串按 RFC3339、几个常见本地布局、纯数字串依次尝试。
func ParseExpiryValue(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		return NormalizeExpiryMS(int64(v))
	case int64:
		return NormalizeExpiryMS(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return NormalizeExpiryMS(n)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return NormalizeExpiryMS(n)
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

// NormalizeExpiryMS 统一到毫秒，并挡掉哨兵与明显无效的值。
func NormalizeExpiryMS(ms int64) (int64, bool) {
	if ms <= 0 {
		return 0, false
	}
	if ms < 1e12 {
		ms *= 1000 // 上游写的是秒
	}
	if ms/1000 > time.Now().AddDate(100, 0, 0).Unix() {
		return 0, false // 100 年之后一律视为哨兵
	}
	return ms, true
}

// MinExpiry 从一份报文里取出「最早作废」那个时刻（逐包额度用）。
//
// ⚠️ 与 FindExpiry 的分工：FindExpiry 回答「整份额度何时作废」，本函数回答
// 「这批包里最早的一条何时作废」。逐包渠道（minimax / zcode / officeace）必须
// 用后者：取第一条包的时刻当整份额度是编造。
func MinExpiry(body []byte) (int64, bool) {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return 0, false
	}
	return minExpiryIn(payload, 5, false)
}

func minExpiryIn(v any, depth int, inPkg bool) (int64, bool) {
	if depth <= 0 || v == nil {
		return 0, false
	}
	var best int64
	seen := false
	consider := func(ms int64, ok bool) {
		if !ok {
			return
		}
		if !seen || ms < best {
			best, seen = ms, true
		}
	}
	switch t := v.(type) {
	case map[string]any:
		for _, k := range ExpiryKeys {
			if raw, has := t[k]; has {
				consider(ParseExpiryValue(raw))
			}
		}
		// key 是余额字段（used/remaining 之类）就说明这一层是「一笔额度」，
		// 它的到期才值得计入最早作废。
		for _, k := range []string{"remaining", "remainingUnits", "remaining_units", "availableUnits", "left"} {
			if _, has := t[k]; has {
				inPkg = true
			}
		}
		for _, child := range t {
			if ms, ok := minExpiryIn(child, depth-1, inPkg); ok {
				consider(ms, true)
			}
		}
	case []any:
		for _, e := range t {
			if ms, ok := minExpiryIn(e, depth-1, inPkg); ok {
				consider(ms, true)
				_ = inPkg
			}
		}
	}
	return best, seen
}

// HasBalance reports positive remaining credit, never package size/usage limits.
//
// ⚠️ 这条判据不能省：一个 200 但字段全空的响应如果退成 Never，这家渠道会被
// 当成「永久可用」排进轮换，而它可能根本没登录。
func HasBalance(v any, depth int) bool {
	if depth <= 0 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"credit", "credits", "balance", "remainCredit",
			"point", "points", "quota", "remain", "left", "remaining", "remainingUnits",
			"availableUnits", "total_balance",
			// buddy 系的本周期口径（PascalCase）：只认 camelCase 会让这家
			// 有额度也被判成「不可用」，且没有任何线索指向字段名。
			"CycleCapacityRemain", "CapacityRemain"} {
			if f, ok := toNum(t[k]); ok && f > 0 {
				return true
			}
		}
		for _, child := range t {
			if HasBalance(child, depth-1) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if HasBalance(e, depth-1) {
				return true
			}
		}
	}
	return false
}

func toNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

// ErrNoAccount 是「这家渠道当前没有可用账户」。
var ErrNoAccount = errors.New("chprov: 该渠道没有可用账号")

// ErrNoSpec 是「这家渠道没有登记额度端点」。
var ErrNoSpec = errors.New("chprov: 该渠道没有登记额度端点")

// short 把一段响应体截成可读的诊断串。
func short(data []byte) string {
	s := scrubSecrets(strings.TrimSpace(string(data)))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// STATUS 拼一句带状态码的失败原因（与各家 plugin 的文案同形）。
func statusText(code int, data []byte) string {
	return fmt.Sprintf("HTTP %d %s", code, short(data))
}
