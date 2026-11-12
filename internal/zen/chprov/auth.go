package chprov

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件是「授权怎么拿到」：四种登录形态的**通用实现**，参数全部来自描述表。
//
// # 为什么登录要走通用实现，而不是逐家抄一遍
//
// 29 家里只有四种形态：设备码（RFC 8628）、本机回环 + PKCE、状态轮询、二维码。
// 同一形态在三家渠道上的差异**全部是参数**（client_id、scope、端点、回调路径），
// 而逐家抄一遍必然漂移 —— 参考实现为「同一语义两处实现」记过多次红线
// （buddy-growth 那类：两处对同一字段给出相反判据，不报错、只是把没做到说成做到了）。
//
// # 每种形态的**真实约束**都写进返回值
//
// 用户点「登录」后要知道三件事：现在该干什么（URL / 二维码 / 贴 Key）、
// 还要等多久（过期时刻）、拿不到时为什么。这三件都是结构字段，不是日志文案。

// LoginSession 是一次进行中的授权。字段即面板要渲染的东西。
type LoginSession struct {
	Channel string `json:"channel"`
	Kind    string `json:"kind"`
	// OpenURL 是「用户现在该打开哪个页面」（设备码 / PKCE / 状态轮询都有）。
	OpenURL string `json:"openUrl,omitempty"`
	// UserCode 是设备码流程里要用户抄进网页的那串（RFC 8628 的 user_code）。
	UserCode string `json:"userCode,omitempty"`
	// QRImage 是二维码流程里上游给的图 URL（前端直接 img src）。
	QRImage string `json:"qrImage,omitempty"`
	// IntervalMS / ExpiresAtMS 是轮询节奏与截止时刻 —— 面板据此倒数。
	IntervalMS  int64 `json:"intervalMs,omitempty"`
	ExpiresAtMS int64 `json:"expiresAtMs,omitempty"`
	// State 是不透明句柄（设备码 / 状态轮询 / PKCE 的 state），轮询时原样带回。
	State string `json:"state,omitempty"`
	// Verifier 是 PKCE 的 code_verifier（只在进程内，不发给前端）。
	Verifier string `json:"-"`
	// DeviceID / Code 是设备码流程里换 token 需要的那两个值。
	DeviceID string `json:"deviceId,omitempty"`
	// PollToken 是 CLI 两段式流程里两段共用的 Bearer（本机铸的那 64 位 hex）。
	//
	// ⚠️ json:"-" 是必须的，不是洁癖：这份会话结构会原样发给前端渲染弹窗，
	// 而这个值本身就是换取 token 的唯一凭据 —— 回显出去等于把授权借给页面。
	// DeviceID 那一格留给设备码形态（RFC 8628 的 device_code 本来就要发给客户端）。
	PollToken string `json:"-"`
	// Detail 是面板上那行说明（为什么没有 refresh 端点这类事实）。
	Detail string `json:"detail,omitempty"`
	// LocalPort 是 PKCE 起在本机的回环端口。
	LocalPort int `json:"localPort,omitempty"`
	// StartedAtMS 用于 10 分钟超时（与 workbuddy 的 wbLoginTTL 同档）。
	StartedAtMS int64 `json:"startedAtMs"`
}

// loginTTL 是一次授权的窗口。10 分钟：与 workbuddy 的 wbLoginTTL 同档，
// 短于它会让用户扫到一半失效，长于它会让面板挂着一个早已作废的会话。
const loginTTL = 10 * time.Minute

// sessions 是进程内的授权会话表，按渠道各自一条（同一家不该有两个并发的登录）。
var (
	sessMu  sync.Mutex
	sessTab = map[string]*LoginSession{}
)

// StartLogin 按描述表的形态开一次授权。
func StartLogin(ctx context.Context, id string) (LoginSession, error) {
	spec, ok := SpecOf(id)
	if !ok {
		return LoginSession{}, errors.New("渠道 " + id + " 没有登记在本表里")
	}
	switch spec.Login.Kind {
	case LoginPaste:
		// 贴 Key 不需要会话：面板直接把输入框给用户，确认后调 SaveToken。
		return LoginSession{
			Channel: id, Kind: LoginPaste,
			Detail:      spec.Login.PasteHint,
			UserCode:    spec.Login.PasteLabel,
			StartedAtMS: time.Now().UnixMilli(),
		}, nil
	case LoginDevice:
		return startDevice(ctx, spec)
	case LoginPoll:
		return startPoll(ctx, spec)
	case LoginCliFlow:
		return startCliFlow(ctx, spec)
	case LoginPKCE:
		return startPKCE(spec)
	case LoginQR:
		// 二维码有两种来源：授权页里内嵌 uuid（微信），或本机生成 code 并自己
		// 渲染图（raccoon）。用描述表的字段区分，不靠渠道 id 特判。
		if spec.Login.QRContent != "" {
			return startQRContent(ctx, spec)
		}
		return startQRPage(ctx, spec)
	case LoginTicket:
		return startTicket(ctx, spec)
	case LoginClaw:
		return startClaw(ctx, spec)
	case LoginQoder:
		return startQoder(ctx, spec)
	}
	return LoginSession{}, errors.New("未知的登录形态 " + spec.Login.Kind)
}

// remember 记一条会话（同渠道覆盖旧的：并发的两次登录会互相顶掉 token，
// 而用户看到的只是「第二个也没成功」，没有任何线索指出是并发导致的）。
func remember(s LoginSession) LoginSession {
	sessMu.Lock()
	sessTab[s.Channel] = &s
	sessMu.Unlock()
	return s
}

// Session 读回一条会话（轮询用）。
func Session(id string) (LoginSession, bool) {
	sessMu.Lock()
	defer sessMu.Unlock()
	s, ok := sessTab[id]
	if !ok {
		return LoginSession{}, false
	}
	return *s, true
}

// Forget 丢掉会话（登录成功或用户取消）。
func Forget(id string) {
	sessMu.Lock()
	delete(sessTab, id)
	sessMu.Unlock()
}

// --- 设备码（RFC 8628）--------------------------------------------------------

// startDevice 发一次设备码请求。
//
// 三家真实形态：
//
//	minimax   POST /oauth2/device/code {client_id,scope} → {device_code, user_code, verification_uri}
//	cline     WorkOS POST /user_management/authorize/device {client_id} → 同上
//
// ⚠️ 两家的**参数位置不同**：minimax 要 form，WorkOS 要 JSON。写成一份 body 会让
// 其中一家 400，而错误文案是 generic 的 invalid_request —— 排查时看不出是位置的问题。
func startDevice(ctx context.Context, spec Spec) (LoginSession, error) {
	body := map[string]any{
		"client_id":  spec.Login.ClientID,
		"grant_type": "urn:ietf:params:oauth:grant-type:device_code",
	}
	if spec.Login.Scope != "" {
		body["scope"] = spec.Login.Scope
	}
	// PKCE：有些上游（minimax 两家）把 S256 challenge 当**必填**，缺了直接 400
	// 「valid S256 PKCE is required」。verifier 只留在进程内（LoginSession 的
	// json:"-"），换 token 时原样带回，不发给前端。
	verifier := ""
	if spec.Login.PKCE {
		v, c, err := pkcePair()
		if err != nil {
			return LoginSession{}, errors.New("生成 PKCE 对失败：" + err.Error())
		}
		verifier = v
		body["code_challenge"] = c
		body["code_challenge_method"] = "S256"
	}
	headers := map[string]string{"accept": "application/json"}
	var code int
	var raw []byte
	var err error
	if strings.Contains(spec.ID, "cline") {
		// WorkOS 收 JSON。
		code, raw, err = HTTPPost(ctx, spec.Login.DeviceURL, headers, body)
	} else {
		// minimax 收 form（实测它不认 JSON body）。
		form := url.Values{}
		for k, v := range body {
			// The device endpoint may add numeric fields (expires_in, interval).
			// An unchecked string assertion here panicked the whole process on
			// any non-string addition.
			form.Set(k, fmt.Sprint(v))
		}
		headers["content-type"] = "application/x-www-form-urlencoded"
		code, raw, err = HTTPPost(ctx, spec.Login.DeviceURL, headers, form.Encode())
	}
	if err != nil {
		return LoginSession{}, err
	}
	if code < 200 || code >= 300 {
		return LoginSession{}, errors.New("设备码请求失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return LoginSession{}, errors.New("设备码响应无法解析：" + short(raw))
	}
	interval := int64(5000)
	if v, ok := numeric(payload["interval"]); ok && v > 0 {
		interval = int64(v) * 1000
	}
	expires := time.Now().Add(loginTTL).UnixMilli()
	if v, ok := numeric(payload["expires_in"]); ok && v > 0 {
		expires = time.Now().Add(time.Duration(v) * time.Second).UnixMilli()
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginDevice,
		State:       field(payload, "device_code"),
		UserCode:    field(payload, "user_code"),
		OpenURL:     field(payload, "verification_uri"),
		IntervalMS:  interval,
		ExpiresAtMS: expires,
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
		Verifier:    verifier,
	}
	if s.OpenURL == "" {
		s.OpenURL = field(payload, "verification_url")
	}
	if s.OpenURL == "" {
		s.OpenURL = field(payload, "verification_uri_complete")
	}
	return remember(s), nil
}

// PollDevice 轮一次设备码：pending / ok。
func PollDevice(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", sess.State)
	form.Set("client_id", spec.Login.ClientID)
	// 发码时带了 challenge 的，换 token 时必须带同一个 verifier，否则上游回
	// 「PKCE verification failed」——那时用户已经在浏览器里点完了，报错看起来
	// 像「授权失败」，其实缺的只是这一步。
	if sess.Verifier != "" {
		form.Set("code_verifier", sess.Verifier)
	}
	headers := map[string]string{"accept": "application/json"}
	var code int
	var raw []byte
	var err error
	if strings.Contains(spec.ID, "cline") {
		body := map[string]any{
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
			"device_code": sess.State,
			"client_id":   spec.Login.ClientID,
		}
		if sess.Verifier != "" {
			body["code_verifier"] = sess.Verifier
		}
		code, raw, err = HTTPPost(ctx, spec.Login.TokenURL, headers, body)
	} else {
		headers["content-type"] = "application/x-www-form-urlencoded"
		code, raw, err = HTTPPost(ctx, spec.Login.TokenURL, headers, form.Encode())
	}
	if err != nil {
		return "", Account{}, err
	}
	if code == http.StatusPreconditionFailed || code == http.StatusBadRequest {
		// RFC 8628 的 authorization_pending 就走 400。**不要**当成失败：
		// 设备码流程的正常态就是「用户还没点确认」。
		return "pending", Account{}, nil
	}
	if code < 200 || code >= 300 {
		return "", Account{}, errors.New("设备码轮询失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("换 token 响应无法解析：" + short(raw))
	}
	token := field(payload, "access_token")
	if token == "" {
		// 有些实现把 pending 放在 200 + error 字段里（WorkOS 就是）。
		if e := field(payload, "error"); e != "" {
			if e == "authorization_pending" || e == "slow_down" {
				return "pending", Account{}, nil
			}
			return "", Account{}, errors.New("上游拒绝换 token：" + e)
		}
		return "", Account{}, errors.New("换 token 响应里没有 access_token")
	}
	acct := Account{
		AccessToken: token,
		RefreshTok:  field(payload, "refresh_token"),
		Source:      LoginDevice,
		CreatedAtMS: time.Now().UnixMilli(),
	}
	if v, ok := numeric(payload["expires_in"]); ok && v > 0 {
		acct.ExpiresAtMS = time.Now().Add(time.Duration(v) * time.Second).UnixMilli()
	}
	if id := field(payload, "account_id"); id != "" {
		acct.Extra = map[string]string{"account_id": id}
	}
	return "ok", acct, nil
}

// --- 状态轮询（workbuddy / zcode 形态）------------------------------------------

// startPoll 让上游铸一个 state 与授权页，用户去浏览器完成，这里轮询换 token。
//
// ⚠️ 与设备码的差别：**没有 user_code**，上游自己开一个浏览器会话；
// 用户只需点开那个 URL。这是 workbuddy 与 CodeBuddy 的真实形态。
func startPoll(ctx context.Context, spec Spec) (LoginSession, error) {
	headers := map[string]string{"content-type": "application/json", "accept": "application/json"}
	if strings.Contains(spec.Login.StartURL, "workbuddy") || strings.Contains(spec.Login.StartURL, "copilot.tencent") {
		headers["x-codebuddy-request"] = "1"
		headers["user-agent"] = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	}
	code, raw, err := HTTPPost(ctx, spec.Login.StartURL, headers, "{}")
	if err != nil {
		return LoginSession{}, err
	}
	if code < 200 || code >= 300 {
		return LoginSession{}, errors.New("发起登录失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return LoginSession{}, errors.New("登录状态响应无法解析：" + short(raw))
	}
	data := flatten(payload)
	m, _ := data.(map[string]any)
	state := field(m, "state")
	open := field(m, "authUrl")
	if open == "" {
		open = field(m, "auth_url")
	}
	if open == "" {
		open = field(m, "url")
	}
	if state == "" || open == "" {
		return LoginSession{}, errors.New("上游没有给出 state / 授权页地址")
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginPoll,
		State: state, OpenURL: open,
		IntervalMS: 2000, ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(), Detail: spec.Login.Note,
	}
	return remember(s), nil
}

// PollState 轮一次状态登录。workbuddy 形态：GET ?state=…，code 11217 = 还没完成。
func PollState(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	head := map[string]string{"accept": "application/json"}
	sep := "?"
	if strings.Contains(spec.Login.PollURL, "?") {
		sep = "&"
	}
	code, raw, err := HTTPGet(ctx, spec.Login.PollURL+sep+"state="+url.QueryEscape(sess.State), head)
	if err != nil {
		return "", Account{}, err
	}
	if code < 200 || code >= 300 {
		return "", Account{}, errors.New("登录轮询失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("登录轮询响应无法解析：" + short(raw))
	}
	if v, ok := numeric(payload["code"]); ok && v == 11217 {
		return "pending", Account{}, nil
	}
	data := flatten(payload)
	m, _ := data.(map[string]any)
	token := field(m, "accessToken")
	if token == "" {
		token = field(m, "access_token")
	}
	if token == "" {
		if msg := field(payload, "msg"); msg != "" {
			return "", Account{}, errors.New("登录未完成：" + msg)
		}
		return "", Account{}, errors.New("登录未完成（响应里没有 accessToken）")
	}
	acct := Account{
		AccessToken: token,
		RefreshTok:  field(m, "refreshToken"),
		Source:      LoginPoll,
		CreatedAtMS: time.Now().UnixMilli(),
	}
	if acct.RefreshTok == "" {
		acct.RefreshTok = field(m, "refresh_token")
	}
	// uid / 到期时刻从 JWT 读（上游把两者都写在令牌里）。
	if sub, _, exp, ok := JWTClaims(token); ok {
		acct.ID = sub
		acct.ExpiresAtMS = exp
		acct.Extra = map[string]string{"uid": sub}
	}
	return "ok", acct, nil
}

// --- 官方 CLI 的两段式设备授权（zcode 形态）----------------------------------
//
// 上游契约（逐字段实测）：
//
//	POST init  Authorization: Bearer <本机铸的 pollToken>  body {"provider":"zai"}
//	           → {flow_id, poll_token, authorize_url, expires_at(秒), poll_interval_sec}
//	GET  poll/<flow_id>  Authorization: Bearer <同一个 pollToken>
//	           → {status:"pending"} / {status:"ready", token, user:{user_id…}, zai:{access_token,…}}
//
// ⚠️ **三条硬约束**，每一条的违反都对应一个具体错误码：
//
//  1. pollToken 必须是 **32 字节随机数的 hex**（64 字符）。实测 32/48 字符得
//     invalid_flow / 3004，64 与 80 都过 —— 上游校验的是「够长的 Bearer 且
//     未与既有流程重号」，而不是某个固定长度。用 32 字节是官方 CLI 的取值。
//  2. 请求体必须有 provider，写 {} 得 **parameter error / 3001**。那正是面板上
//     那句「发起登录失败」的来源 —— 不是端点坏了，是两段式契约被当成轮询用了。
//  3. 授权页由上游给（authorize_url 指向 chat.z.ai），本机不拼 client_id：
//     client_P8X5CMWmlaRO9gyO-KSqtg 是官方桌面客户端的公开 id，写进本项目只会
//     在它被轮换时静默失效。

// startCliFlow 发一次 init，拿回 flow_id 与授权页。
func startCliFlow(ctx context.Context, spec Spec) (LoginSession, error) {
	poll := cliPollToken()
	provider := spec.Login.Provider
	if provider == "" {
		provider = spec.ID
	}
	// Bearer 就是那把 pollToken：没有它上游只回 parameter error / 3001，
	// 与「body 写错」同形 —— 这两条错法在面板上都只有一句「发起登录失败」。
	headers := map[string]string{
		"content-type":  "application/json",
		"accept":        "application/json",
		"authorization": "Bearer " + poll,
	}
	body := map[string]any{"provider": provider}
	code, raw, err := HTTPPost(ctx, spec.Login.StartURL, headers, body)
	if err != nil {
		return LoginSession{}, err
	}
	if code < 200 || code >= 300 {
		return LoginSession{}, errors.New("发起登录失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return LoginSession{}, errors.New("登录状态响应无法解析：" + short(raw))
	}
	if bus, ok := numeric(payload["code"]); ok && bus != 0 {
		msg := field(payload, "msg")
		if msg == "" {
			msg = "业务码 " + strconv.Itoa(int(bus))
		}
		return LoginSession{}, errors.New("发起登录失败：" + msg)
	}
	data := flatten(payload)
	m, _ := data.(map[string]any)
	flow := firstNonEmpty(field(m, "flow_id"), field(m, "flowId"))
	open := firstNonEmpty(field(m, "authorize_url"), field(m, "authorizeUrl"))
	if flow == "" || open == "" {
		return LoginSession{}, errors.New("上游没有给出 flow_id / 授权页地址")
	}
	// 轮询节奏与截止时刻都按上游说的走：上游说 2 秒就是 2 秒。写死一个数字会让
	// 「上游调慢了限流」变成我们这边的高频打到 429。
	interval := int64(2000)
	if v, ok := numeric(m["poll_interval_sec"]); ok && v >= 1 {
		interval = int64(v) * 1000
	}
	deadline := time.Now().Add(loginTTL).UnixMilli()
	if v, ok := numeric(m["expires_at"]); ok && v > 0 {
		if ms, ok2 := NormalizeExpiryMS(int64(v)); ok2 {
			deadline = ms
		}
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginCliFlow,
		State: flow, OpenURL: open,
		PollToken:   poll,
		IntervalMS:  interval,
		ExpiresAtMS: deadline,
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	return remember(s), nil
}

// pollCliFlow 轮一次 CLI 授权。ready 时把 jwt 与 zai access token 一起落进凭据：
//
//	jwt  → AccessToken（额度端点认它，实测 200）
//	zai  → Extra["zai_access_token"]（业务登录用，额度端点认它恒 401）
//	user_id → Extra["uid"]
//	device_mid → Extra["device_mid"]（本机铸的 UUID，见下）
//
// ⚠️ 上游的 ready 报文里**没有 device_mid**，而额度端点缺它得 400 code 3001
// ——这一格缺着，登录成功之后紧接着那次额度读取必然失败，面板上就是「登录了
// 但读不到额度」。本项目不读别人的磁盘缓存（chprov 的三条线之一），所以这里
// 自己铸一个：实测任意非空值都过（随机 UUID 与 ZCode 自己的 deviceMid 都拿 200），
// 说明它是**客户端自报的设备标识**，不是上游绑定的凭据。铸在登录流里、跟着这条
// 账号落盘 ⇒ 同一账号永远是同一台机器，换账号换值（正是上游期望的形状）。
func pollCliFlow(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	head := map[string]string{
		"accept":        "application/json",
		"authorization": "Bearer " + sess.PollToken,
	}
	target := strings.TrimRight(spec.Login.PollURL, "/") + "/" + url.PathEscape(sess.State)
	code, raw, err := HTTPGet(ctx, target, head)
	if err != nil {
		return "pending", Account{}, nil // 单次网络抖动不当失败（与 QR 长轮询同口径）
	}
	if code < 200 || code >= 300 {
		return "", Account{}, errors.New("登录轮询失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("登录轮询响应无法解析：" + short(raw))
	}
	if bus, ok := numeric(payload["code"]); ok && bus != 0 {
		msg := field(payload, "msg")
		if msg == "" {
			msg = "业务码 " + strconv.Itoa(int(bus))
		}
		Forget(spec.ID)
		return "", Account{}, errors.New("登录被上游拒了：" + msg)
	}
	data := flatten(payload)
	m, _ := data.(map[string]any)
	switch field(m, "status") {
	case "pending", "":
		return "pending", Account{}, nil
	case "failed":
		Forget(spec.ID)
		return "", Account{}, errors.New("上游说这次授权失败（failed），重新发起一次")
	}
	// ready：token 是 zcode 自己的 jwt，zai.access_token 是业务登录换取的那一份。
	jwt := field(m, "token")
	zai := ""
	if sub, _ := m["zai"].(map[string]any); sub != nil {
		zai = firstNonEmpty(field(sub, "access_token"), field(sub, "accessToken"))
	}
	if jwt == "" {
		forget := field(payload, "msg")
		if forget == "" {
			forget = "响应里没有 token"
		}
		Forget(spec.ID)
		return "", Account{}, errors.New("登录未完成：" + forget)
	}
	extra := map[string]string{"device_mid": uuidV4()}
	if zai != "" {
		extra["zai_access_token"] = zai
	}
	acct := Account{
		AccessToken: jwt,
		Source:      LoginCliFlow,
		CreatedAtMS: time.Now().UnixMilli(),
	}
	if sub, _, exp, ok := JWTClaims(jwt); ok {
		acct.ID = sub
		acct.ExpiresAtMS = exp
		extra["uid"] = sub
	}
	if u := objField(m, "user"); u != nil {
		if id := field(u, "user_id"); id != "" && acct.ID == "" {
			acct.ID = id
		}
		if name := field(u, "name"); name != "" {
			acct.Nickname = name
		}
	}
	acct.Extra = extra
	return "ok", acct, nil
}

// cliPollToken 是 init/poll 两段共用的 Bearer：32 字节随机数的 hex（64 字符）。
//
// ⚠️ 长度不是随便取的：实测 32 / 48 字符得 invalid_flow（3004），64 / 80 都过。
// 这个值**必须两段一致** —— 换了就换不成 token，而上游只回「invalid_flow」，
// 看不出是「你自己换了 Bearer」。
func cliPollToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 读不到熵就退到时间戳派生的伪随机：这里宁可弱一点也不让登录入口消失。
		return zcodeHex(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	return hex.EncodeToString(b)
}

// --- 本机回环 + PKCE ---------------------------------------------------------

// startPKCE 起一个本机回环监听，把授权页给用户。
//
// ⚠️ 这里**真的起一个 127.0.0.1 监听**：PKCE 流程的回调必须落到一个真实端口，
// 而上游把 redirect_uri 当白名单核对（写错端口 = 授权页直接报 redirect_uri_mismatch）。
// 监听器在拿到 code 或超时后自己关闭，**不是**常驻服务：多开一次登录不该留下
// 一个占着端口、再也收不到东西的 goroutine。
//
// ⚠️ 回调只处理**一次**：拿到 code 立刻 close。第二次回调（用户刷新页面）
// 会拿到 410，而不是被当成又一次授权 —— 两次 code 换同一份 token 的话，
// 上游通常会作废后一个，用户看到的是「登录成功了但我又是未登录」。
func startPKCE(spec Spec) (LoginSession, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return LoginSession{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// 端口被占/被防火墙挡：如实报，不悄悄换一条「贴 Key」的路——
		// 那会让用户以为这家支持贴 Key，而实际上只能走浏览器。
		return LoginSession{}, errors.New("本机回环监听失败：" + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state := uuidV4()
	redirect := "http://127.0.0.1:" + strconv.Itoa(port) + spec.Login.CallbackPath
	openURL := spec.Login.AuthURL
	if openURL != "" {
		// 授权页参数按各家的真实形态拼：Google 要 response_type=code + PKCE，
		// 其余几家只要 client_id + redirect_uri + state。
		q := url.Values{}
		q.Set("redirect_uri", redirect)
		q.Set("state", state)
		if strings.Contains(openURL, "accounts.google.com") {
			q.Set("client_id", geminiClientID)
			q.Set("response_type", "code")
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "S256")
			q.Set("scope", "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile")
			q.Set("access_type", "offline")
			q.Set("prompt", "consent")
		} else {
			q.Set("response_type", "code")
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "S256")
		}
		// trae 与 accio 的授权页参数集合**各自是一份完整契约**，不能走通用那套：
		//
		//	trae   17 个参数，回调地址的参数名是 auth_callback_url（不是 redirect_uri），
		//	       缺 auth_from/login_channel/auth_type/redirect/login_trace_id 时
		//	       登录页会**永远停在「授权中」**，既不跳转也不回传。
		//	accio  回调地址的参数名是 return_url；缺它恰好命中登录页的 legacy
		//	       客户端门禁，弹「请升级客户端」而不是登录框。
		//
		// ⚠️ 这两条此前都按通用形态送 redirect_uri，症状是「授权页能打开、但走不完」。
		if strings.Contains(openURL, "trae.cn") {
			machineID, deviceID, err := traeLoginIdentity()
			if err != nil {
				ln.Close()
				return LoginSession{}, errors.New("生成 trae 设备标识失败：" + err.Error())
			}
			traceID := machineID + deviceID
			if len(traceID) > 16 {
				traceID = traceID[len(traceID)-16:]
			}
			q := url.Values{}
			q.Set("login_version", "1")
			q.Set("auth_from", "solo")
			q.Set("login_channel", "native_ide")
			q.Set("plugin_version", "2.3.62834")
			q.Set("auth_type", "local")
			q.Set("client_id", "en1oxy7wnw8j9n")
			q.Set("redirect", "0")
			q.Set("login_trace_id", traceID)
			q.Set("auth_callback_url", redirect)
			q.Set("machine_id", machineID)
			q.Set("device_id", deviceID)
			q.Set("x_device_id", deviceID)
			q.Set("x_machine_id", machineID)
			q.Set("x_device_brand", "PC")
			q.Set("x_device_type", "PC")
			q.Set("x_os_version", "1.0")
			q.Set("x_app_version", "0.1.52")
			q.Set("x_app_type", "stable")
			// 覆盖前面套的通用三件套：trae 只认上面这一份集合。
			openURL = spec.Login.AuthURL + "?" + q.Encode()
		} else if strings.Contains(openURL, "accio-ai.com") {
			// accio 的回调参数名是 return_url（**不是** redirect_uri），且必须带
			// ttid 与 language：缺 return_url 会命中它登录页的 legacy 客户端门禁，
			// 用户看到的是「请升级客户端」弹窗而不是登录框。
			q := url.Values{}
			q.Set("return_url", redirect)
			q.Set("state", state)
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "S256")
			q.Set("client_id", "accio-work")
			q.Set("ttid", "ttid_"+shortHex(16))
			q.Set("language", "zh")
			openURL = spec.Login.AuthURL + "?" + q.Encode()
		} else if tpl := spec.Login.AuthQueryTemplate; tpl != "" {
			// 这一家的参数集合本身就是契约（lobsterai 只认 source=electron +
			// redirect_uri + state，多给一个 code_challenge 会被当作网页来源走另一条流程）。
			//
			// ⚠️ 拼在**整个 URL 的末尾**，包括 hash 之后：lobsterai 的登录入口是
			// `…/portal#/login`，参数本来就属于 fragment（页面自己从 location.hash
			// 里读）。把它挪到 `?` 前面会被当成门户首页的参数，登录页拿不到 redirect_uri。
			query := strings.NewReplacer(
				"{{redirect_uri}}", url.QueryEscape(redirect),
				"{{state}}", url.QueryEscape(state),
				"{{client_id}}", url.QueryEscape(spec.Login.ClientID),
				"{{scope}}", url.QueryEscape(spec.Login.Scope),
				"{{code_challenge}}", url.QueryEscape(challenge),
				"{{code_challenge_method}}", "S256",
			).Replace(tpl)
			sep := "?"
			if strings.Contains(openURL, "?") {
				sep = "&"
			}
			openURL += sep + query
		} else {
			sep := "?"
			if strings.Contains(openURL, "?") {
				sep = "&"
			}
			openURL += sep + q.Encode()
		}
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginPKCE,
		State: state, Verifier: verifier, OpenURL: openURL,
		LocalPort: port, IntervalMS: 1000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	// 后台收 code：拿到就写进会话的 codeSlot，轮询那边读走即清。
	go serveCallback(ln, spec, state)
	return remember(s), nil
}

// codeSlot 保存 PKCE 回调带回来的 code（按 channel + state 索引）。
var (
	codeMu  sync.Mutex
	codeTab = map[string]string{}
)

// serveCallback 收一次回调并关闭监听器。
func serveCallback(ln net.Listener, spec Spec, state string) {
	defer ln.Close()
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			codeMu.Lock()
			codeTab[spec.ID+"|"+state] = "!" + e
			codeMu.Unlock()
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "授权被拒绝："+e+"，可以关掉这个页面了。")
			return
		}
		if q.Get("state") != state {
			// state 不匹配是 CSRF 形态：**不**写进槽位，也不提示「成功了」。
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "state 不匹配，这次回调被丢弃。")
			return
		}
		code := q.Get("code")
		if code == "" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "回调里没有 code。")
			return
		}
		codeMu.Lock()
		codeTab[spec.ID+"|"+state] = code
		codeMu.Unlock()
		_, _ = io.WriteString(w, "授权已收到，可以关掉这个页面回到面板了。")
		// Give the response a moment to flush, then release the port.
		time.AfterFunc(200*time.Millisecond, func() { _ = srv.Close() })
	})
	srv.Handler = mux
	// Serve() used to block for the full 10-minute TTL even after the
	// callback arrived: each login leaked a listener and a sleep goroutine.
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(loginTTL):
			_ = srv.Close()
		case <-done:
		}
	}()
	_ = srv.Serve(ln)
	close(done)
}

// PollPKCE 轮一次：回调到了就换 token，没到就 pending。
func PollPKCE(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	key := spec.ID + "|" + sess.State
	codeMu.Lock()
	code, ok := codeTab[key]
	if ok {
		delete(codeTab, key)
	}
	codeMu.Unlock()
	if !ok {
		return "pending", Account{}, nil
	}
	if strings.HasPrefix(code, "!") {
		return "", Account{}, errors.New("授权被拒绝：" + strings.TrimPrefix(code, "!"))
	}
	redirect := "http://127.0.0.1:" + strconv.Itoa(sess.LocalPort) + spec.Login.CallbackPath
	body := map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirect,
		"code_verifier": sess.Verifier,
	}
	if strings.Contains(spec.Login.ExchangeURL, "googleapis") {
		body["client_id"] = geminiClientID
		body["client_secret"] = geminiClientSecret
	}
	headers := map[string]string{"accept": "application/json"}
	code2, raw, err := HTTPPost(ctx, spec.Login.ExchangeURL, headers, body)
	if err != nil {
		return "", Account{}, err
	}
	if code2 < 200 || code2 >= 300 {
		return "", Account{}, errors.New("换 token 失败：" + statusText(code2, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("换 token 响应无法解析：" + short(raw))
	}
	tok := field(payload, "access_token")
	if tok == "" {
		// Google 把 token 放在 data 外面；有些实现对成功的判定是 200 + 业务码。
		body2 := flatten(payload)
		if m, ok := body2.(map[string]any); ok {
			tok = field(m, "access_token")
		}
	}
	if tok == "" {
		return "", Account{}, errors.New("换 token 响应里没有 access_token：" + short(raw))
	}
	acct := Account{
		AccessToken: tok,
		RefreshTok:  field(payload, "refresh_token"),
		Source:      LoginPKCE,
		CreatedAtMS: time.Now().UnixMilli(),
	}
	if v, ok := numeric(payload["expires_in"]); ok && v > 0 {
		acct.ExpiresAtMS = time.Now().Add(time.Duration(v) * time.Second).UnixMilli()
	}
	// 这一家的附加字段：Google 要 project，华为系要 cookie，trae 要 uid。
	switch {
	case strings.Contains(spec.ID, "gemini"):
		acct.Extra = map[string]string{"project": geminiProject}
	case strings.Contains(spec.ID, "officeace"):
		acct.Extra = map[string]string{"cookie": "ssoSessionId=" + tok}
	case strings.Contains(spec.ID, "trae"):
		if sub, _, _, ok := JWTClaims(tok); ok {
			acct.Extra = map[string]string{"uid": sub}
		}
	}
	return "ok", acct, nil
}

// --- 二维码（微信扫码）--------------------------------------------------------

// startQR 取一次微信二维码。
//
// ⚠️ 微信开放平台的扫码登录是**两段**：先拿 uuid，再把 uuid 换成图；
// 轮询走 long.open.weixin.qq.com（长轮询），返回 408 表示「还没扫」。
// 本项目只**展示**二维码与轮询结果，扫码本身在用户手机上完成。
func startQR(ctx context.Context, spec Spec) (LoginSession, error) {
	headers := map[string]string{"accept": "application/json"}
	code, raw, err := HTTPGet(ctx, spec.Login.QRURL, headers)
	if err != nil {
		return LoginSession{}, err
	}
	if code < 200 || code >= 300 {
		return LoginSession{}, errors.New("二维码请求失败：" + statusText(code, raw))
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	data := flatten(payload)
	m, _ := data.(map[string]any)
	uuidVal := field(m, "uuid")
	if uuidVal == "" {
		uuidVal = field(m, "qrcode_uuid")
	}
	if uuidVal == "" {
		// 上游没给 uuid：如实报「这一家要扫码，但取图端点这次没给句柄」，
		// 而不是展示一个永远轮询不到结果的空二维码。
		return LoginSession{}, errors.New("上游没有给出二维码句柄（uuid）")
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginQR,
		State:      uuidVal,
		QRImage:    "https://open.weixin.qq.com/connect/qrcode/" + url.PathEscape(uuidVal),
		IntervalMS: 3000, ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	return remember(s), nil
}

// PollQR 轮一次扫码状态：两种形态各走各的端点（用描述表的字段区分，不特判渠道 id）。
func PollQR(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	if spec.Login.QRContent != "" {
		return pollQRCode(ctx, spec, sess)
	}
	return pollWechatQR(ctx, spec, sess)
}

// pollWechatQR 是微信长轮询那一段。
//
// 返回 `window.wx_errcode=...;window.wx_code="..."` 这种脚本片段，语义以微信授权页
// 内嵌 JS 为准（那是**官方源码**，不是推测）：
//
//	408 待扫码 / 404 已扫待确认 / 405 **已确认**（wx_code 就在这一帧）/
//	403 用户取消 / 402 二维码失效。
//
// ⚠️ 405 与 404 **不能读反**：读反的后果是「扫码确认后永远等不到 code」，用户看到
// 的是「已扫码但没后续」，而凭据池里始终是空的。
func pollWechatQR(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	urlQR := "https://long.open.weixin.qq.com/connect/l/qrconnect?uuid=" + url.PathEscape(sess.State) + "&_=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	code, raw, err := HTTPGet(ctx, urlQR, nil)
	if err != nil {
		return "pending", Account{}, nil // 长轮询被取消是常态，不当失败
	}
	if code == 408 {
		return "pending", Account{}, nil
	}
	if code != 200 {
		return "", Account{}, errors.New("扫码轮询失败：" + statusText(code, raw))
	}
	// 微信的响应是 `window.wx_errcode=…;window.wx_code='…'` 这种脚本片段。
	text := string(raw)
	if !strings.Contains(text, "wx_code") {
		return "pending", Account{}, nil
	}
	if !strings.Contains(text, "wx_errcode=405") {
		return "pending", Account{}, nil
	}
	codeVal := between(text, "wx_code='", "'")
	if codeVal == "" {
		codeVal = between(text, `wx_code="`, `"`)
	}
	if codeVal == "" {
		return "", Account{}, errors.New("扫码回调里没有 code")
	}
	// 拿到微信 code 只是第一段：这一家的会话由讯飞账号端点下发（HMAC-SHA1 签名），
	// 已绑手机号的那一支直接用 skip 换回来；未绑的那一支把原因带出来（差一次短信）。
	acct, err := bindLoomyWechat(ctx, codeVal)
	if err != nil {
		return "", Account{}, err
	}
	Forget(spec.ID)
	return "ok", acct, nil
}

// --- 讯飞账号端点（loomy 的微信 code → 会话）---------------------------------
//
// 拿到微信 code 只完成了一半：这一家的会话由**账号端点**下发，而那一条要
// HMAC-SHA1 签名（Authorization: account {ak}:{sig}）。签名实现在 sign.go，
// 这里只负责信封与三步分支。
//
//	POST /login/thirdAccount/bind/auth      {tcode:{code}, type:"wx"} → {rcode, bind}
//	POST /login/thirdAccount/bind/skip      {rcode}                  → {session, userid}
//	POST /login/thirdAccount/bind/sendMsg   {rcode, phone, ccode, expire} → {msgid}
//	POST /login/thirdAccount/bind/checkCode {rcode, mcode, msgid, expire} → {session, userid}
//
// ⚠️ bind 缺失时按**未绑定**处理（保守方向）：若实际已绑，用户最多多走一次
// 绑定；若实际未绑却当已绑去 skip，会拿到一个没有手机号的会话。
const loomyAccountBase = "https://account.xfinfr.com"

// loomyAccountAK/SK 是**随 Loomy 客户端分发的公开常量**（App 里的 .env.prod），
// 只用于账号端点的签名，与任何用户数据无关 —— 不是「我们的密钥」。
const (
	loomyAccountAK  = "2thryby66wxi53sk"
	loomyAccountSK  = "zsak6eadrbawz683wf5r3m2snrwj868r"
	loomyAppID      = "GM3LOOMY"
	loomySessionTTL = 1209600 // 14 天：向服务端**声明**的有效期，响应不带时间戳
)

// postLoomyAccount 发一次账号端点请求并拆信封。
//
// ⚠️ body 序列化**一次**、签名与发送共用同一个串：二次序列化会改字节
// （键序/空格），签名随即失效，而失败文案是「HMAC signature cannot be verified」。
func postLoomyAccount(ctx context.Context, path string, param map[string]any) (map[string]any, error) {
	trace, err := randomHex(16)
	if err != nil {
		return nil, errors.New("生成 traceid 失败：" + err.Error())
	}
	body := map[string]any{
		"base": map[string]any{
			"appid": loomyAppID, "modelid": "Web", "version": "1.0.0", "devid": "web",
			// ⚠️ 硬编码 macOS：客户端在 Windows 上发的也是这个值。
			"ua": "Loomy|Desktop|Electron|macOS", "traceid": trace,
		},
		"param": param,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	txt := string(encoded)
	spec := QuotaSpec{Method: "POST", Path: path, Body: txt}
	dst := map[string]string{"accept": "application/json"}
	if err := signLoomy(dst, "POST", path, spec, Account{Extra: map[string]string{
		"access_key": loomyAccountAK, "secret_key": loomyAccountSK,
	}}); err != nil {
		return nil, err
	}
	code, raw, err := HTTPPost(ctx, loomyAccountBase+path, dst, txt)
	if err != nil {
		return nil, errors.New("讯飞账号请求失败：" + err.Error())
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("讯飞账号响应不是 JSON（" + strconv.Itoa(code) + "）：" + short(raw))
	}
	// 信封：{code, desc, data}。code 非 0 一律是失败，desc 就是出路。
	if c := field(payload, "code"); c != "0" {
		return nil, errors.New(firstNonEmpty(field(payload, "desc"), "讯飞账号请求失败："+short(raw)))
	}
	if d, ok := payload["data"].(map[string]any); ok {
		return d, nil
	}
	return map[string]any{}, nil
}

// loomySession 从账号端点的 data 里取会话与用户 id，缺一即失败。
func loomySession(d map[string]any) (Account, error) {
	session := field(d, "session")
	userid := firstNonEmpty(field(d, "userid"), field(d, "userId"))
	if session == "" {
		return Account{}, errors.New("讯飞登录响应缺少 session")
	}
	if userid == "" {
		return Account{}, errors.New("讯飞登录响应缺少 userid")
	}
	return Account{
		ID:          userid,
		AccessToken: session,
		Source:      LoginQR,
		CreatedAtMS: time.Now().UnixMilli(),
		ExpiresAtMS: time.Now().Add(time.Duration(loomySessionTTL) * time.Second).UnixMilli(),
	}, nil
}

// bindLoomyWechat 用微信 code 换会话：已绑手机号走 skip，未绑则报出下一步。
//
// ⚠️ 未绑手机号这一支**必须把原因带出来**：静默 pending 会让用户以为扫码没生效，
// 而实际上只差一次短信验证。
func bindLoomyWechat(ctx context.Context, code string) (Account, error) {
	d, err := postLoomyAccount(ctx, "/login/thirdAccount/bind/auth", map[string]any{
		"tcode": map[string]any{"code": code}, "type": "wx",
	})
	if err != nil {
		return Account{}, err
	}
	rcode := field(d, "rcode")
	if rcode == "" {
		return Account{}, errors.New("微信授权响应缺少 rcode")
	}
	if bind, _ := numeric(d["bind"]); bind != 1 {
		return Account{}, errors.New("这个微信还没绑定讯飞手机号（rcode " + short([]byte(rcode)) +
			"）：先在 Loomy 客户端里绑一次手机号，或直接从客户端抄 session 贴进来")
	}
	skip, err := postLoomyAccount(ctx, "/login/thirdAccount/bind/skip", map[string]any{
		"rcode": rcode, "expire": loomySessionTTL,
	})
	if err != nil {
		return Account{}, err
	}
	return loomySession(skip)
}

// pollQRCode 轮一次「本机生成 code」的扫码（raccoon 形态）。
//
// 上游按业务信封回答（code 0 = 成功），data.status 三态：pending / logging /
// success / canceled。
//
// ⚠️ 任何异常都降级为 pending：这是 2 秒一次的循环，偶发网络抖动不该中断登录；
// 而把未知状态当成 success 会拿到空 token 后卡死。
//
// ⚠️ success 却缺 access_token 时**必须当 pending**：上游偶尔先置 success 再补
// token（同一 code 的下一次轮询就齐了），当成功会存下一条空凭据。
func pollQRCode(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	code, raw, err := HTTPPost(ctx, spec.Login.QRURL,
		map[string]string{"accept": "application/json", "content-type": "application/json"},
		map[string]any{"qrcode_code": sess.State})
	if err != nil || code < 200 || code >= 300 {
		return "pending", Account{}, nil
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "pending", Account{}, nil
	}
	if v, ok := numeric(payload["code"]); ok && v != 0 {
		return "pending", Account{}, nil
	}
	d, _ := flatten(payload).(map[string]any)
	switch field(d, "status") {
	case "canceled":
		Forget(spec.ID)
		return "", Account{}, errors.New("扫码登录被取消，重新发起一次")
	case "success":
		tok := firstNonEmpty(field(d, "access_token"), field(d, "accessToken"))
		if tok == "" {
			return "pending", Account{}, nil
		}
		acct := Account{
			AccessToken: tok,
			RefreshTok:  firstNonEmpty(field(d, "refresh_token"), field(d, "refreshToken")),
			Source:      LoginQR,
			CreatedAtMS: time.Now().UnixMilli(),
		}
		// device_id / org 是这一家的额度端点要的头（缺了不报错、只是被当成个人空组织）。
		extra := map[string]string{}
		if v := field(d, "device_id"); v != "" {
			extra["device_id"] = v
		}
		if len(extra) > 0 {
			acct.Extra = extra
		}
		if sub, _, exp, ok := JWTClaims(tok); ok {
			acct.ID = sub
			acct.ExpiresAtMS = exp
		}
		Forget(spec.ID)
		return "ok", acct, nil
	}
	return "pending", Account{}, nil
}

// --- 共用小工具 ---------------------------------------------------------------

// geminiClientID / geminiClientSecret / geminiProject 是 Google 的**公开客户端**。
//
// ⚠️ 它们不是「我们的密钥」：Google 的桌面客户端 client_secret 按设计就不是秘密
// （官方文档明写「installed apps cannot keep secrets」），泄漏成本为零。真正的
// 秘密是每个用户自己的 refresh_token —— 那一份存在 store 里，从不出现在本文件。
const (
	geminiClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	geminiClientSecret = "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"
	geminiProject      = "aicode-consumers"
)

// traeLoginIdentity 铸一对本次登录用的机器/设备标识。
//
// trae 的授权页要求 machine_id 与 device_id 各 15 位数字，且 login_trace_id 是
// 两者拼接的**尾部 16 字符** —— 回调不保证回传前两者，只回 trace，靠它关联回本次登录。
// 每次登录新铸：同一台机器上的第二次登录若沿用旧值，上游会把两笔授权看成同一条。
func traeLoginIdentity() (string, string, error) {
	seed, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	return seededDigits(15, seed, "machine"), seededDigits(15, seed, "device"), nil
}

// shortHex 生成 n 字节随机数的 hex（accio 的 ttid 用）。
func shortHex(n int) string {
	h, err := randomHex(n)
	if err != nil {
		return uuidV4()
	}
	return h
}

// pkcePair 生成 S256 的 code_verifier / code_challenge。
func pkcePair() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier := strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(base64.URLEncoding.EncodeToString(b))
	sum := sha256.Sum256([]byte(verifier))
	challenge := strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(base64.URLEncoding.EncodeToString(sum[:]))
	return verifier, challenge, nil
}

// JWTClaims 读 JWT 的 sub 与 exp（不验签：我们只读上游刚发给我们的令牌，
// 验签要密钥，而这里要的只是「这份令牌属于谁、还剩多久」这两个显示用事实）。
func JWTClaims(token string) (sub string, iss string, expMS int64, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", 0, false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return "", "", 0, false
	}
	sub = field(claims, "sub")
	iss = field(claims, "iss")
	if v, ok2 := numeric(claims["exp"]); ok2 && v > 0 {
		expMS = int64(v) * 1000
	}
	return sub, iss, expMS, true
}

// field 从 map 里取一个字符串字段（缺失返回空串）。
func field(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// between 取 text 里 a 与 b 之间的子串（微信脚本片段的唯一读法）。
func between(text, a, b string) string {
	i := strings.Index(text, a)
	if i < 0 {
		return ""
	}
	rest := text[i+len(a):]
	j := strings.Index(rest, b)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// Poll 轮一次当前会话：按形态分派，返回 pending / ok / 错误。
func Poll(ctx context.Context, id string) (string, Account, error) {
	sess, ok := Session(id)
	if !ok {
		return "", Account{}, errors.New("没有进行中的登录")
	}
	if time.Since(time.UnixMilli(sess.StartedAtMS)) > loginTTL {
		Forget(id)
		return "", Account{}, errors.New("登录已超时，请重新开始")
	}
	spec, ok := SpecOf(id)
	if !ok {
		return "", Account{}, errors.New("渠道 " + id + " 没有登记在本表里")
	}
	switch sess.Kind {
	case LoginDevice:
		return PollDevice(ctx, spec, sess)
	case LoginPoll:
		return PollState(ctx, spec, sess)
	case LoginCliFlow:
		return pollCliFlow(ctx, spec, sess)
	case LoginPKCE:
		return PollPKCE(ctx, spec, sess)
	case LoginQR:
		return PollQR(ctx, spec, sess)
	case LoginTicket:
		return pollTicket(ctx, spec, sess)
	case LoginClaw:
		return pollClaw(ctx, spec, sess)
	case LoginQoder:
		return pollQoder(ctx, spec, sess)
	case LoginPaste:
		return "", Account{}, errors.New("贴 Key 形态没有轮询：直接把 Key 提交给面板")
	}
	return "", Account{}, errors.New("未知的登录形态 " + sess.Kind)
}
