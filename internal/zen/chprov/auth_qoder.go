package chprov

// Qoder 的内置登录：本机的 Qoder IDE 不再是唯一凭据来源。
//
// # 为什么它不能只是「贴 Key」
//
// Qoder 的模型调用要一份 **cosy** 凭据（cosy_key + encrypt_user_info），而那份
// 凭据是客户端自己用一把随机 AES key 现场铸的：RSA 包住 AES key，AES-CBC 包住
// 用户身份。IDE 会把铸好的两份东西写进自己的缓存，所以「跟着 IDE 走」看起来
// 能用 —— 但它要求用户先装 IDE 并登录，且 IDE 一退出登录这条渠道就静默消失。
//
// 真实链路是设备码：授权页托管在 qoder.cn（/device/selectAccounts），换 token
// 在 openapi.qoder.com.cn（/api/v1/deviceToken/poll）。两者是**不同 host**，
// 把 poll 打到 qoder.cn 得 401、把授权页打到 openapi 得 404 —— 这不是端点坏了，
// 是两段各在自己的域名上。
//
// # 三个易错处，全部实测
//
//	404 是「用户还没在浏览器里点确认」，不是「端点不存在」 —— 要继续轮询
//	401/403 才是真拒绝（凭据被撤销 / 流程已作废）
//	换到 token 只是第一步：还要用 /api/v1/userinfo 读 uid，再铸 cosy 凭据
//
// 少了最后一步，凭据能落盘、面板会显示「已登录」，而每一次模型请求都 401。

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"zenui/internal/zen/qoderhub/remote"
)

const (
	// qoderClientID 是桌面客户端登记在授权服务上的公开 id（随客户端分发）。
	qoderClientID = "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	// qoderPollPending 是「还在等用户点确认」这一档的响应码。
	qoderPollPending = 404
)

// qoderAuthBase 是授权页所在域；qoderOpenAPIBase 是换 token 与读用户信息的域。
//
// 两者是 var 而不是 const，理由与 WorkBuddyBase 相同：这条链路的契约（字段名、
// 404 的含义、userinfo 之后还要铸 cosy）**只能靠假上游验证**，而假上游要能把
// host 换掉。真实取值写在这里，不在任何调用点。
var (
	qoderAuthBase    = "https://qoder.cn"
	qoderOpenAPIBase = "https://openapi.qoder.com.cn"
)

// startQoder 铸一次设备授权：本机生成 verifier / challenge / nonce，
// 授权页 URL 直接交给用户。
func startQoder(ctx context.Context, spec Spec) (LoginSession, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return LoginSession{}, err
	}
	nonce := uuidV4()
	machine := qoderMachineID()

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machine)
	q.Set("client_id", qoderClientID)
	selectURL := qoderAuthBase + "/device/selectAccounts?" + q.Encode()

	// 客户端把授权页包在 /users/sign-in 里：直接开 selectAccounts 会得到一个
	// 需要登录态的页面，未登录时用户看到的是空白而不是登录框。
	open := qoderAuthBase + "/users/sign-in?biz_variant=qoder&oauth_callback=" + url.QueryEscape(selectURL)

	s := LoginSession{
		Channel: spec.ID, Kind: LoginQoder,
		OpenURL:     open,
		State:       nonce,
		Verifier:    verifier,
		DeviceID:    machine,
		IntervalMS:  2000,
		ExpiresAtMS: time.Now().Add(loginTTL).UnixMilli(),
		StartedAtMS: time.Now().UnixMilli(),
		Detail:      "在浏览器里完成登录后回到这里：本机每 2 秒问一次授权结果。",
	}
	return remember(s), nil
}

// pollQoder 轮一次设备授权，成功时把 token 折成一份 cosy 凭据。
func pollQoder(ctx context.Context, spec Spec, sess LoginSession) (string, Account, error) {
	q := url.Values{}
	q.Set("nonce", sess.State)
	q.Set("verifier", sess.Verifier)
	q.Set("challenge_method", "S256")
	endpoint := qoderOpenAPIBase + "/api/v1/deviceToken/poll?" + q.Encode()

	headers := map[string]string{"accept": "application/json", "user-agent": "qoder/1.1.31"}
	code, raw, err := HTTPGet(ctx, endpoint, headers)
	if err != nil {
		return "", Account{}, err
	}
	if code == qoderPollPending {
		// 用户还没点确认。这一档是**正常态**，不是失败。
		return "pending", Account{}, nil
	}
	if code == 401 || code == 403 {
		return "", Account{}, errors.New("授权流程已作废或被拒绝，请重新发起登录")
	}
	if code < 200 || code >= 300 {
		return "", Account{}, errors.New("设备授权轮询失败：" + statusText(code, raw))
	}

	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", Account{}, errors.New("换 token 响应无法解析：" + short(raw))
	}
	// 字段名是**上游契约**，不是口味：客户端读的是 u.token + u.refresh_token
	// （并以此判「换到了没有」），device_token / access_token 只是兼容别名。
	//
	// ⚠️ 只认 device_token 的症状正是「登录失败：换 token 响应里没有 device_token」——
	// 浏览器那边已经确认了、授权也确实生效了，而这里把一份有效响应判成失败。
	// 三条候选按上游优先序读，最后一个才是我们自己的历史写法。
	token := firstNonEmpty(field(payload, "token"), field(payload, "device_token"), field(payload, "access_token"))
	if token == "" {
		return "", Account{}, errors.New("换 token 响应里没有 token（拿到的是：" + keysHint(payload) + "）")
	}

	acct := Account{
		AccessToken: token,
		RefreshTok:  field(payload, "refresh_token"),
		Source:      LoginQoder,
		CreatedAtMS: time.Now().UnixMilli(),
	}
	if v, ok := numeric(payload["expires_at"]); ok && v > 0 {
		acct.ExpiresAtMS = normalizeEpochMS(int64(v))
	} else if v, ok := numeric(payload["expires_in"]); ok && v > 0 {
		acct.ExpiresAtMS = time.Now().Add(time.Duration(v) * time.Second).UnixMilli()
	}

	// 换到 token 只是第一步。cosy 凭据要用 uid 铸，而 uid 只能从 userinfo 读。
	id, name, email, err := qoderUserInfo(ctx, token)
	if err != nil {
		return "", Account{}, err
	}
	acct.ID = id
	acct.Nickname = name
	cosyKey, cosyInfo, err := qoderMintCosy(id, name, email, token)
	if err != nil {
		return "", Account{}, err
	}
	acct.Extra = map[string]string{
		"cosy_key":          cosyKey,
		"encrypt_user_info": cosyInfo,
		"machine_id":        sess.DeviceID,
		"uid":               id,
	}
	// 内置 sidecar 读的是**这一份**凭据，而不是 IDE 的缓存：登录成功后立刻把它
	// 写过去，下一次模型请求才用得上刚拿到的手。写失败不回滚登录 —— 凭据已经
	// 在表里了，面板显示的是真实状态，只是这一轮模型请求还发不出去。
	_ = writeQoderSidecarCredential(acct)
	return "ok", acct, nil
}

// QoderSidecarAuthFile 是内置 Qoder 网关读凭据的那条路径。由 core 在启动时
// 设成数据目录下的固定位置：**同一个值**既给写（这里）也给读（sidecar 配置），
// 两处各拼一次路径迟早会拼成两个文件，症状是「登录成功了、模型请求仍 401」。
var QoderSidecarAuthFile string

// WriteQoderSidecarCredential 把一份凭据推进内置转发层。
//
// ⚠️ **三条写入路径都要走它**：设备授权轮询、贴 Key、从本机插件导入。只有设备
// 授权那一处调用时，症状是「贴了 Key / 导入成功、面板显示已登录，而模型请求
// 恒 401」—— 凭据在表里，转发层读的却是另一个文件，且不报任何错。
func WriteQoderSidecarCredential(acct Account) error { return writeQoderSidecarCredential(acct) }

// writeQoderSidecarCredential 把凭据折成 sidecar 认识的形状写盘。
//
// sidecar 的 schema 是它自己的（source / token_expire_time / auth{cosy_key,
// encrypt_user_info, user_id, machine_id, access_token}），不是本项目 Account
// 的形状：直接写 Account 会被它判成「缺 cosy_key」而静默退回 IDE 缓存。
func writeQoderSidecarCredential(acct Account) error {
	path := strings.TrimSpace(QoderSidecarAuthFile)
	if path == "" {
		return errors.New("未配置内置 Qoder 网关的凭据路径")
	}
	doc := map[string]any{
		"source": acct.Source,
		"auth": map[string]string{
			"cosy_key":          acct.Field("cosy_key"),
			"encrypt_user_info": acct.Field("encrypt_user_info"),
			"user_id":           firstNonEmpty(acct.ID, acct.Field("uid")),
			"machine_id":        acct.Field("machine_id"),
			"access_token":      acct.AccessToken,
		},
	}
	if acct.ExpiresAtMS > 0 {
		// sidecar 的 schema 把这一格当**毫秒**读（与 IDE 缓存的 expire_time 同单位），
		// 写秒会让它算出「1970 年就过期了」并把登录判成失效。
		doc["token_expire_time"] = strconv.FormatInt(acct.ExpiresAtMS, 10)
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// A failed rename leaves a plaintext sidecar credential (access_token,
		// cosy_key) in a stray .tmp next to the real file.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ClearQoderSidecarCredential 删掉转发层自己那份凭据文件。
//
// 退出渠道时必须调用：转发层每个请求都重读这个文件，留着它等于「面板说已经退出、
// 而每一个模型请求仍在用那份凭据跑」—— 用户点了退出、额度还在被花，且零报错。
// 托管账号显式退出后，QoderCredentials 返回空切片，阻止转发层回退 IDE 缓存。
func ClearQoderSidecarCredential() error {
	path := strings.TrimSpace(QoderSidecarAuthFile)
	if path == "" {
		return errors.New("未配置内置 Qoder 网关的凭据路径")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// QoderCredentials exposes snapshots to the embedded forwarder, so account
// switching never swaps a process-wide credential file while another call runs.
func QoderCredentials(accounts map[string]Accounts) []remote.Credential {
	_, haveMain := accounts["qoder"]
	_, haveAlias := accounts["qodercn"]
	if !haveMain && !haveAlias {
		return nil
	}
	out := make([]remote.Credential, 0)
	seen := map[string]bool{}
	for _, id := range []string{"qoder", "qodercn"} {
		for _, a := range accounts[id].Usable() {
			if seen[a.Key()] || a.Field("cosy_key") == "" || a.Field("encrypt_user_info") == "" {
				continue
			}
			seen[a.Key()] = true
			uid := a.ID
			if uid == "" {
				uid = a.Field("uid")
			}
			out = append(out, remote.Credential{CosyKey: a.Field("cosy_key"), EncryptUserInfo: a.Field("encrypt_user_info"),
				UserID: uid, MachineID: a.Field("machine_id"), AccessToken: a.AccessToken,
				Source: "channel:" + a.Key(), TokenExpireTime: a.ExpiresAtMS})
		}
	}
	return out
}

// qoderMintCosy 铸 cosy 凭据。与 IDE 现场铸的那份同形（那把 RSA 公钥就随
// 客户端分发，只能加密）；铸不出来就如实失败，不自造一份「看起来像」的材料 ——
// 那种凭据能落盘、面板会显示已登录，而每一次模型请求都 401。
func qoderMintCosy(uid, name, email, token string) (key, info string, err error) {
	return remote.MintCosyCredential(uid, name, email, token)
}

// qoderUserInfo 读当前 token 的账号身份。少了它，cosy 凭据就无法铸对。
func qoderUserInfo(ctx context.Context, token string) (id, name, email string, err error) {
	headers := map[string]string{
		"accept":        "application/json",
		"authorization": "Bearer " + token,
		"user-agent":    "qoder/1.1.31",
	}
	code, raw, err := HTTPGet(ctx, qoderOpenAPIBase+"/api/v1/userinfo", headers)
	if err != nil {
		return "", "", "", err
	}
	if code < 200 || code >= 300 {
		return "", "", "", errors.New("读取账号信息失败：" + statusText(code, raw))
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", "", "", errors.New("账号信息无法解析：" + short(raw))
	}
	id = field(payload, "id")
	if id == "" {
		id = field(payload, "user_id")
	}
	if id == "" {
		id = field(payload, "uid")
	}
	if id == "" {
		return "", "", "", errors.New("账号信息里没有用户标识")
	}
	name = field(payload, "name")
	if name == "" {
		name = field(payload, "username")
	}
	email = field(payload, "email")
	return id, name, email, nil
}

// keysHint 把响应的顶层字段名按字典序列出来，用在「字段名对不上」的报错里。
//
// # 为什么值得单独写一句
//
// 换 token 失败时最需要回答的问题是「上游到底给了什么形状」。只说「没有
// device_token」会把一次**字段改名**报成一次**登录失败**，而下一个人只能靠猜
// 或者抓包。字段名不含凭据值，可以放心回显。
func keysHint(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "空对象"
	}
	return strings.Join(keys, ", ")
}

// qoderMachineID 读本机的设备标识：IDE 与 CLI 都把它写在自己的配置目录里。
// 读不到就现铸一个 —— 它是「这台设备」的标识，不是登录凭据，换一个不会失效。
func qoderMachineID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return uuidV4()
	}
	for _, rel := range []string{
		filepath.Join(".qoder-cn", ".auth", "machine-id"),
		filepath.Join(".qoderwork", "machine-id"),
		filepath.Join(".qoder-cn", "machine-id"),
		filepath.Join(".qoder", "machine-id"),
	} {
		if body, err := os.ReadFile(filepath.Join(home, rel)); err == nil {
			if v := strings.TrimSpace(string(body)); v != "" {
				return v
			}
		}
	}
	return uuidV4()
}

// normalizeEpochMS 把上游的「秒或毫秒」统一成毫秒。
func normalizeEpochMS(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 1e12 {
		return v * 1000
	}
	return v
}
