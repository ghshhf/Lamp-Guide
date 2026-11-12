package chprov

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// 本文件是后补齐的四条形态：华为票据（codearts）、华为 AgentArts 云 state（officeace）、
// 二维码取图（loomy / raccoon），以及 officeace 额度端点要的 V11 签名。
//
// # 为什么这几条此前全在「点了登录没反应」这一档
//
// 它们各自被登记成了「看起来最像」的既有形态，而实际契约每家都不同：
//
//	codearts     登记成 LoginPoll（POST + {}）→ 那条端点只挂 GET、还要 secret → 404
//	officeace    登记成 LoginPKCE（本机回环）→ 它要先在云端登记 state、sts 要 DPoP → 400
//	loomy/raccoon 登记成 LoginQR（读 JSON 的 uuid 字段）→ 二维码根本不在 JSON 里
//
// 症状恒定一句「发起登录失败」，因为面板只能透传上游那一行。形态登记错的代价比
// 「这家没接」更大：用户会以为上游坏了，而凭据其实一直有效。

// --- 票据形态（华为 doer：codearts）--------------------------------------------

// startTicket 起一个本机回环，把华为登录页给用户。
//
// 上游契约（逐字段实测）：
//
//	浏览器 → https://auth.huaweicloud.com/authui/login.html?service=<doer URL>
//	doer    → 渲染/跳到 http://127.0.0.1:{port}/authentication?secret=<一次性密钥>
//	本机    → GET <ticket 端点>?ticket_id=<uuid>&secret=<那个密钥>
//	         → {credential:{access, secret, securitytoken, expires_at}} 或 {result:{...}}
//
// ⚠️ 票据端点只挂 GET：POST 得 404 method POST not found（GET/POST/PUT/OPTIONS/
// HEAD/PATCH/DELETE 七种方法逐一试过，只有 GET 有路由）。把它当「发起」用
// （POST + {}）正是本项目此前那条 HTTP 404 的来源。
//
// ⚠️ 没拿到 secret 时不要轮询：没 secret 打同一个端点得「密钥不能为空」——
// 那不是 pending，是「流程还没走完」；混成同一个状态会让面板一直转圈。
func startTicket(ctx context.Context, spec Spec) (LoginSession, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return LoginSession{}, errors.New("本机回环监听失败：" + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ticketID := uuidV4()
	secret, err := randomHex(32)
	if err != nil {
		ln.Close()
		return LoginSession{}, errors.New("生成票据密钥失败：" + err.Error())
	}
	callback := "http://127.0.0.1:" + strconv.Itoa(port) + spec.Login.CallbackPath
	doer := spec.Login.TicketBase +
		"?IdeaType=jetbrains" +
		"&auth_callback_url=" + url.QueryEscape(callback) +
		"&plugin-name=" + url.QueryEscape(spec.Login.PluginName) +
		"&plugin-version=" + url.QueryEscape(spec.Login.PluginVersion) +
		"&ticket_id=" + url.QueryEscape(ticketID)
	open := spec.Login.AuthURL + "?service=" + url.QueryEscape(doer)
	s := LoginSession{
		Channel: spec.ID, Kind: LoginTicket,
		State: ticketID, OpenURL: open,
		PollToken:   secret,
		LocalPort:   port,
		IntervalMS:  2000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	go serveTicketCallback(ln, spec, ticketID)
	return remember(s), nil
}

// serveTicketCallback 收一次 secret 回调。
//
// ⚠️ 与 PKCE 的回调同形但落点不同：这里收到的不是 code，是一次性密钥 ——
// 换凭据要拿它去轮询票据端点。故它进 codeTab（按 channel|ticket_id 索引），
// 与 PKCE 共用那张表但键不同（键里有 ticket_id，不会串到另一次登录）。
func serveTicketCallback(ln net.Listener, spec Spec, ticketID string) {
	defer ln.Close()
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sec := r.URL.Query().Get("secret")
		if sec == "" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "回调里没有 secret：登录没走完，或这一页是被直接打开的。")
			return
		}
		codeMu.Lock()
		codeTab[spec.ID+"|"+ticketID] = sec
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

// pollTicket 轮一次票据：有 secret 就去换凭据，没有就 pending。
//
// ⚠️ 「没有 secret」是**常态**（用户还在浏览器里输入密码），不能当失败；
// 而「上游回了 400 无效ticketId」是**终态**，继续轮一万次只会让面板一直转圈。
func pollTicket(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	key := spec.ID + "|" + sess.State
	codeMu.Lock()
	secret, arrived := codeTab[key]
	codeMu.Unlock()
	if !arrived || secret == "" {
		return "pending", Account{}, nil
	}
	q := "?ticket_id=" + url.QueryEscape(sess.State) + "&secret=" + url.QueryEscape(secret)
	head := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json;charset=UTF-8",
	}
	if spec.Login.PluginName != "" {
		head["plugin-name"] = spec.Login.PluginName
	}
	if spec.Login.PluginVersion != "" {
		head["plugin-version"] = spec.Login.PluginVersion
	}
	code, raw, err := HTTPGet(ctx, spec.Login.PollURL+q, head)
	if err != nil {
		return "pending", Account{}, nil
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		if code >= 200 && code < 300 {
			return "pending", Account{}, nil
		}
		return "", Account{}, errors.New("票据轮询响应无法解析：" + statusText(code, raw))
	}
	if acct, done := ticketCredential(payload); done {
		codeMu.Lock()
		delete(codeTab, key)
		codeMu.Unlock()
		Forget(spec.ID)
		acct.Source = LoginTicket
		acct.CreatedAtMS = time.Now().UnixMilli()
		return "ok", acct, nil
	}
	// 拿不出凭据：上游那句话必须带出来 ——「无效ticketId」（要重开登录）与
	// 「密钥不能为空」（流程没走完）是两件完全不同的事。
	//
	// ⚠️ 终态必须在这里断掉：上游对「ticket 已作废/没登记」回的是 **HTTP 400** +
	// body 里的 error_code（TM.00001001 无效ticketId）。只认 401/403 的话，这条
	// 会一路 pending 到 10 分钟超时 —— 用户看到的是永远转圈，而不是「重开一次」。
	msg := statusText(code, raw)
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		Forget(spec.ID)
		return "", Account{}, errors.New("票据被上游拒了：" + msg)
	}
	if isTicketTerminal(payload, msg) {
		Forget(spec.ID)
		return "", Account{}, errors.New("票据已失效，请重开登录：" + msg)
	}
	return "pending", Account{}, nil
}

// isTicketTerminal 判这次票据轮询是不是**终态失败**（不该再轮下去）。
//
// 上游的三种回话（逐字实测）：
//
//	404 APIG.0101 The API does not exist: method POST not found  → 方法错，终态
//	400 TM.00001001 无效ticketId: <id>                            → 票据作废，终态
//	400 TM.00001001 密钥不能为空                                  → 流程没走完，**pending**
//
// ⚠️ 第三种与第二种共用同一个 error_code，"密钥不能为空" 才是 pending。
// 按 code 判会把「用户还在输密码」判成失败，按文案判又会把每次 pending 判成失败；
// 必须两句一起看。
func isTicketTerminal(payload map[string]any, msg string) bool {
	if strings.Contains(msg, "密钥不能为空") {
		return false
	}
	if strings.Contains(msg, "无效ticketId") || strings.Contains(msg, "invalid ticket") {
		return true
	}
	if strings.Contains(msg, "method POST not found") || strings.Contains(msg, "APIG.0101") {
		return true
	}
	if ec := field(payload, "error_code"); ec == "TM.00001001" {
		// 同一个业务码下的两种语义：只有「密钥不能为空」是 pending。
		return !strings.Contains(msg, "密钥不能为空")
	}
	return false
}

// ticketCredential 从票据响应里折出 AK/SK 三件套。
//
// 两种信封都见过（credential / result），字段大小写也变过 —— 逐种都试；
// 缺任一必备字段就不算成功（半份凭据发不出任何请求，存下来只会让面板显示
// 「已登录但读不到额度」）。
func ticketCredential(payload map[string]any) (Account, bool) {
	data := flatten(payload)
	m, _ := data.(map[string]any)
	if m == nil {
		m = payload
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := field(m, k); v != "" {
				return v
			}
		}
		return ""
	}
	ak := get("access_key_id", "accessKeyId", "access")
	sk := get("secret_access_key", "secretAccessKey", "secret")
	st := get("security_token", "securityToken", "securitytoken")
	if ak == "" || sk == "" {
		return Account{}, false
	}
	extra := map[string]string{"access_key_id": ak, "secret_access_key": sk}
	if st != "" {
		extra["security_token"] = st
	}
	if id := get("user_id", "userId", "user_name", "userName"); id != "" {
		extra["uid"] = id
	}
	acct := Account{Extra: extra}
	if exp := get("expires_at", "expiresAt", "expiration"); exp != "" {
		if ms, ok := ParseExpiryValue(exp); ok {
			acct.ExpiresAtMS = ms
		}
	}
	if name := get("user_name", "userName"); name != "" {
		acct.Nickname = name
	}
	return acct, true
}

// --- 云端 state 形态（华为 AgentArts：officeace）-------------------------------

// startClaw 先在云端登记 state，再把授权页给用户。
//
// ⚠️ state 必须用上游发的：自造的 state 回调后查不到 code（上游只认它自己
// 登记过的那一个）。这一点与 PKCE 恰好相反（那边 state 是本机铸、用于比对），
// 故不能复用 startPKCE —— 复用会让「发起成功、永远查不到 code」。
//
// ⚠️ 授权页要把真正的 authorize 端点**嵌在 service 参数里**（先走华为登录页，
// 登录完才回到 authorize）。直接给 authorize 会因为没有会话而 400。
func startClaw(ctx context.Context, spec Spec) (LoginSession, error) {
	code, raw, err := HTTPPost(ctx, spec.Login.StateURL,
		map[string]string{"content-type": "application/json", "accept": "application/json"}, "{}")
	if err != nil {
		return LoginSession{}, err
	}
	if code < 200 || code >= 300 {
		return LoginSession{}, errors.New("授权状态登记失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return LoginSession{}, errors.New("授权状态响应无法解析：" + short(raw))
	}
	d, _ := flatten(payload).(map[string]any)
	state := firstNonEmpty(field(d, "state"), field(payload, "state"))
	if state == "" {
		return LoginSession{}, errors.New("上游没有给出 state")
	}
	verifier, challenge, err := pkcePair()
	if err != nil {
		return LoginSession{}, errors.New("生成 PKCE 对失败：" + err.Error())
	}
	// ⚠️ code_challenge_method 恒为 SHA-256（不是 RFC 的 S256）：IAM 只认这一个，
	// 写 S256 直接 400 IAM.0007 codeChallengeMethod must be SHA-256（已实测）；
	// 而漏写 code_challenge 得 400 IAM.0018 code_challenge ... not present。
	//
	// ⚠️ 授权端点用 spec.Login.AuthorizeURL，**不能**拿登录页地址去拼：
	// /authui/login.html + /v1/oauth2/authorize 得到 /authui/login.html/v1/oauth2/…
	// 那条地址在浏览器里打不开，用户看到的是华为的 404 页，而面板上只是空白授权页。
	authBase := spec.Login.AuthorizeURL
	if authBase == "" {
		authBase = strings.TrimRight(spec.Login.AuthURL, "/") + "/v1/oauth2/authorize"
	}
	inner := authBase + "?response_type=code" +
		"&client_id=" + url.QueryEscape(spec.Login.ClientID) +
		"&scope=" + url.QueryEscape(spec.Login.Scope) +
		"&code_challenge=" + url.QueryEscape(challenge) +
		"&code_challenge_method=SHA-256" +
		"&state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(spec.Login.RedirectURI)
	// 外层交给华为登录页（authui/login.html），service 是**真正的授权端点**。
	// 这一层与参考实现（login.js / officeace-auth.js）逐字一致。
	open := spec.Login.AuthURL + "?service=" + url.QueryEscape(inner)
	s := LoginSession{
		Channel: spec.ID, Kind: LoginClaw,
		State: state, Verifier: verifier, OpenURL: open,
		IntervalMS:  2000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	return remember(s), nil
}

// pollClaw 轮一次 code，拿到就换 STS 凭据。
//
// ⚠️ 上游对「还没授权」回 202（不是 404、也不是 error 字段）。202 是 pending，
// 混成失败会让用户刚打开浏览器就看到「登录失败」。
func pollClaw(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	sep := "?"
	if strings.Contains(spec.Login.CodeURL, "?") {
		sep = "&"
	}
	code, raw, err := HTTPGet(ctx, spec.Login.CodeURL+sep+"state="+url.QueryEscape(sess.State),
		map[string]string{"accept": "application/json"})
	if err != nil {
		return "pending", Account{}, nil
	}
	if code == http.StatusAccepted || code == http.StatusNotFound {
		return "pending", Account{}, nil
	}
	if code < 200 || code >= 300 {
		return "", Account{}, errors.New("授权码轮询失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("授权码响应无法解析：" + short(raw))
	}
	d, _ := flatten(payload).(map[string]any)
	codeVal := firstNonEmpty(field(d, "code"), field(payload, "code"))
	// ⚠️ 这一家的 code 字段在报错信封里是业务码（数字），在成功信封里是授权码
	// （字符串）。只按「有没有 code」判会把它永远当成功，然后拿一个数字去换 token。
	if codeVal == "" || isNumericString(codeVal) {
		return "pending", Account{}, nil
	}
	token, err := exchangeClawToken(ctx, spec, sess, codeVal)
	if err != nil {
		return "", Account{}, err
	}
	Forget(spec.ID)
	token.Source = LoginClaw
	token.CreatedAtMS = time.Now().UnixMilli()
	return "ok", token, nil
}

// exchangeClawToken 用 code + verifier + DPoP 换 STS 凭据。
//
// ⚠️ sts 的 token 端点要 DPoP：缺那个头直接 400
// `Invalid header parameter: DPoP, required`，与「凭据无效」同形。
func exchangeClawToken(ctx context.Context, spec Spec, sess LoginSession, code string) (Account, error) {
	key, err := newDPoPKey()
	if err != nil {
		return Account{}, errors.New("生成 DPoP 密钥失败：" + err.Error())
	}
	proof, err := key.sign("POST", spec.Login.ExchangeURL)
	if err != nil {
		return Account{}, errors.New("DPoP 签名失败：" + err.Error())
	}
	form := url.Values{}
	form.Set("client_id", spec.Login.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", sess.Verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", spec.Login.RedirectURI)
	head := map[string]string{
		"accept":       "application/json",
		"content-type": "application/x-www-form-urlencoded",
		"DPoP":         proof,
	}
	status, raw, err := HTTPPost(ctx, spec.Login.ExchangeURL, head, form.Encode())
	if err != nil {
		return Account{}, err
	}
	if status < 200 || status >= 300 {
		return Account{}, errors.New("换 STS 凭据失败：" + statusText(status, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return Account{}, errors.New("STS 响应无法解析：" + short(raw))
	}
	cred, _ := payload["credentials"].(map[string]any)
	if cred == nil {
		return Account{}, errors.New("STS 响应里没有 credentials：" + short(raw))
	}
	akey := firstNonEmpty(field(cred, "access_key_id"), field(cred, "access"))
	skey := firstNonEmpty(field(cred, "secret_access_key"), field(cred, "secret"))
	stok := firstNonEmpty(field(cred, "security_token"), field(cred, "securitytoken"))
	if akey == "" || skey == "" || stok == "" {
		return Account{}, errors.New("STS 凭据三件套不全（AK/SK/securityToken）：" + short(raw))
	}
	extra := map[string]string{
		"access_key_id":     akey,
		"secret_access_key": skey,
		"security_token":    stok,
	}
	acct := Account{Extra: extra}
	// refresh_token 是**明文可读的 JWT**，exp 恒为「登录时刻 + 30 天」：
	// 续期不会延长它。故这份凭据的真实死线是 refresh 的 exp，不是 access 的。
	if rt := field(payload, "refresh_token"); rt != "" {
		acct.RefreshTok = rt
		if _, _, exp, ok := JWTClaims(rt); ok && exp > 0 {
			s := time.UnixMilli(exp)
			extra["refresh_expires"] = s.Format(time.RFC3339)
		}
	}
	if exp := field(cred, "expiration"); exp != "" {
		if ms, ok := ParseExpiryValue(exp); ok {
			acct.ExpiresAtMS = ms
		}
	}
	return acct, nil
}

// isNumericString 报告一串是不是纯数字（用来区分「业务码」与「授权码」）。
func isNumericString(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// --- 二维码 -------------------------------------------------------------------

// startQRPage 从授权页 HTML 里提 uuid（微信形态：loomy）。
//
// ⚠️ 页面里 uuid 出现两次（img src=/connect/qrcode/<uuid> 与 fordevtool 的
// l/qrconnect?uuid=<uuid>），两处都试 —— 只认一处会在页面微调时静默失效
// （症状是「上游没有给出二维码句柄」，而二维码其实就在同一页里）。
func startQRPage(ctx context.Context, spec Spec) (LoginSession, error) {
	state := uuidV4()
	open := spec.Login.QRPageURL +
		"?appid=" + url.QueryEscape(spec.Login.QRAppID) +
		"&redirect_uri=" + url.QueryEscape(spec.Login.QRRedirectURI) +
		"&response_type=code&scope=snsapi_login" +
		"&state=" + url.QueryEscape(state) + "#wechat_redirect"
	head := map[string]string{
		// 微信对空 UA / 爬虫 UA 会拒；带浏览器 UA 才返回内嵌 uuid 的那一页。
		"user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"referer": "https://open.weixin.qq.com/",
	}
	status, raw, err := HTTPGet(ctx, open, head)
	if err != nil {
		return LoginSession{}, err
	}
	if status < 200 || status >= 300 {
		return LoginSession{}, errors.New("二维码授权页请求失败：" + statusText(status, raw))
	}
	uuidVal := extractWechatUUID(string(raw))
	if uuidVal == "" {
		return LoginSession{}, errors.New("授权页里没找到二维码句柄（uuid）")
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginQR,
		State:       uuidVal,
		OpenURL:     open,
		QRImage:     spec.Login.QRImageBase + url.PathEscape(uuidVal),
		IntervalMS:  3000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	return remember(s), nil
}

// extractWechatUUID 从微信授权页 HTML 里提 uuid。
//
// 字符集按实测放宽（001ZDsw64Vu7ll2E 与 4Y0N_jyVQg== 两种形态都有）；
// 下限定 6 而不是更长：微信未承诺长度，收紧只会在格式微调时静默失效。
func extractWechatUUID(html string) string {
	if html == "" {
		return ""
	}
	for _, marker := range []string{"/connect/qrcode/", "uuid="} {
		if i := strings.Index(html, marker); i >= 0 {
			if v := takeUUID(html[i+len(marker):]); v != "" {
				return v
			}
		}
	}
	return ""
}

// takeUUID 从一串开头处切出合法 uuid（遇到第一个非 uuid 字符就停）。
func takeUUID(s string) string {
	end := 0
	for end < len(s) {
		c := s[end]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '=' || c == '+' || c == '/'
		if !ok {
			break
		}
		end++
	}
	if end < 6 || end > 64 {
		return ""
	}
	return s[:end]
}

// startQRContent 让本机生成 code 与二维码内容，图由本机渲染（raccoon 形态）。
//
// ⚠️ 这一家不需要上游给句柄：实测任意自造 code 都被接受并进入 pending。
// 图不落盘、也不去图床：直接渲染成 data URL —— 让二维码在断网时也能画出来，
// 而外部图床那张图一旦加载失败，面板上就是一片空白格。
func startQRContent(ctx context.Context, spec Spec) (LoginSession, error) {
	raw, err := randomHex(16)
	if err != nil {
		return LoginSession{}, errors.New("生成扫码 code 失败：" + err.Error())
	}
	content := strings.ReplaceAll(spec.Login.QRContent, "{{code}}", raw)
	png, err := qrcode.Encode(content, qrcode.Medium, 320)
	if err != nil {
		return LoginSession{}, errors.New("渲染二维码失败：" + err.Error())
	}
	s := LoginSession{
		Channel: spec.ID, Kind: LoginQR,
		State:       raw,
		QRImage:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		IntervalMS:  2000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      spec.Login.Note,
	}
	return remember(s), nil
}

// --- V11 签名（officeace 的额度端点）------------------------------------------

// signHuaweiV11 就地写入 V11-HMAC-SHA256 签名头。
//
// 算法（逐字节复刻上游客户端的 B()/F()）：
//
//	密钥派生  u = HMAC-SHA256(accessKey, secretKey)
//	          k = HMAC-SHA256(u, scope||0x01) 取前 32 字节，**转 hex 当 key 用**
//	canonical METHOD \n URI(尾斜杠) \n Query \n 头行(每行 LF 结尾) \n 签名头名 \n hex(SHA256(body))
//	待签     V11-HMAC-SHA256 \n X-Sdk-Date \n scope \n hex(SHA256(canonical))
//	头       Authorization: V11-HMAC-SHA256 Credential=AK/scope, SignedHeaders=…, Signature=…
//
// ⚠️ 三条都曾导致 401，别凭直觉改：
//  1. GET 不带 Content-Type（客户端只在非 GET 时设它）。多这一个头，服务端
//     重算的 canonical 就多一行。
//  2. 派生密钥最后要转成 hex 字符串再用，不是直接用那 32 字节。
//  3. 头行与待签串都是 LF 结尾，canonical 路径强制尾斜杠。
//
// ⚠️ 与 signHuawei（SDK-HMAC-SHA256）不能互换：同一台网关对后者回 400
// `APIG.0624 Unsupported signature algorithm` —— 它只认 V11。
func signHuaweiV11(dst map[string]string, method, rawURL string, s QuotaSpec, a Account) error {
	ak := a.Field("access_key_id")
	sk := a.Field("secret_access_key")
	st := a.Field("security_token")
	if ak == "" || sk == "" {
		return errText("凭据缺少 AK/SK")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	uri := u.Path
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	date := time.Now().UTC().Format("20060102T150405Z")
	scope := date[:8] + "/cn-southwest-2/apic"
	payloadHash := sha256Hex(s.Body)

	heads := map[string]string{
		"host":                u.Host,
		"x-subscription-type": "v2",
		"x-sdk-date":          date,
	}
	if st != "" {
		heads["x-security-token"] = strings.TrimSpace(st)
	}
	// 规则 1：GET 不带 content-type。
	if !strings.EqualFold(method, "GET") {
		heads["content-type"] = "application/json;charset=utf8"
	}
	keys := make([]string, 0, len(heads))
	for k := range heads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+":"+strings.TrimSpace(heads[k])+"\n")
	}
	canonical := strings.Join([]string{
		strings.ToUpper(method), uri, canonicalQuery(u.RawQuery),
		strings.Join(lines, ""), strings.Join(keys, ";"), payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"V11-HMAC-SHA256", date, scope, sha256Hex(canonical),
	}, "\n")
	u1 := hmacSHA256([]byte(ak), sk)
	derived := hmacSHA256(u1, scope+"\x01")
	key := hex.EncodeToString(derived[:32])
	sig := hex.EncodeToString(hmacSHA256([]byte(key), stringToSign))

	dst["x-sdk-date"] = date
	dst["x-subscription-type"] = "v2"
	if st != "" {
		dst["x-security-token"] = strings.TrimSpace(st)
	}
	if !strings.EqualFold(method, "GET") {
		dst["content-type"] = "application/json;charset=utf8"
	}
	dst["authorization"] = "V11-HMAC-SHA256 Credential=" + ak + "/" + scope +
		", SignedHeaders=" + strings.Join(keys, ";") + ", Signature=" + sig
	return nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// --- DPoP（officeace 的 STS 换发要求）----------------------------------------

// dpopKey 是一份 ES256（P-256）密钥：STS 端点要求 DPoP 头带公钥 JWK，
// 而私钥只用来签这一次 proof（下一次换发用新的 —— 与上游客户端同形）。
type dpopKey struct {
	priv *ecdsa.PrivateKey
}

func newDPoPKey() (*dpopKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &dpopKey{priv: priv}, nil
}

// sign 签一份 dpop+jwt：htm 是 HTTP 方法、htu 是完整 URL。
func (k *dpopKey) sign(htm, htu string) (string, error) {
	pub := k.priv.PublicKey
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	jwk := `{"crv":"P-256","kty":"EC","x":"` + b64(pub.X.Bytes()) +
		`","y":"` + b64(pub.Y.Bytes()) + `"}`
	header := `{"typ":"dpop+jwt","alg":"ES256","jwk":` + jwk + `}`
	payload := `{"htm":"` + htm + `","htu":"` + htu + `","iat":` +
		strconv.FormatInt(time.Now().Unix(), 10) + `,"jti":"` + uuidV4() + `"}`
	input := b64([]byte(header)) + "." + b64([]byte(payload))
	digest := sha256.Sum256([]byte(input))
	r, s2, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// ECDSA 的签名按 JOSE 要求是 r||s 各定长（P-256 = 32 字节）。
	sig := make([]byte, 64)
	copy(sig[32-len(r.Bytes()):32], r.Bytes())
	copy(sig[64-len(s2.Bytes()):], s2.Bytes())
	return input + "." + b64(sig), nil
}

// randomHex 生成 n 字节随机数的 hex（用来铸票据密钥与扫码 code）。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// bigIntPad 只为让上面的 r/s 截取不越界（Go 的 big.Int.Bytes 已是定长以下）。
var _ = big.NewInt
