package lane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zenui/internal/zen/chprov"
)

// The account side of the WorkBuddy 账号渠道: login, refresh, the daily
// activity claim, and the credits readout. Everything here rides wbMu, which
// serializes token rotation — upstream swaps the refresh token on every
// refresh, so two overlapping refreshes void the second pair and log the
// account out.

// WBAccountFromChannel 把关卡的「渠道凭据表」折成 lane 自己的账号快照。
//
// # 为什么需要这一层
//
// WorkBuddy 的登录有**两条入口**，它们各写各的格子：
//
//	设置页的「登录」（lane 自己的 state 轮询）→ config.workBuddy
//	渠道面的「登录」（chprov 的 poll 形态）    → config.channelAccounts.workbuddy
//
// 而 lane 只读前者。于是用户在渠道面登录成功、面板显示「已登录、还剩 N 天」，
// 目录却一条 WorkBuddy 模型都不出现 —— 因为 workBuddyReady() 看的是那个空的
// 格子。这个缺陷没有任何报错，只会表现成「登录了但没模型」。
//
// 两份都在时以**渠道表**为准：它是这次登录真正写下的那一份，而 config.workBuddy
// 可能是上一次登录留下的旧对（上游每次刷新都换 refresh token，旧的那份已作废）。
// 两份都没有时返回 fallback 原样，让上层照旧判「未登录」。
func WBAccountFromChannel(accts chprov.Accounts, fallback WBAccount) WBAccount {
	usable := accts.Usable()
	if len(usable) == 0 {
		return fallback
	}
	// Legacy callers request one snapshot; pool callers convert each account.
	acct := usable[len(usable)-1]
	if strings.TrimSpace(acct.AccessToken) == "" {
		return fallback
	}
	out := WBAccount{
		ChannelKey:   acct.Key(),
		AccessToken:  acct.AccessToken,
		RefreshToken: acct.RefreshTok,
		UID:          strings.TrimSpace(acct.ID),
		Nickname:     strings.TrimSpace(acct.Nickname),
		ExpiresAtMS:  acct.ExpiresAtMS,
		Enabled:      true,
	}
	if out.UID == "" {
		out.UID = acct.Field("uid")
	}
	// 到期时刻：JWT 自己那一格永远比字段可靠（字段可能缺、也可能是秒）。
	if sub, _, expMS, ok := wbJWTClaims(out.AccessToken); ok {
		if expMS > 0 {
			out.ExpiresAtMS = expMS
		}
		if out.UID == "" {
			out.UID = sub
		}
	}
	if out.UID == "" {
		// 指纹种子从 uid 派生，空 uid 会让每次请求换一副面孔（WAF 直接拦）。
		out.UID = "anonymous"
	}
	// 活跃日与登录无关，跟着旧快照走：同一账号重登不该让当天的签到记录丢一次。
	if strings.EqualFold(fallback.UID, out.UID) {
		out.LastActivityDate = fallback.LastActivityDate
	}
	return out
}

// SetWorkBuddyAccount installs the account snapshot from config. Catalog
// refresh is the caller's job (main wires it at boot; admin refreshes after
// login/logout/toggles).
func (l *Lane) SetWorkBuddyAccount(acct WBAccount) {
	l.wbMu.Lock()
	l.wbAcct = acct
	l.wbPool = nil
	l.wbActive = ""
	l.wbRefreshedAt = time.Time{}
	l.wbRestrictedUntil = time.Time{}
	l.wbRestrictionStrikes = 0
	l.wbMu.Unlock()
}

// SetWorkBuddyPersist wires the write-back path for rotated token pairs and
// activity dates. Called with wbMu held by the refresher — the callback must
// not call back into the account setters.
func (l *Lane) SetWorkBuddyPersist(fn func(WBAccount)) {
	l.wbMu.Lock()
	l.wbPersist = fn
	pool := append([]*wbPoolSlot(nil), l.wbPool...)
	l.wbMu.Unlock()
	for _, slot := range pool {
		slot.client.SetWorkBuddyPersist(l.wbPoolPersist(slot.key))
	}
}

// SetWorkBuddyLoginPersist handles explicit login separately from refresh:
// login may add an identity, while refresh may only update an existing one.
// The login callback runs outside wbMu and may rebuild the account pool.
func (l *Lane) SetWorkBuddyLoginPersist(fn func(WBAccount)) {
	l.wbMu.Lock()
	l.wbLoginPersist = fn
	l.wbMu.Unlock()
}

// WorkBuddySnapshot returns a copy of the live account state.
func (l *Lane) WorkBuddySnapshot() WBAccount {
	l.wbMu.Lock()
	acct := l.wbAcct
	var active *Lane
	for _, slot := range l.wbPool {
		if slot.key == l.wbActive {
			active = slot.client
			break
		}
	}
	l.wbMu.Unlock()
	if active != nil {
		return active.WorkBuddySnapshot()
	}
	return acct
}

// WorkBuddyLogout drops the account. The store copy is the caller's to clear;
// this only takes the live snapshot down so in-flight code stops using it.
func (l *Lane) WorkBuddyLogout() {
	l.wbMu.Lock()
	l.wbAcct = WBAccount{}
	l.wbPool = nil
	l.wbActive = ""
	l.wbLoginState = ""
	l.wbMu.Unlock()
	l.fireChange()
}

// workBuddyReady returns an enabled, token-bearing copy of the account.
func (l *Lane) workBuddyReady() (WBAccount, bool) {
	acct := l.WorkBuddySnapshot()
	if !acct.Enabled || strings.TrimSpace(acct.AccessToken) == "" {
		return WBAccount{}, false
	}
	return acct, true
}

// RefreshWorkBuddyCatalog updates only the local account-backed roster. A
// channel membership toggle must not wait for unrelated Zen/Kilo HTTP fetches.
func (l *Lane) RefreshWorkBuddyCatalog() {
	_, ready := l.workBuddyReady()
	l.mu.Lock()
	cat := make([]ModelInfo, 0, len(l.catalog)+3)
	var previous []ModelInfo
	for _, m := range l.catalog {
		if m.Channel != ChannelWorkBuddy {
			cat = append(cat, m)
		} else {
			previous = append(previous, m)
		}
	}
	if ready {
		if len(previous) == 0 {
			previous = buildWorkBuddyCatalog()
		}
		cat = append(cat, previous...)
	}
	l.catalog = cat
	l.mu.Unlock()
	if l.OnCatalogReload != nil {
		l.OnCatalogReload()
	}
	l.notify()
}

// wbPersist writes the account back under the lock that guarantees one writer.
func (l *Lane) wbPersistLocked() {
	if l.wbPersist == nil {
		return
	}
	l.wbPersist(l.wbAcct)
}

// wbPost is the one post entry every WorkBuddy turn goes through: the probe,
// the daily activity claim, and user traffic. It refreshes a token that is
// close to expiry before spending it, and on a credential refusal refreshes
// once and retries — a rotated-but-unsaved pair would otherwise fail the turn
// even though a valid refresh token is sitting right there.
func (l *Lane) wbPostSingle(ctx context.Context, body map[string]any, conversationID string, onData func(payload []byte) error) (*Usage, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return nil, &UpstreamError{Code: CodeCredential, Message: "WorkBuddy 账号未登录或已停用"}
	}
	if left := l.wbRestrictionCooldown(); left > 0 {
		// The account was restricted upstream; nothing this process sends can
		// lift it, and each attempt is one more strike. Answer from the local
		// ledger instead of burning a round trip on a certainty.
		return nil, &UpstreamError{Code: wbCodeRestricted,
			Message:    "WorkBuddy 账号被上游限制中（" + strconv.Itoa(int(left.Seconds())) + " 秒后重试），换号或等上游解除",
			RetryAfter: int(left.Seconds())}
	}
	if wbTokenNearExpiry(acct) {
		if rerr := l.refreshWorkBuddy(ctx); rerr == nil {
			if fresh, ok := l.workBuddyReady(); ok {
				acct = fresh
			}
		}
		// A failed proactive refresh is not fatal: the old token may still
		// answer, and the 401 path below gets one more chance.
	}
	usage, err := PostWorkBuddyStreamed(ctx, body, acct, conversationID, onData)
	ue := asUpstream(err)
	if ue == nil {
		return usage, err
	}
	switch ue.Code {
	case CodeCredential:
		if rerr := l.refreshWorkBuddy(ctx); rerr == nil {
			if fresh, ok := l.workBuddyReady(); ok {
				return PostWorkBuddyStreamed(ctx, body, fresh, conversationID, onData)
			}
		}
	case wbCodeRestricted:
		// Park the account instead of replaying it: the restriction is a
		// verdict about this credential's traffic, and repeat offences get a
		// longer ban than first ones. Every model on it is equally suspect,
		// so the park is account-wide rather than per model.
		l.wbNoteRestriction()
	}
	return usage, err
}

// wbNoteRestriction records one 11140 and reports the quarantine deadline.
//
// The ladder is deliberately long compared to the 6004 model windows: a
// restriction is lifted by the upstream only, and a healthy-looking billing
// readout is no evidence that the chat plane has forgiven the account.
func (l *Lane) wbNoteRestriction() time.Duration {
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	l.wbRestrictionStrikes++
	wait := wbRestrictionWait(l.wbRestrictionStrikes)
	l.wbRestrictedUntil = time.Now().Add(wait)
	return wait
}

// wbRestrictionCooldown reports the remaining account quarantine, if any
// (lazy: a lapsed deadline reports zero).
func (l *Lane) wbRestrictionCooldown() time.Duration {
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	if l.wbRestrictedUntil.IsZero() {
		return 0
	}
	left := time.Until(l.wbRestrictedUntil)
	if left <= 0 {
		l.wbRestrictedUntil = time.Time{}
		l.wbRestrictionStrikes = 0
		return 0
	}
	return left
}

// refreshWorkBuddy exchanges the refresh token for a fresh pair. Only the
// short bookkeeping sections hold wbMu: the network round trip runs under a
// dedicated refreshMu, so a hung upstream no longer freezes snapshots, quota
// reads and user traffic behind 15 seconds of global lock. A failed exchange
// gets a cooldown, otherwise N concurrent 401s each retry a dead endpoint.
func (l *Lane) refreshWorkBuddy(ctx context.Context) error {
	l.wbRefreshMu.Lock()
	defer l.wbRefreshMu.Unlock()

	l.wbMu.Lock()
	acct := l.wbAcct
	refreshedAt := l.wbRefreshedAt
	failedAt := l.wbRefreshFailedAt
	l.wbMu.Unlock()
	if acct.RefreshToken == "" {
		return errors.New("WorkBuddy 没有 refresh token")
	}
	if !wbTokenNearExpiry(acct) && time.Since(refreshedAt) < time.Minute {
		return nil // someone just refreshed; the pair is current
	}
	if !failedAt.IsZero() && time.Since(failedAt) < 15*time.Second && !wbTokenNearExpiry(acct) {
		return errors.New("WorkBuddy 刷新冷却中")
	}
	fail := func(err error) error {
		l.wbMu.Lock()
		l.wbRefreshFailedAt = time.Now()
		l.wbMu.Unlock()
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WorkBuddyBase+wbAuthRefreshPath, strings.NewReader("{}"))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", wbDesktopUAFor(acct.Realm))
	req.Header.Set("x-codebuddy-request", "1")
	req.Header.Set("x-refresh-token", acct.RefreshToken)
	req.Header.Set("x-auth-refresh-source", "plugin")
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Errorf("WorkBuddy 刷新返回 HTTP %d: %s", resp.StatusCode, truncate(string(data), 200)))
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fail(err)
	}
	if payload.Code != 0 || payload.Data.AccessToken == "" {
		return fail(fmt.Errorf("WorkBuddy 刷新被拒（code %d）", payload.Code))
	}
	acct.AccessToken = payload.Data.AccessToken
	if payload.Data.RefreshToken != "" {
		// The pair rotates: keep the new refresh token even if the answer
		// omits it (the old one may already be consumed).
		acct.RefreshToken = payload.Data.RefreshToken
	}
	if _, iss, expMS, ok := wbJWTClaims(acct.AccessToken); ok {
		if expMS > 0 {
			acct.ExpiresAtMS = expMS
		}
		_ = iss
	}
	l.wbMu.Lock()
	l.wbAcct = acct
	l.wbRefreshedAt = time.Now()
	l.wbRefreshFailedAt = time.Time{}
	l.wbPersistLocked()
	l.wbMu.Unlock()
	return nil
}

// WorkBuddyLoginStart opens a login: the upstream mints a state and an
// authUrl, and the user finishes the login in their browser. There is no
// callback — completion is observed by polling with the state.
func (l *Lane) WorkBuddyLoginStart() (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		WorkBuddyBase+wbAuthStatePath+"?platform=CLI", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", wbDesktopUAFor(""))
	req.Header.Set("x-codebuddy-request", "1")
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("登录状态请求返回 HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 || payload.Data.State == "" || payload.Data.AuthURL == "" {
		return "", fmt.Errorf("登录状态请求被拒（code %d）", payload.Code)
	}
	l.wbMu.Lock()
	l.wbLoginState = payload.Data.State
	l.wbLoginAt = time.Now()
	l.wbMu.Unlock()
	return payload.Data.AuthURL, nil
}

// WorkBuddyLoginPoll checks one round of the login: "pending" while the
// browser side is unfinished, "ok" once the token pair landed (already
// persisted and folded into the lane). Poll errors name what went wrong.
func (l *Lane) WorkBuddyLoginPoll() (string, error) {
	l.wbMu.Lock()
	state := l.wbLoginState
	startedAt := l.wbLoginAt
	l.wbMu.Unlock()
	if state == "" {
		return "", errors.New("没有进行中的登录")
	}
	if time.Since(startedAt) > wbLoginTTL {
		l.wbMu.Lock()
		l.wbLoginState = ""
		l.wbMu.Unlock()
		return "", errors.New("登录已超时，请重新开始")
	}
	pollCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	httpStatus, body, err := wbGet(pollCtx, wbAuthTokenPath+"?state="+state, WBAccount{})
	if err != nil {
		return "", err
	}
	if httpStatus != 200 {
		return "", fmt.Errorf("登录轮询返回 HTTP %d", httpStatus)
	}
	var payload struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data wbTokenResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.Code == wbLoginPending {
		return "pending", nil
	}
	if payload.Code != 0 || payload.Data.AccessToken == "" {
		return "", fmt.Errorf("登录未完成（code %d）%s", payload.Code, strings.TrimSpace(payload.Msg))
	}
	acct, err := wbAccountFromToken(payload.Data)
	if err != nil {
		return "", err
	}
	l.wbMu.Lock()
	if l.wbLoginState != state {
		l.wbMu.Unlock()
		return "", errors.New("登录已取消，请重新开始")
	}
	l.wbAcct = acct
	l.wbLoginState = ""
	persist := l.wbLoginPersist
	if persist == nil {
		persist = l.wbPersist
		l.wbPool = nil
		l.wbActive = ""
	}
	l.wbMu.Unlock()
	if persist != nil {
		persist(acct)
	}
	go l.RefreshCatalog(context.Background())
	l.fireChange()
	return "ok", nil
}

type wbTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    any    `json:"expiresAt"`
}

// wbAccountFromToken validates the minted pair: the JWT must name the intl
// realm (this build only speaks workbuddy.ai) and carries uid/expiry itself.
func wbAccountFromToken(t wbTokenResponse) (WBAccount, error) {
	sub, iss, expMS, ok := wbJWTClaims(t.AccessToken)
	if !ok {
		return WBAccount{}, errors.New("返回的令牌无法解析")
	}
	if !strings.Contains(iss, "workbuddy.ai") {
		return WBAccount{}, errors.New("令牌不属于国际版（workbuddy.ai），本渠道暂只支持国际版")
	}
	if sub == "" {
		sub = "anonymous"
	}
	acct := WBAccount{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		UID:          sub,
		ExpiresAtMS:  expMS,
		Enabled:      true,
	}
	if acct.ExpiresAtMS == 0 {
		acct.ExpiresAtMS = wbExpToMS(t.ExpiresAt)
	}
	// Best-effort nickname; its absence costs a display name only.
	if _, body, err := wbGet(context.Background(), wbLoginAccountPath+"?state=", acct); err == nil {
		var named struct {
			Data struct {
				Nickname string `json:"nickname"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &named) == nil {
			acct.Nickname = strings.TrimSpace(named.Data.Nickname)
		}
	}
	return acct, nil
}

// wbGet performs one identity-bearing GET against the WorkBuddy root and
// returns (upstream code, body) without treating non-2xx as a Go error — the
// callers read the envelope's own codes.
func wbGet(ctx context.Context, path string, acct WBAccount) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, WorkBuddyBase+path, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range workBuddyHeaders(acct, wbRequestID()) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// WorkBuddyDailyActivity is a legacy minimal free conversation helper. A
// successful call or spent window is not proof that a daily reward was granted.
// Core no longer schedules this helper as automatic check-in. The free-only
// guard and legacy callers remain compatible.
func (l *Lane) WorkBuddyDailyActivity(ctx context.Context) (bool, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return false, nil
	}
	today := time.Now().Format("2006-01-02")
	if acct.LastActivityDate == today {
		return false, nil
	}
	// The old hardcoded free roster is no longer an authorization to spend.
	// Only an undiscounted, remotely confirmed zero permits unattended activity.
	cat, _, _ := l.Snapshot()
	free := false
	for _, m := range cat {
		if m.Channel == ChannelWorkBuddy && m.ID == "deepseek-v4.1-flash" {
			free = m.Pricing.ConfirmedFree()
			break
		}
	}
	if !free {
		return false, nil
	}
	body := map[string]any{
		"model":            "deepseek-v4.1-flash",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
		"messages":         []map[string]any{{"role": RoleUser, "content": "Hi"}},
		"max_tokens":       10,
		"reasoning_effort": "none",
	}
	// A fresh conversation id: the claim has no next turn, so a stable one
	// would only let one session's cache entry pin the reward path.
	_, err := l.wbPost(ctx, body, "", func([]byte) error { return nil })
	if ue := asUpstream(err); err != nil && (ue == nil || ue.Code != CodeQuota) {
		// Transient trouble (transport, 5xx, refresh failure): leave the day
		// open so the next hourly tick retries.
		return true, err
	}
	l.wbMarkActivity(today)
	return true, err
}

func (l *Lane) wbMarkActivity(date string) {
	l.wbMu.Lock()
	l.wbAcct.LastActivityDate = date
	acct := l.wbAcct
	fn := l.wbPersist
	l.wbMu.Unlock()
	if fn != nil {
		fn(acct)
	}
}

// WorkBuddyCredits reads the billing resource summary for the status card.
// The response's numeric layout is not contractual, so the reader accepts any
// numeric field named like a balance anywhere under data and reports the
// first one it finds; an empty string means "nothing recognizable".
func (l *Lane) WorkBuddyCredits(ctx context.Context) (string, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return "", errors.New("WorkBuddy 账号未登录")
	}
	// The old goroutine + select only abandoned the caller: wbGet used
	// context.Background(), so cancellation left a 15s socket and a goroutine
	// writing into escaped stack variables. wbGet now honours ctx directly.
	outerCode, body, getErr := wbGet(ctx, wbUserResourcePath, acct)
	if getErr != nil {
		return "", getErr
	}
	if outerCode != 200 {
		return "", fmt.Errorf("积分查询返回 HTTP %d", outerCode)
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "", errors.New("积分响应无法解析")
	}
	data, _ := payload["data"].(map[string]any)
	if v := wbFindBalance(data, 3); v != "" {
		return v, nil
	}
	return "", nil
}

func wbFindBalance(m map[string]any, depth int) string {
	if m == nil || depth <= 0 {
		return ""
	}
	for _, key := range []string{"credit", "credits", "balance", "remainCredit", "totalCredit", "point", "points"} {
		if f, ok := toNum(m[key]); ok && f >= 0 {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	for _, v := range m {
		if child, ok := v.(map[string]any); ok {
			if s := wbFindBalance(child, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// probeWorkBuddy re-tests one WorkBuddy model with a minimal chat: the free
// window's cheapest possible proof of life. The lane probe machinery calls
// this through probeModelFor — ProbeModel itself is hardwired to the Zen
// endpoint.
func (l *Lane) probeWorkBuddy(ctx context.Context, m ModelInfo) ProbeResult {
	res := ProbeResult{Model: m.ID, State: StateUnknown, At: time.Now().UnixMilli()}
	if _, ok := l.workBuddyReady(); !ok {
		res.State = StateUnavailable
		res.Detail = "WorkBuddy 未登录或已停用"
		return res
	}
	body := map[string]any{
		"model":          m.ID,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]any{{"role": RoleUser, "content": "ping"}},
		"max_tokens":     10,
	}
	start := time.Now()
	firstAt := int64(0)
	_, err := l.wbPost(ctx, body, "", func([]byte) error {
		if firstAt == 0 {
			firstAt = atLeastMS(start)
			res.TTFTMs = firstAt
		}
		return nil
	})
	res.LatencyMs = atLeastMS(start)
	if err != nil {
		ue := asUpstream(err)
		switch ue.Code {
		case CodeQuota:
			res.State = StateThrottled
			res.Detail = "免费窗口已用满" + wbThrottleDetail(ue)
		case CodeCredential:
			res.State = StateUnavailable
			res.Detail = "登录态失效（刷新失败）"
		case CodeServer:
			if ue.Unavailable {
				res.State = StateUnavailable
			}
		}
		return res
	}
	res.State = StateAvailable
	return res
}

// wbThrottleDetail appends the reset wall clock to a throttle verdict when the
// failure carried one.
func wbThrottleDetail(ue *UpstreamError) string {
	if ue == nil || ue.RetryAfter <= 0 {
		return ""
	}
	until := time.Now().Add(time.Duration(ue.RetryAfter) * time.Second)
	return "（" + until.Format("15:04") + " 重置）"
}
