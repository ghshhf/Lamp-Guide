package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zenui/internal/zen/channels"
	"zenui/internal/zen/chprov"
	"zenui/internal/zen/lane"
	"zenui/internal/zen/store"
)

// 渠道账号面（29 家的登录、凭据、额度）：把 chprov 的描述表接到管理端。
//
// # 三层分工
//
//	chprov        描述表 + 额度读取 + 四种登录形态（纯实现，不碰 store）
//	channels      展示层元数据（名字 / 厂商 / 协议族 / 档位）
//	本文件         凭据落盘、面板读数、管理端路由
//
// # 一条纪律：凭据只从「用户自己的动作」来
//
// WorkBuddy 与 Qoder 由本项目自己的登录 / sidecar 托管（lane 那两条路径）。
// 其余 27 家一律走 chprov 的四种形态 —— **不读任何客户端缓存、不读别人的磁盘**。
// 参考实现有一套「本机浏览器会话 / 客户端 session 文件」的来源，本项目刻意不迁：
// 那类来源在别人的机器上不存在，而它一旦失败，症状是「面板说没登录、其实浏览器里
// 明明登录着」—— 一条用户无法自救的谜题。

// chanAccounts 读某渠道当前的**全部**凭据（没有则空集）。
//
// 同一家渠道可以登录多个账号：额度按账号算，一个号烧完换下一个号才是
// 「这一家真的还能发请求」；只留一条会静默丢掉其余账号。
func (s *Server) chanAccounts(id string) chprov.Accounts {
	cfg := s.Store.Config()
	list := append(chprov.Accounts(nil), cfg.ChannelAccounts[id]...)
	if id == "qoder" || id == "qodercn" {
		for _, alias := range []string{"qoder", "qodercn"} {
			if alias == id {
				continue
			}
			for _, acct := range cfg.ChannelAccounts[alias] {
				list.Upsert(acct)
			}
		}
	}
	if id == "workbuddy" && cfg.WorkBuddy.AccessToken != "" {
		a := cfg.WorkBuddy
		legacy := chprov.Account{ID: a.UID, Nickname: a.Nickname, AccessToken: a.AccessToken,
			RefreshTok: a.RefreshToken, ExpiresAtMS: a.ExpiresAtMS, Source: "poll"}
		found := false
		for _, existing := range list {
			if existing.Key() == legacy.Key() {
				found = true
				break
			}
		}
		if !found {
			list = append(list, legacy)
		}
	}
	return list
}

// chanAccount 是「这一家的第一条可用凭据」，供只需要一份材料的调用方使用
// （例如额度读取：一次读一个账号的额度）。面板与计数走 chanAccounts。
func (s *Server) chanAccount(id string) chprov.Account {
	usable := s.chanAccounts(id).Usable()
	if len(usable) == 0 {
		return chprov.Account{}
	}
	return usable[0]
}

// chanReadQuota 读某渠道的额度。workbuddy / qoder 走 lane 的既有实现
// （令牌轮换与 sidecar 是本项目自己的能力），其余走 chprov 描述表。
//
// ⚠️ 同一个渠道**只能有一条读取路径**：两处各读一遍会让面板与排序拿到两份读数，
// 而它们的差异无从解释（参考实现为「同一语义两处实现」记过多次红线）。
func (s *Server) chanReadQuota(id string) chprov.Quota {
	qs := s.chanQuotas(id)
	if len(qs) == 0 {
		return chprov.Fail("未登录（没有可用凭据）")
	}
	return foldQuotas(qs)
}

// chanQuotas 逐账号读额度，返回「凭据 Key → 读数」。
//
// # 为什么逐账号读
//
// 同一家渠道的两个账号是两份独立的额度：A 号还剩 3 天、B 号还剩 20 天。
// 只读第一条会让面板把这一家说成「3 天后作废」，而实际上 B 号还能撑很久 ——
// 用户据此以为要充值，而问题不在那里。
func (s *Server) chanQuotas(id string) map[string]chprov.Quota {
	// WorkBuddy's real quota lives in the lane (token rotation +
	// WorkBuddyExpiry), not in chprov's generic channel table. The generic
	// reader cannot see a logged-in lane account and answers "no usable
	// credential", which silently dropped a funded own account out of the
	// aggregate chain and left only the free stations. qoder already took
	// this path inside the no-accounts branch below; WorkBuddy must not wait
	// for that branch to be reached.
	if id == "workbuddy" {
		q := s.cachedQuota("lane|"+id, func() chprov.Quota {
			q, ok := s.laneQuota(id)
			if !ok {
				return chprov.Fail("WorkBuddy 额度读取不可用")
			}
			return q
		})
		return map[string]chprov.Quota{"lane": q}
	}
	accts := s.chanAccounts(id).Usable()
	if len(accts) == 0 {
		// workbuddy / qoder 的凭据不在 ChannelAccounts 里（它们在 lane 与 store.WorkBuddy），
		// 故先问 lane，再问本表。
		if id == "qoder" || id == "qodercn" {
			cfg := s.Store.Config()
			_, primary := cfg.ChannelAccounts["qoder"]
			_, alias := cfg.ChannelAccounts["qodercn"]
			if primary || alias {
				return nil
			}
			q := s.cachedQuota("lane|"+id, func() chprov.Quota { q, _ := s.laneQuota(id); return q })
			return map[string]chprov.Quota{"lane": q}
		}
		return nil
	}
	// 一分钟内复用上一次的读数（与聚合面同一档）：这一面每次刷新都要逐账号
	// 串行打上游，一家三个号就是三次往返，三十家叠起来足够让整页卡住。
	// 缓存键带账号 Key：换了凭据的账号必须重新读，不能借旧读数冒充。
	out := make(map[string]chprov.Quota, len(accts))
	// 串行读：并行会让 20 个账号同时打上游，而那正是限流的形状。
	for _, acct := range accts {
		encoded, _ := json.Marshal(acct)
		cacheKey := fmt.Sprintf("%s|%s|%x", id, acct.Key(), sha256.Sum256(encoded))
		out[acct.Key()] = s.cachedQuota(cacheKey, func() chprov.Quota {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			if chprov.IsNativeChannel(id) {
				ctx = s.nativeContext(ctx)
			}
			return chprov.ReadQuota(ctx, id, acct)
		})
	}
	return out
}

// chanQuotaTTL 是逐账号额度读数的缓存窗口。
//
// 一分钟与聚合面的临期缓存同档：两个面读的是同一批上游端点，两套窗口会让
// 「渠道面说还剩 3 天、聚合面说还剩 20 天」在同一分钟内同时成立。
const chanQuotaTTL = time.Minute

// quotaRetryDelay is how long a failed quota read stays cached. A full
// chanQuotaTTL used to pin one unlucky 12s timeout onto a funded account for
// a whole minute — the "sometimes my account with credit is just ignored"
// symptom.
const quotaRetryDelay = 15 * time.Second

// 缓存是进程级的：它只存「刚刚读到的读数」，不存凭据，也不跨进程复用。
type quotaFlight struct {
	done chan struct{}
	q    chprov.Quota
}

func (s *Server) cachedQuota(key string, read func() chprov.Quota) chprov.Quota {
	s.quotaMu.Lock()
	if ent, ok := s.quotaCache[key]; ok && time.Since(ent.at) < chanQuotaTTL {
		s.quotaMu.Unlock()
		return ent.q
	}
	if pending := s.quotaPending[key]; pending != nil {
		s.quotaMu.Unlock()
		<-pending.done
		return pending.q
	}
	if s.quotaPending == nil {
		s.quotaPending = map[string]*quotaFlight{}
	}
	pending := &quotaFlight{done: make(chan struct{})}
	s.quotaPending[key] = pending
	generation := s.quotaGeneration
	s.quotaMu.Unlock()
	q := read()
	s.quotaMu.Lock()
	if s.quotaCache == nil {
		s.quotaCache = map[string]quotaEntry{}
	}
	at := time.Now()
	if q.Failed {
		// A transient read failure (timeout, upstream 5xx, a token refresh in
		// flight) must not make a funded account vanish from the chain. Reuse
		// the last successful reading when there is one, and never pin a
		// failure for the full cache window.
		if prev, ok := s.quotaCache[key]; ok && prev.q.OK() {
			q = prev.q
		}
		at = at.Add(-chanQuotaTTL + quotaRetryDelay)
	}
	if generation == s.quotaGeneration {
		s.quotaCache[key] = quotaEntry{q: q, at: at}
	}
	pending.q = q
	if s.quotaPending[key] == pending {
		delete(s.quotaPending, key)
	}
	close(pending.done)
	s.quotaMu.Unlock()
	return q
}

type quotaEntry struct {
	q  chprov.Quota
	at time.Time
}

// forgetQuota 丢掉某一家（或某一条）的额度读数。
//
// 写操作之后必须调用：贴了新区 Key、退出一个账号、导入一批凭据，这三件事都会
// 让上一分钟的读数变成谎话，而面板最不该在这时候说「还剩多久」。
//
// 参数三种粒度：id 空 = 整表作废（导入之后整批凭据都换了）；key 空 = 这一家全部
// 作废（退了整家或刚换掉一份）；两者都给 = 只作废那一条。
func (s *Server) forgetQuota(id string, key string) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quotaGeneration++
	if id == "" && key == "" {
		s.quotaCache = nil
		s.quotaPending = nil
		return
	}
	for k := range s.quotaPending {
		if (key == "" && (strings.HasPrefix(k, id+"|") || k == "lane|"+id)) ||
			(key != "" && strings.HasPrefix(k, id+"|"+key+"|")) {
			delete(s.quotaPending, k)
		}
	}
	for k := range s.quotaCache {
		if key == "" {
			if strings.HasPrefix(k, id+"|") || k == "lane|"+id {
				delete(s.quotaCache, k)
			}
			continue
		}
		if strings.HasPrefix(k, id+"|"+key+"|") {
			delete(s.quotaCache, k)
		}
	}
}

// foldQuotas 把一组账号读数折成这一家的一个结论。
//
// 折叠规则（与轮换的判据同源）：
//
//	任一账号读到真实到期 → 取**最早**的那个（先烧最快作废的，和排序口径一致）
//	否则任一账号是永久额度 → Never（还能发请求，只是没有到期时刻）
//	否则任一账号读失败   → Failed（这次读不到，仍可作为最后一站）
//	否则                → 零值（上游对每一份都说不可用）
//
// ⚠️ 判「这一家还能不能发请求」时看的是**有没有一条能用的**，不是「全都好用」：
// 一个号过期了、另一个号还好着，这一家当然还能发。把「全都好用」当门槛会让
// 第二个号白登。
func foldQuotas(qs map[string]chprov.Quota) chprov.Quota {
	var best chprov.Quota
	known, never, failed, unusable := false, false, false, 0
	details := make([]string, 0, len(qs))
	for _, q := range qs {
		switch {
		case q.Known:
			if q.ExpiryMS <= time.Now().UnixMilli() {
				continue
			}
			if !known || q.ExpiryMS < best.ExpiryMS {
				best = q
			}
			known = true
		case q.Never:
			never = true
		case q.Failed:
			failed = true
			if q.Detail != "" {
				details = append(details, q.Detail)
			}
		default:
			unusable++
		}
	}
	switch {
	case known:
		if len(qs) > 1 {
			best.Detail = strconv.Itoa(len(qs)) + " 个账号里最早的到期时刻"
		}
		return best
	case never:
		return chprov.Quota{Never: true, Detail: strconv.Itoa(len(qs)) + " 个账号里没有到期时刻，按永久参与"}
	case failed:
		sortStrings(details)
		return chprov.Fail(strings.Join(details, "；"))
	}
	_ = unusable
	return chprov.Unusable()
}

// laneQuota 把 workbuddy / qoder 的既有读数折成 chprov.Quota。
func (s *Server) laneQuota(id string) (chprov.Quota, bool) {
	switch id {
	case "workbuddy":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exp, err := s.Lane.WorkBuddyExpiry(ctx)
		if err != nil {
			return chprov.Fail(err.Error()), true
		}
		return laneExpiryToQuota(exp.Known, exp.Never, exp.ExpiryMS), true
	case "qoder", "qodercn":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exp, err := lane.QoderExpiry(ctx, qoderGet)
		if err != nil {
			return chprov.Fail(err.Error()), true
		}
		return laneExpiryToQuota(exp.Known, exp.Never, exp.ExpiryMS), true
	}
	return chprov.Quota{}, false
}

// laneExpiryToQuota 把 lane 的两态读数折成 chprov 的三态。
//
// ⚠️ lane 那边没有 Failed 这一档（它把读失败也折成零值）。这里**不补**：补上就得
// 改 lane 的语义，而那会连带改动轮换的既有判据。故 workbuddy / qoder 读失败时
// 在面板上表现为「不可用」，其余 27 家表现为「这次读不到」—— 这个差异写进 note，
// 用户看得见。（下一步的收口是把 lane 也升成三态，那是一次独立改动。）
func laneExpiryToQuota(known, never bool, ms int64) chprov.Quota {
	switch {
	case known:
		return chprov.Quota{Known: true, ExpiryMS: ms}
	case never:
		return chprov.Quota{Never: true}
	}
	return chprov.Unusable()
}

// --- 管理端路由 ---------------------------------------------------------------

// adminChannelImport 是 POST channel/import：从本机参考插件的凭据库导入已登录的渠道。
//
// # 为什么这条路径必须有
//
// 用户在这台机器上已经在那边登录过 7 家；不接这条路径，他得逐家重扫一次码，
// 而那些凭据**就在盘上**、也是他自己授权的。参考实现的各家 *-auth 同样先找
// 「本机既有会话」，找不到才走新授权。
//
// # 三条纪律（与 chprov 里那份实现一一对应）
//
//   - **只读**：绝不回写那些文件（它们是插件与客户端的资产）；
//   - **白名单**：只搬本表认识的渠道，别的 key 一律不碰；
//   - **如实报**：导入了哪几家、跳过了哪几家、为什么。
func (s *Server) adminChannelImport(w http.ResponseWriter, r *http.Request) {
	accts, res := chprov.ImportAccounts()
	if len(accts) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "imported": 0, "result": res})
		return
	}
	added := 0
	s.Store.Mutate(func(c *store.Config) {
		if c.ChannelAccounts == nil {
			c.ChannelAccounts = map[string]chprov.Accounts{}
		}
		for id, list := range accts {
			// 已经有**这个账号**的不覆盖：用户手工贴的那份可能比插件里更新
			// （例如他刚在自己面板换过 key），而「导入」只该补齐空缺。
			// 但同一家的**另一个账号**要照收：多账号是这条导入路径存在的理由之一。
			cur := c.ChannelAccounts[id]
			if s.chanSingle(id) && len(cur.Usable()) > 0 {
				res.Skipped[id] = "本机已有凭据，保留现有的（不覆盖）"
				continue
			}
			for _, acct := range list {
				if cur.Upsert(acct) {
					added++
					continue
				}
				res.Skipped[id+"/"+acct.Key()] = "本机已有同一账号的凭据，保留现有的（不覆盖）"
			}
			c.ChannelAccounts[id] = cur
		}
	})
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 导进来的都是新凭据：整表读数作废，否则面板会拿旧的「还剩多久」冒充它们。
	s.forgetQuota("", "")
	s.syncOpenCode()
	s.queueNativeCatalogRefresh()
	/* 导入同样要接线：这条路径只往 ChannelAccounts 里写，而 WorkBuddy 的目录由
	   lane 的快照决定、Qoder 的请求由转发层那个文件决定。不接着做，用户「导入
	   成功、面板显示 7 家已登录」，而目录里一条 WorkBuddy 模型都没有、Qoder 的
	   请求仍是 401 —— 与逐家重新登录之后的行为不一致，且零报错。 */
	if len(accts["workbuddy"]) > 0 {
		s.syncWorkBuddyLane()
	}
	for _, id := range []string{"qoder", "qodercn"} {
		if list := accts[id].Usable(); len(list) > 0 {
			s.syncQoderLane(list[len(list)-1])
			break
		}
	}
	if s.logger != nil {
		s.logger.Infof("渠道凭据已从本机插件导入：%d 家 / %d 个账号", len(accts), added)
	}
	/* 家数和**账号数**要分开报：同一家的第二个号也导进来了，只说「导入 3 家」
	   会让用户以为那 5 个号里只进来了 3 个。 */
	writeJSON(w, 200, map[string]any{
		"ok": true, "imported": len(accts), "accounts": added, "result": res,
	})
}

// adminChannelState 是 GET channel/state：每家一条完整读数（凭据状态 + 额度 + 登录形态）。
//
// 输出契约：
//
//	channels[]  id/name/vendor/protocol/authForm/baseUrl/tier/creditUnit/dailyClaim
//	            visionNote/note/accounts/accountList[]/wired/spec(额度端点)/login(形态)/quota
//	totals      家数 / 参与轮换数 / 已登录数 / 读到临期数 / 三天内作废数 / 账号总数
//
// ⚠️ accountList[] 逐条只报「标签 / 身份 / 来源 / 有没有附加字段 / 这一条的额度读数」，
// **不回显令牌**：一次截图就带走全部凭据的风险比「面板上少显示几个字符」大得多。
func (s *Server) adminChannelState(w http.ResponseWriter, r *http.Request) {
	channelList := channels.Channels()
	quotaRows := make([]map[string]chprov.Quota, len(channelList))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, c := range channelList {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			quotaRows[i] = s.chanQuotas(c.ID)
			<-slots
		}()
	}
	wg.Wait()
	rows := make([]map[string]any, 0, len(channels.Channels()))
	loggedIn, withExpiry, soon, accountCount := 0, 0, 0, 0
	now := time.Now()
	for i, c := range channelList {
		accts := s.chanAccounts(c.ID).Usable()
		accountCount += len(accts)
		// 有凭据或有 lane 账号才算「已登录」：两者都是「这一家现在发得出请求」。
		has := len(accts) > 0 || s.aggAccounts(c.ID) > 0
		if has {
			loggedIn++
		}
		spec, _ := chprov.SpecOf(c.ID)
		// 逐账号读一次额度：面板要能说「3 个账号里最早的那个 5 天后作废」，
		// 而不是只报第一条的读数（那条可能恰好是刚充值过的那个号）。
		quotas := quotaRows[i]
		q := chprov.Quota{Failed: true, Detail: "未登录（没有可用凭据）"}
		if len(quotas) > 0 {
			q = foldQuotas(quotas)
		}
		if q.OK() {
			withExpiry++
		}
		if q.Known && time.UnixMilli(q.ExpiryMS).Sub(now) <= 3*24*time.Hour {
			soon++
		}
		list := make([]map[string]any, 0, len(accts))
		for _, a := range accts {
			var qp *chprov.Quota
			if q, ok := quotas[a.Key()]; ok {
				qq := q
				qp = &qq
			}
			list = append(list, chanAccountJSONWithQuota(a, qp))
		}
		row := map[string]any{
			"id": c.ID, "name": c.Name, "vendor": c.Vendor,
			"protocol": c.Protocol, "authForm": c.AuthForm, "baseUrl": c.BaseURL,
			"tier": c.Tier, "creditUnit": c.CreditUnit, "dailyClaim": c.DailyClaim,
			"visionNote": c.VisionNote, "note": c.Note,
			"wired": c.ExpiryWired, "loggedIn": has, "accounts": s.aggAccounts(c.ID),
			// routable 与聚合面同源：这一家现在真的发得出模型请求吗。凭据读得到
			// 额度、却没有任何推理实现的渠道，这里如实写 false。
			"routable":     s.channelRoutable(c.ID),
			"accountCount": len(accts), "accountList": list,
			"single": s.chanSingle(c.ID),
			"quota": map[string]any{
				"known": q.Known, "never": q.Never, "failed": q.Failed,
				"expiryMs": q.ExpiryMS, "detail": q.Detail, "remaining": q.Remaining,
			},
			"spec": map[string]any{
				"baseUrl": spec.Quota.BaseURL, "path": spec.Quota.Path,
				"method": spec.Quota.Method, "auth": spec.Quota.Auth,
				"extraKeys": spec.ExtraKeys,
			},
			"login": map[string]any{
				"kind": spec.Login.Kind, "pasteLabel": spec.Login.PasteLabel,
				"pasteHint": spec.Login.PasteHint, "note": spec.Login.Note,
				"deviceUrl": spec.Login.DeviceURL, "authUrl": spec.Login.AuthURL,
				"startUrl": spec.Login.StartURL, "qrUrl": spec.Login.QRURL,
			},
			// tokenHint 保留给只有一条凭据的老调用方（它等价于第一条的摘要）。
			"tokenHint": func() map[string]any {
				if len(accts) == 0 {
					return tokenHint(chprov.Account{})
				}
				return tokenHint(accts[0])
			}(),
		}
		if chprov.IsNativeChannel(c.ID) {
			required := chprov.QuotaExtraKeys(c.ID)
			if required == nil {
				required = []string{}
			}
			row["spec"].(map[string]any)["requiredExtraKeys"] = required
			row["catalogCount"] = len(s.nativeModels()[c.ID])
			for _, a := range accts {
				s.nativeCatalogMu.RLock()
				entry, exists := s.nativeCatalog[nativeCatalogKey(c.ID, a)]
				s.nativeCatalogMu.RUnlock()
				if exists && entry.Error != "" {
					row["catalogError"] = entry.Error
					break
				}
			}
		}
		rows = append(rows, row)
	}
	/* 供应商是**另一套东西**，必须在这里如实交代，否则这一面会显得"漏了东西"：
	   用户在「供应商」面接进来的 4 家，只有 3 家的 id 恰好与渠道表同名，第 4 家
	   （intern）根本不在渠道表里 —— 它只走 relay，没有额度端点、没有登录形态。
	   不给这句，读的人只能得出"渠道页没显示出我接的供应商"这个错误结论。 */
	provs := s.channelProviderCrosswalk()
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"channels":  rows,
		"providers": provs,
		"totals": map[string]any{
			"channels": len(rows), "wired": len(channels.WiredIDs()),
			"loggedIn": loggedIn, "withQuota": withExpiry, "expiringSoon": soon,
			"accounts":  accountCount,
			"providers": len(provs), "shared": countShared(provs),
		},
	})
}

// channelProviderCrosswalk 把「供应商」面的每一家对到「渠道」面的同名渠道。
//
// # 为什么这张表必须存在
//
// 两个面吃的是两份不同的数据：供应商是用户自己填的 OpenAI 兼容端点（relay 直连，
// 不需要登录，只要 Key），渠道是编译期那张 30 家的清单（要登录、有额度端点、
// 参与跨渠道轮换）。它们只在「id 恰好同名」时才重叠 —— qoder 就是靠这个 id 把
// 托管供应商与渠道接上的（见 aggCatalogs / aggTargetOf）。
//
// 所以「我接了 4 家供应商，渠道页只显示 3 家」不是漏显示，而是第 4 家没有对应的
// 渠道。这一面若不说这句话，用户只会去反复点刷新。
//
// 输出：id/name/enabled/models/channel(bool)/channelName/hasKey
func (s *Server) channelProviderCrosswalk() []map[string]any {
	cfg := s.Store.Config()
	out := make([]map[string]any, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		ch, linked := channels.ByID(p.ID)
		row := map[string]any{
			"id": p.ID, "name": p.Name, "enabled": p.Enabled,
			"hasKey": p.APIKey != "", "models": len(p.Models),
			"channel": linked,
		}
		if linked {
			row["channelName"] = ch.Name
		}
		out = append(out, row)
	}
	return out
}

// countShared 数「既有供应商又有同名渠道」的那几家：页头那句话说这个数。
func countShared(rows []map[string]any) int {
	n := 0
	for _, r := range rows {
		if b, _ := r["channel"].(bool); b {
			n++
		}
	}
	return n
}

// chanAccountJSON 是一条凭据的面板摘要：身份 / 标签 / 来源 / 附加字段清单，**不含令牌**。
func chanAccountJSON(a chprov.Account) map[string]any { return chanAccountJSONWithQuota(a, nil) }

// chanAccountJSONWithQuota 同上，另带这一条自己的额度读数（指针：nil 表示"没读"）。
func chanAccountJSONWithQuota(a chprov.Account, q *chprov.Quota) map[string]any {
	out := map[string]any{
		"key": a.Key(), "label": a.Label(), "source": a.Source,
		"id": a.ID, "nickname": a.Nickname,
	}
	if a.CreatedAtMS > 0 {
		out["createdAtMs"] = a.CreatedAtMS
	}
	if a.ExpiresAtMS > 0 {
		out["expiresAtMs"] = a.ExpiresAtMS
	}
	if len(a.Extra) > 0 {
		keys := make([]string, 0, len(a.Extra))
		for k := range a.Extra {
			keys = append(keys, k)
		}
		sortStrings(keys)
		out["extraKeys"] = keys
	}
	if q != nil {
		out["quota"] = map[string]any{
			"known": q.Known, "never": q.Never, "failed": q.Failed,
			"expiryMs": q.ExpiryMS, "detail": q.Detail, "remaining": q.Remaining,
		}
	}
	return out
}

// tokenHint 只说「有没有凭据、什么时候拿到的」，**不回显令牌本体**。
//
// ⚠️ 面板上回显 token 会让一次截图泄漏全部凭据，而用户从截图里看不出那是敏感信息。
func tokenHint(a chprov.Account) map[string]any {
	h := map[string]any{"present": !a.Empty(), "source": a.Source}
	if a.Nickname != "" {
		h["nickname"] = a.Nickname
	}
	if a.ExpiresAtMS > 0 {
		h["expiresAtMs"] = a.ExpiresAtMS
	}
	if a.CreatedAtMS > 0 {
		h["createdAtMs"] = a.CreatedAtMS
	}
	if a.Extra != nil {
		// 附加字段只报「在不在」，不回显值：cookie 与 AK/SK 同样是凭据。
		keys := make([]string, 0, len(a.Extra))
		for k := range a.Extra {
			keys = append(keys, k)
		}
		sortStrings(keys)
		h["extraKeys"] = keys
	}
	return h
}

// --- 写入与登录 ---------------------------------------------------------------

// adminChannelLogin 是 POST channel/login：按描述表的形态开一次授权。
func (s *Server) adminChannelLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	id := strings.TrimSpace(in.ID)
	spec, ok := chprov.SpecOf(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "渠道不存在: " + id})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := chprov.StartLogin(ctx, id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "kind": spec.Login.Kind})
		return
	}
	if s.logger != nil {
		s.logger.Infof("渠道 %s 开始登录（形态 %s）", id, sess.Kind)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "session": sess, "kind": spec.Login.Kind})
}

// adminChannelPoll 是 GET channel/poll?id=…：轮一次登录。成功后凭据落盘。
func (s *Server) adminChannelPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "缺少 id"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	state, acct, err := chprov.Poll(ctx, id)
	if err != nil {
		chprov.Forget(id)
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "state": "error"})
		return
	}
	if state != "ok" {
		writeJSON(w, 200, map[string]any{"ok": true, "state": state})
		return
	}
	added := s.saveChannelAccount(id, acct, s.chanSingle(id))
	chprov.Forget(id)
	if s.logger != nil {
		s.logger.Infof("渠道 %s 已登录（来源 %s，第 %d 个账号）", id, acct.Source, len(s.chanAccounts(id)))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": "ok", "added": added, "account": chanAccountJSON(acct), "quota": func() map[string]any {
		q := s.chanReadQuota(id)
		return map[string]any{"known": q.Known, "never": q.Never, "failed": q.Failed,
			"expiryMs": q.ExpiryMS, "detail": q.Detail, "remaining": q.Remaining}
	}()})
}

// chanSingle 报告这家渠道是否**只能有一个账号**。
//
// qoder / qodercn 的模型请求走本机那一个 sidecar，而 sidecar 只认**一份**凭据：
// 面板上挂第二个账号不会让它多发一次请求，只会多出一行永远轮不到的重复项。
// 注意这一条与「凭据在哪张表里」无关 —— 内置登录之后凭据就在本表里，单份的
// 理由变成了 sidecar 的形状，而不是「由谁托管」。
//
// workbuddy 同理：本机只有一份登录态。其余 27 家是真多账号。
func (s *Server) chanSingle(id string) bool {
	return false
}

// saveChannelAccount 写回一条凭据（默认 upsert：同账号覆盖、新账号追加）。
//
// ⚠️ 登录成功后**立刻**读一次额度：读得到的临期写进面板，读不到也如实记一句 ——
// 让「登录成功了、但这家读不到额度」与「登录成功了、额度是未知的」在同一屏上分开。
func (s *Server) saveChannelAccount(id string, acct chprov.Account, replaceAll bool) bool {
	added := false
	s.Store.Mutate(func(c *store.Config) {
		if c.ChannelAccounts == nil {
			c.ChannelAccounts = map[string]chprov.Accounts{}
		}
		list := c.ChannelAccounts[id]
		if replaceAll {
			// 单账号形态（workbuddy / qoder 的 sidecar 只认一份）换 token 时整组替换，
			// 否则旧的那条会和新的一条并排，而实际只有新的一条能发请求。
			list = chprov.Accounts{acct}
		} else {
			added = list.Upsert(acct)
		}
		c.ChannelAccounts[id] = list
	})
	if err := s.Store.Save(); err != nil && s.logger != nil {
		s.logger.Errorf("渠道 %s 的凭据落盘失败: %v", id, err)
	}
	// 换了凭据，上一分钟的读数就不再是这一条的读数：贴错 Key 后若还显示
	// 「还剩 20 天」，用户会以为已经接入成功。
	//
	// 作废整家而不是只作废这一条：Upsert 覆盖的是同 Key 的那一条，而
	// 「新增」会改动折叠结果（这一家的结论取决于全部账号）。
	s.forgetQuota(id, "")
	if chprov.IsNativeChannel(id) {
		s.queueNativeCatalogRefresh()
	}
	if id == "opencode" {
		s.syncOpenCode()
	}
	if id == "workbuddy" {
		s.syncWorkBuddyLane()
	}
	if id == "qoder" || id == "qodercn" {
		s.syncQoderLane(acct)
	}
	return added
}

// syncQoderLane 把凭据表里的 Qoder 凭据推进内置转发层，并让供应商目录跟上。
//
// # 为什么必须在写入之后立刻做
//
// 内置转发层读的是自己那个凭据文件（chprov.QoderSidecarAuthFile），而渠道面
// 的三条写入路径（设备授权 / 贴 Key / 导入）都只往 ChannelAccounts 里写。
// 少了这一步，用户「贴了 Key、面板显示已登录、额度也读得到」，而模型请求恒 401
// —— 转发层手里还是旧文件（或根本没有文件），且没有任何一处报错。
//
// 目录重建也是同一个理由：转发层换了手，模型列表要跟着重探一次，否则选择器
// 里还是上一份凭据能看到的那些模型。
func (s *Server) syncQoderLane(acct chprov.Account) {
	// 没有 cosy 凭据就别写：forward 层会把一份缺 cosy_key 的文件判成不可用，
	// 然后退回 IDE 缓存 —— 那比不写更难排查。此时如实记一句，面板上的凭据行
	// 仍然是真实的（凭据确实在表里，只是这一份转发层用不了）。
	if acct.Field("cosy_key") == "" || acct.Field("encrypt_user_info") == "" {
		if s.logger != nil {
			s.logger.Errorf("渠道 %s 的凭据缺少 cosy_key/encrypt_user_info，未推进转发层", acct.Label())
		}
		return
	}
	if err := chprov.WriteQoderSidecarCredential(acct); err != nil {
		if s.logger != nil {
			s.logger.Errorf("渠道 %s 的凭据未能写入内置 Qoder 转发层: %v", acct.Label(), err)
		}
		return
	}
	// 落盘之后立刻探一次（force）并同步供应商：缓存窗口是一分钟，不 force 的话
	// 刚换的手要等下一次页面刷新才生效。探活要打上游，故不拖住这次登录的响应。
	go func() {
		st := probeQoderCached(true)
		s.ensureQoderProvider(st)
	}()
}

// syncWorkBuddyLane 把渠道表里的 WorkBuddy 凭据推进 lane，并重建目录。
//
// # 为什么必须在写入之后立刻做
//
// 渠道面的「登录」写的是 channelAccounts.workbuddy，而模型目录由 lane 的账号
// 快照决定。少了这一步，用户登录成功、面板显示「已登录、还剩 N 天」，而模型
// 选择器里一条 WorkBuddy 模型都没有 —— 直到下次重启才生效。这个症状与
// 「没登录」在界面上完全一样，用户唯一的自救方式是重启，而他不会想到。
//
// 目录重建是 goroutine：它要打上游、写盘，不该拖住这次登录的响应。
func (s *Server) syncWorkBuddyLane() {
	cfg := s.Store.Config()
	/* 登录这件事本身就是「启用」：渠道面登录之后把那格开关写成 true。
	   不写的话，设置页那个开关停在 false（它只在有人动过设置页时才被落盘），
	   而重启时 core 按持久化的开关折叠账号 —— 目录会把刚登录的模型收回去，
	   面板上却还写着「已登录」。 */
	s.Store.Mutate(func(c *store.Config) { c.WorkBuddy.Enabled = true })
	_ = s.Store.Save()
	cfg.WorkBuddy.Enabled = true
	s.Lane.SetWorkBuddyChannels(cfg.ChannelAccounts["workbuddy"], cfg.WorkBuddy)
	go s.Lane.RefreshCatalog(context.Background())
}

// adminChannelToken 是 POST channel/token：贴 Key 形态，也用于「手工换一份 token」。
//
// ⚠️ 这是 29 家里 12 家的**唯一**登录形态（不是退路）：它们的官方客户端本身就贴 Key。
// 把这条路径当成「降级方案」会让面板只给一半渠道留入口。
func (s *Server) adminChannelToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string            `json:"id"`
		Token    string            `json:"token"`
		Refresh  string            `json:"refresh"`
		Nickname string            `json:"nickname"`
		Extra    map[string]string `json:"extra"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	id := strings.TrimSpace(in.ID)
	if _, ok := chprov.SpecOf(id); !ok {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "渠道不存在: " + id})
		return
	}
	token := strings.TrimSpace(in.Token)
	if token == "" && len(in.Extra) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "token 与附加字段都是空的"})
		return
	}
	acct := chprov.Account{
		AccessToken: token, RefreshTok: strings.TrimSpace(in.Refresh),
		Nickname: strings.TrimSpace(in.Nickname), Extra: in.Extra,
		Source: "paste", CreatedAtMS: time.Now().UnixMilli(),
	}
	if id == "autoclaw" || id == "autoclawintl" {
		acct.AccessToken = strings.TrimSpace(strings.TrimPrefix(acct.AccessToken, "Bearer "))
		if acct.RefreshTok == "" {
			acct.RefreshTok = acct.Extra["refresh_token"]
		}
		delete(acct.Extra, "refresh_token")
	}
	// uid / 到期时刻能从 JWT 读就读（面板据此显示「还剩多久」）。
	if sub, _, exp, ok := chprov.JWTClaims(acct.AccessToken); ok {
		if acct.ID == "" {
			acct.ID = sub
		}
		acct.ExpiresAtMS = exp
	}
	// 贴 Key 是**加一个账号**，不是换掉整组：同一家的第二个 Key 也走这个框。
	added := s.saveChannelAccount(id, acct, false)
	// 立刻读一次额度：让「贴错了 Key」当场显现，而不是等到下一次请求 401。
	//
	// ⚠️ 读的是**这一条的**额度，不是折叠后的这一家：用户刚贴进来的 Key 是否有效，
	// 只有这一条的读数能回答；折叠结果可能被另一个好账号盖住。
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if chprov.IsNativeChannel(id) {
		ctx = s.nativeContext(ctx)
	}
	q := chprov.ReadQuota(ctx, id, acct)
	if s.logger != nil {
		s.logger.Infof("渠道 %s 凭据已保存（%s），额度读数: %s", id, acct.Label(), chprov.ChannelQuotaDetail(chprov.MustSpec(id), q))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "added": added,
		"accountCount": len(s.chanAccounts(id).Usable()), "account": chanAccountJSON(acct),
		"quota": map[string]any{
			"known": q.Known, "never": q.Never, "failed": q.Failed,
			"expiryMs": q.ExpiryMS, "detail": q.Detail, "remaining": q.Remaining,
		}})
}

// adminChannelLogout 是 POST channel/logout：清掉**一条**凭据，或清掉整家（不带 key）。
//
// ⚠️ 一定要能只退一个账号：「退出」把这一家的两个号一起清掉，用户的动作与
// 他看见的那一行文字（那一个账号）对不上，而另一个号是他还要留着的。
func (s *Server) adminChannelLogout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	id := strings.TrimSpace(in.ID)
	key := strings.TrimSpace(in.Key)
	qoderFamily := id == "qoder" || id == "qodercn"
	removed := 0
	all := false
	s.Store.Mutate(func(c *store.Config) {
		if c.ChannelAccounts == nil {
			c.ChannelAccounts = map[string]chprov.Accounts{}
		}
		if qoderFamily {
			for _, alias := range []string{"qoder", "qodercn"} {
				if key == "" {
					removed += len(c.ChannelAccounts[alias])
					c.ChannelAccounts[alias] = chprov.Accounts{}
					continue
				}
				next, hit := c.ChannelAccounts[alias].Remove(key)
				if hit {
					removed++
					if next == nil {
						next = chprov.Accounts{}
					}
					c.ChannelAccounts[alias] = next
				}
			}
			all = key == ""
			return
		}
		list := c.ChannelAccounts[id]
		if id == "workbuddy" && c.WorkBuddy.AccessToken != "" {
			legacy := chprov.Account{ID: c.WorkBuddy.UID, AccessToken: c.WorkBuddy.AccessToken}
			if key == "" || legacy.Key() == key {
				c.WorkBuddy = lane.WBAccount{}
				removed++
			}
		}
		if key == "" {
			// 不带 key = 整家退出（面板上那一行的「退出」按钮就是它）。
			removed += len(list)
			all = true
			delete(c.ChannelAccounts, id)
			return
		}
		next, hit := list.Remove(key)
		if hit {
			removed = 1
			if len(next) == 0 {
				delete(c.ChannelAccounts, id)
			} else {
				c.ChannelAccounts[id] = next
			}
		}
	})
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 只有整家退出才丢掉进行中的登录会话：退一个账号时，另一个号的授权可能正等着轮询。
	if all {
		chprov.Forget(id)
	}
	s.forgetQuota(id, key)
	if qoderFamily {
		s.forgetQuota("qoder", "")
		s.forgetQuota("qodercn", "")
	}
	if qoderFamily && (all || removed > 0) {
		// 整家退出必须连转发层那份凭据一起收：它每个请求都重读那个文件，只清
		// ChannelAccounts 等于「面板说退出了，而请求仍在用那份凭据发出去」。
		// 与之配套把托管供应商停用，模型才会真的从选择器里消失。
		remaining := s.chanAccounts("qoder").Usable()
		if len(remaining) > 0 {
			s.syncQoderLane(remaining[0])
		} else {
			s.ensureQoderProvider(qoderStatus{})
			if err := chprov.ClearQoderSidecarCredential(); err != nil && s.logger != nil {
				s.logger.Errorf("清掉内置 Qoder 转发层凭据失败: %v", err)
			}
		}
		go func() {
			st := probeQoderCached(true)
			s.ensureQoderProvider(st)
		}()
	}
	if id == "workbuddy" && (all || removed > 0) {
		// 整家退出必须连 lane 那一格一起清：两处都是「这一家的账号」，只清一处
		// 会让另一个格的旧凭据把模型目录续回来 —— 用户以为退出了，模型还在选。
		s.Store.Mutate(func(c *store.Config) {
			legacy := chprov.Account{ID: c.WorkBuddy.UID, AccessToken: c.WorkBuddy.AccessToken}
			if all || legacy.Key() == key {
				c.WorkBuddy = lane.WBAccount{}
			}
		})
		_ = s.Store.Save()
		s.Lane.WorkBuddyLogout()
		if len(s.chanAccounts(id).Usable()) > 0 {
			s.syncWorkBuddyLane()
		}
		go s.Lane.RefreshCatalog(context.Background())
	}
	if id == "opencode" {
		s.syncOpenCode()
	}
	if s.logger != nil {
		if all {
			s.logger.Infof("渠道 %s 已整家退出（清掉 %d 条凭据）", id, removed)
		} else {
			s.logger.Infof("渠道 %s 退出一个账号（%s），剩 %d 条", id, key, len(s.chanAccounts(id).Usable()))
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "removed": removed,
		"remaining": len(s.chanAccounts(id).Usable())})
}

// sortStrings 是本文件用到的稳定排序（避免为了一个调用点多引一个包在头部）。
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
