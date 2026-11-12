package gateway

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zenui/internal/zen/aggregate"
	"zenui/internal/zen/channels"
	"zenui/internal/zen/lane"
	"zenui/internal/zen/logx"
	"zenui/internal/zen/store"
)

// 聚合面（跨渠道选型）的管理端：把 aggregate 包的纯决策层接上真实数据。
//
// 三层分工，缺一层都不行：
//
//	aggregate 包    归一键 / 排序 / 挑号，只吃参数，不碰 IO
//	channels 包     编译期渠道表：哪家参与轮换、哪家只有直连
//	本文件          把 lane 目录、供应商目录、额度端点的到期时刻拼成一张表
//
// # 为什么额度到期时刻要缓存
//
// workbuddy 的额度端点与 qoder 的 sidecar 都要走上游/本地进程，一次页面刷新
// 若逐条现读，三家的延迟叠起来，这一面会比其它六面慢一个量级。缓存窗口取
// 一分钟（与 workbuddy 状态卡同一档），并给「重算临期」按钮一个 force 通道。

// 两个测试注入点。它们存在的原因很实际：链上有几站、谁排第一，这两件事在单测里
// 必须能固定下来，否则每条断言都得先登录一个真账号、再等一次真探测 —— 那样的
// 测试只能证明「这台机器今天登录过」，证明不了判据本身。
//
// 生产路径上恒为 nil，走真实读取；判定收在下面两个一行方法里，免得每个调用点
// 各写一次「有没有注入」。
var (
	aggTestExpiries func(force bool) map[string]lane.QuotaExpiry
	aggTestAccounts func(channelID string) int
)

const aggExpiryTTL = time.Minute

// quotaFailLogAt throttles the "read failed" diagnostic. It used to fire for
// every wired channel on every aggregate recomputation — ~760k identical
// DEBUG lines and 90-134 MB per day — under the always-on "app" category, so
// no user switch could silence it.
var (
	quotaFailLogMu sync.Mutex
	quotaFailLogAt = map[string]time.Time{}
)

// aggChannelExpiry reads the same per-account quota cache as the account panel.
// A failed read stays distinct from a confirmed empty balance; both are excluded
// from the aggregate chain until a usable quota is known.
func (s *Server) aggChannelExpiry(channelID string) lane.QuotaExpiry {
	if channelID == "opencode" || channelID == lane.ChannelKilo || s.aggregateProvider(channelID) != nil {
		// These routes expose no credit-expiry API. Keep them as fallback routes;
		// Never here means no expiry was supplied, not unlimited/free usage.
		return lane.QuotaExpiry{Never: true}
	}
	if !channels.IsWired(channelID) {
		return lane.QuotaExpiry{}
	}
	q := s.chanReadQuota(channelID)
	switch {
	case q.Known:
		return lane.QuotaExpiry{Known: true, ExpiryMS: q.ExpiryMS}
	case q.Never:
		return lane.QuotaExpiry{Never: true}
	case q.Failed:
		if s.logger != nil {
			key := channelID + "|" + q.Detail
			now := time.Now()
			quotaFailLogMu.Lock()
			if len(quotaFailLogAt) > 256 {
				quotaFailLogAt = map[string]time.Time{}
			}
			last := quotaFailLogAt[key]
			due := now.Sub(last) >= 5*time.Minute
			if due {
				quotaFailLogAt[key] = now
			}
			quotaFailLogMu.Unlock()
			if due {
				s.logCat(logx.CatProbe, "debug", "聚合：%s 这次读不到额度（%s）", channelID, q.Detail)
			}
		}
		return lane.QuotaExpiry{Failed: true}
	}
	return lane.QuotaExpiry{}
}

// aggExpiries 返回一张渠道 → 临期的表，一分钟内复用。
func (s *Server) aggExpiries(force bool) map[string]lane.QuotaExpiry {
	if aggTestExpiries != nil {
		return aggTestExpiries(force)
	}
	if force {
		s.forgetQuota("", "")
	}
	// Cache and coalesce at the account boundary. A second channel-level
	// cache would outlive login/logout and make the panel disagree with routing.
	next := map[string]lane.QuotaExpiry{}
	ids := map[string]bool{}
	for _, id := range channels.WiredIDs() {
		ids[id] = true
	}
	for id := range s.aggCatalogs() {
		ids[id] = true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			q := s.aggChannelExpiry(id)
			<-slots
			mu.Lock()
			next[id] = q
			mu.Unlock()
		}()
	}
	wg.Wait()
	return next
}

// aggCatalogs collects executable catalogs: Zen/Kilo/WorkBuddy from the lane,
// Qoder once from its managed provider, and enabled user providers.
func (s *Server) aggCatalogs() map[string][]aggregate.Model {
	out := map[string][]aggregate.Model{}
	now := time.Now()
	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.SystemOne {
			continue
		}
		id := m.Channel
		if id == "" {
			id = "opencode"
		}
		out[id] = append(out[id], aggregate.Model{ID: m.ID, Name: m.Name, Pricing: m.Pricing, Rate: m.Pricing.Effective(now)})
	}
	cfg := s.Store.Config()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled {
			continue
		}
		id := aggregateProviderChannel(p.ID)
		for _, mid := range aggregateProviderModels(p) {
			name := mid
			if meta, ok := p.ModelMeta[mid]; ok && meta.Name != "" {
				name = meta.Name
			}
			out[id] = append(out[id], aggregate.Model{ID: mid, Name: name, Pricing: p.ModelMeta[mid].Pricing, Rate: p.ModelMeta[mid].Pricing.Effective(now)})
		}
	}
	for id, models := range s.nativeModels() {
		for _, m := range models {
			out[id] = append(out[id], aggregate.Model{ID: m.ID, Name: m.Name, Pricing: m.Pricing, Rate: m.Pricing.Effective(now)})
		}
	}
	return out
}

func aggregateProviderModels(p *store.Provider) []string {
	return cleanModels(append(append([]string(nil), p.CatalogModels...), p.Models...))
}

// Keep user providers distinct from built-in account routes with the same id.
func aggregateProviderChannel(id string) string {
	if id == qoderProviderID {
		return "qoder"
	}
	if _, exists := channels.ByID(id); exists || id == lane.ChannelKilo || strings.HasPrefix(id, "provider:") {
		return "provider:" + id
	}
	return id
}

func (s *Server) aggregateProvider(channelID string) *store.Provider {
	cfg := s.Store.Config()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID != qoderProviderID && p.Enabled && aggregateProviderChannel(p.ID) == channelID {
			return p
		}
	}
	return nil
}

func (s *Server) aggWired(id string) bool {
	return channels.IsWired(id) || id == lane.ChannelKilo || s.aggregateProvider(id) != nil
}

func (s *Server) aggExtraChannelIDs() []string {
	var ids []string
	for id := range s.aggCatalogs() {
		if _, registered := channels.ByID(id); !registered {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// aggAccounts 是一个渠道当前「有几个可用账号」。它不是模型数：一个渠道一个账号
// 就能服务它全部的模型，而 0 个账号意味着无论目录里有什么都发不出去。
func (s *Server) aggAccounts(channelID string) int {
	if aggTestAccounts != nil {
		return aggTestAccounts(channelID)
	}
	cfg := s.Store.Config()
	if channelID == "opencode" && s.Lane.OpenCodeEnabledCount() == 0 {
		return 0
	}
	if channelID == "opencode" || channelID == lane.ChannelKilo {
		cat, _, _ := s.Lane.Snapshot()
		for _, m := range cat {
			if (channelID == "opencode" && m.Channel == "") || m.Channel == channelID {
				return 1
			}
		}
		return 0
	}
	if s.aggregateProvider(channelID) != nil {
		return 1
	}
	switch channelID {
	case "workbuddy":
		if !cfg.WorkBuddy.Enabled {
			return 0
		}
		// lane 的 WorkBuddy 账号没登录时，导入来的凭据照样能发请求 —— 两条来源
		// 都要算。只算 lane 那一份的结果是「导入成功、候选表里永远轮不到」，
		// 而那正是本项目最在意的那种静默缺陷。
		return len(lane.WBAccountsFromChannel(cfg.ChannelAccounts[channelID], cfg.WorkBuddy))
	case "qoder", "qodercn":
		if _, main := cfg.ChannelAccounts["qoder"]; main {
			return len(s.chanAccounts("qoder").Usable())
		}
		if _, alias := cfg.ChannelAccounts["qodercn"]; alias {
			return len(s.chanAccounts("qoder").Usable())
		}
		st := probeQoderCached(false)
		if st.LoggedIn {
			return 1
		}
		if len(cfg.ChannelAccounts[channelID].Usable()) > 0 {
			return 1
		}
		return 0
	}
	// 其余渠道的凭据在 ChannelAccounts 里，一家可能有多条（多账号）。
	//
	// 返回的是**可用账号数**：它有实际用途——面板与聚合面要说清「这一家有几个号」，
	// 而「一个号过期、另一个号还能用」这一家仍然在链上。
	//
	// ⚠️ 这里**不**要求 tier 非空：TierNone 的那几家（上游确实没有额度端点）
	// 照样算「有账号」——它们按永久额度参与轮换，而 0 账号意味着永远发不出去。
	return len(cfg.ChannelAccounts[channelID].Usable())
}

func aggregateChannelEnabled(cfg *store.Config, id string) bool {
	if enabled, set := cfg.AggregateChannels[id]; set && !enabled {
		return false
	}
	return id != "workbuddy" || cfg.WorkBuddy.Enabled
}

// buildAggregateState 组装 GET aggregate/state 的响应。
//
// 输出契约（前端按此读，改动要同步 web/js/pages/gateway.js）：
//
//	enabled            开关
//	order              渠道的烧掉顺序（渠道 id）
//	channels[]         每家：id/name/vendor/creditUnit/wired/accounts/note
//	virtual[]          每个规范名：key/name/candidates[]
//	candidates[]       每条：channel/realId/realName/price/expiryMs/never/eligible/wired/excluded
//	totals             家数 / 规范名数 / 候选数 / 三天内作废数
func (s *Server) buildAggregateState(force bool) map[string]any {
	cfg := s.Store.Config()
	expiries := s.aggExpiries(force)
	vetoed := map[string]bool{}
	for _, k := range cfg.AggregateExclude {
		vetoed[k] = true
	}

	virtuals := aggregate.BuildVirtualModels(s.aggCatalogs())
	manual := cfg.AggregateManualRank

	outVirtual := make([]map[string]any, 0, len(virtuals))
	totalCandidates, expiringSoon := 0, 0
	now := time.Now()
	for _, vm := range virtuals {
		rows := make([]aggregate.Candidate, 0, len(vm.Candidates))
		rowMaps := make([]map[string]any, 0, len(vm.Candidates))
		for _, c := range vm.Candidates {
			exp := expiries[c.Channel]
			state := aggregate.ExpiryUnusable
			switch {
			case exp.Known:
				if exp.ExpiryMS > now.UnixMilli() {
					state = aggregate.ExpiryKnown
				}
			case exp.Never:
				state = aggregate.ExpiryNever
			}
			accounts := s.aggAccounts(c.Channel)
			excluded := vetoed[c.Key()]
			channelEnabled := aggregateChannelEnabled(cfg, c.Channel)
			c.State = state
			c.Expiry = time.Time{}
			if exp.Known {
				c.Expiry = time.UnixMilli(exp.ExpiryMS)
			}
			c.Wired = s.aggWired(c.Channel)
			c.Excluded = excluded
			/* 可当选 = 参与轮换 + 有账号 + 临期读到了（含永久）+ 用户没关掉 +
			   **真的发得出去**。
			   最后一条曾经缺席：面板按前四条标「可当选」，而执行侧 aggregateChain
			   还要过 aggTargetOf —— qoder 系的托管供应商一旦被停用（登录掉线时
			   ensureQoderProvider 就是这么做的），候选仍在面板上亮着 eligible，
			   而链上永远轮不到它。两处判据必须同源，否则又是一处静默分叉。 */
			_, routable := s.aggTargetOf(c)
			eligible := channelEnabled && c.Wired && accounts > 0 && state != aggregate.ExpiryUnusable && !excluded && routable
			rows = append(rows, c)
			rowMaps = append(rowMaps, map[string]any{
				"channel": c.Channel, "realId": c.RealID, "realName": c.RealName,
				"price": priceJSON(c.Price), "expiryMs": exp.ExpiryMS, "never": exp.Never,
				"creditsRate": c.Pricing.Rate, "priceUnit": "credits",
				"requiresKey": s.Lane.ModelRequiresKey(c.Channel, c.RealID),
				"eligible":    eligible, "wired": c.Wired, "excluded": excluded, "via": c.Via,
				"channelEnabled": channelEnabled,
				// readFailed 让界面把「这一家这次读不到额度」与「这一家没有可用额度」
				// 分开说：前者是网络/进程问题，点「重算临期」可能就好了。
				"readFailed": exp.Failed,
			})
			if exp.Known && time.UnixMilli(exp.ExpiryMS).Sub(now) <= 3*24*time.Hour {
				expiringSoon++
			}
		}
		// 排序用同一套判据（手工位次 → 临期 → 倍率），但**不剔除**：面板要逐条
		// 列出每个渠道的真实 id 与为什么不行，剔除掉就无从核查了。
		sorted, _ := aggregate.Rank(rows, func(c aggregate.Candidate) (int, bool) {
			/* 位次来源分两层：这个虚拟模型自己排过的名单优先（它说的是"这个模型
			   的退路先后"），没排过时落到渠道级顺序（"这家整体上先烧"）。缺了第二层，
			   面板上拖动渠道顺序会与真实路由分叉：显示换了序，实际还按临期走。 */
			return aggregateManualPosition(manual, vm.Key, c.Channel)
		})
		ordered := make([]map[string]any, 0, len(sorted))
		for _, c := range sorted {
			for _, m := range rowMaps {
				if m["channel"] == c.Channel && m["realId"] == c.RealID {
					ordered = append(ordered, m)
					break
				}
			}
		}
		for _, m := range rowMaps {
			found := false
			for _, o := range ordered {
				if o["channel"] == m["channel"] && o["realId"] == m["realId"] {
					found = true
					break
				}
			}
			if !found {
				ordered = append(ordered, m) // 不可用的排在末尾，但仍列出
			}
		}
		totalCandidates += len(rows)
		outVirtual = append(outVirtual, map[string]any{
			"key": vm.Key, "name": vm.Name, "candidates": ordered,
		})
	}

	order := s.aggChannelOrder(manual, expiries)
	list := channels.Channels()
	channelRows := make([]map[string]any, 0, len(list))
	for _, c := range list {
		exp := expiries[c.ID]
		channelRows = append(channelRows, map[string]any{
			"id": c.ID, "name": c.Name, "vendor": c.Vendor,
			"protocol": c.Protocol, "authForm": c.AuthForm, "baseUrl": c.BaseURL,
			"wired": c.ExpiryWired, "creditUnit": c.CreditUnit,
			"dailyClaim": c.DailyClaim, "visionNote": c.VisionNote, "note": c.Note,
			"tier":       c.Tier,
			"readFailed": exp.Failed, "expiryMs": exp.ExpiryMS, "never": exp.Never,
			"accounts":         s.aggAccounts(c.ID),
			"aggregateEnabled": aggregateChannelEnabled(cfg, c.ID),
			/* routable 回答「这家现在真的发得出请求吗」。29 家里只有 WorkBuddy 与
			   Qoder 两条推理路径；其余凭据读得到额度、却发不出对话。不给这一格，
			   面板会把它们说成「参与轮换、有账号」，而候选表里永远轮不到 —— 而
			   用户没有办法从界面上看出这两件事的差别。 */
			"routable": s.channelRoutable(c.ID),
		})
	}
	for _, id := range s.aggExtraChannelIDs() {
		name, baseURL, note := id, "", "没有额度到期接口，作为后备参与；不代表免费或无限额度。"
		if id == lane.ChannelKilo {
			name = "Kilo 免费车道"
		}
		if p := s.aggregateProvider(id); p != nil {
			name, baseURL = p.Name, p.BaseURL
			if name == "" {
				name = p.ID
			}
		}
		channelRows = append(channelRows, map[string]any{
			"id": id, "name": name, "vendor": "", "baseUrl": baseURL, "note": note,
			"wired": true, "routable": s.channelRoutable(id), "tier": "none", "creditUnit": "无",
			"accounts": s.aggAccounts(id), "never": true, "expiryMs": int64(0), "readFailed": false,
			"aggregateEnabled": aggregateChannelEnabled(cfg, id),
		})
	}
	participating := 0
	for _, c := range channelRows {
		if c["aggregateEnabled"] == true && c["routable"] == true && c["wired"] == true {
			participating++
		}
	}

	return map[string]any{
		"enabled": cfg.AggregateEnabled,
		"order":   order,
		/* 把手工位次原样带出来：面板改一行动一次整表覆盖，前端手里没有旧表就
		   会把别的键（各虚拟模型的位次）一起清掉。带上它，前端才能做"只改这一格、
		   其余不动"的读改写，而不是每次保存都重排全世界。 */
		"manualRank": manual,
		// 保留暂时未出现在目录中的停用项，避免前端改另一行时把旧名单丢掉。
		"excluded": append([]string{}, cfg.AggregateExclude...),
		"channels": channelRows,
		"virtual":  outVirtual,
		"totals": map[string]any{
			"participatingChannels": participating,
			"wiredChannels":         len(channels.WiredIDs()) + len(s.aggExtraChannelIDs()),
			"virtualModels":         len(outVirtual),
			"candidates":            totalCandidates,
			"expiringSoon":          expiringSoon,
		},
	}
}

// ChannelOrderKey 是 AggregateManualRank 里"渠道级顺序"那一格的键。
//
// 为什么不复用某个虚拟模型的键：渠道的先后是一个**跨模型**的事实——它回答
// 「这 30 家谁先烧」，而虚拟模型的候选表只包含"这家真的上报了这个模型"的那几家。
// 用候选表反推渠道顺序，会让没有任何候选的渠道永远排不动（它们不在任何一张名单
// 里，位次写不进去、读不回来）。所以渠道级顺序单独占一格，键用一个不可能与真实
// 虚拟模型名冲突的前缀（虚拟模型键来自模型名，不含方括号与空格）。
const ChannelOrderKey = "[channels]"

// priceJSON 把解析不出的倍率（+Inf）写成 null：JSON 没有 Infinity 这个字面量，
// 编码它会直接失败；前端把 null 读作「没标倍率」，与 aggregate 的语义一致。
func priceJSON(v float64) any {
	if v != v || v > 1e300 || v < -1e300 {
		return nil
	}
	return v
}

// aggChannelOrder 排渠道级的烧掉顺序。
//
// 三条判据，与 aggregate.Rank 的口径一致（否则「渠道级顺序」与「链上顺序」会分叉，
// 面板显示的解释就对不上它自己的链）：
//
//  1. 手工排过的渠道严格压在没排过的之前（取它在**任一**虚拟模型里的最早位次）；
//  2. 临期升序：越早作废的先烧；
//  3. 都不满足时按渠道表的登记顺序（那是本项目的默认轮换序）。
func (s *Server) aggChannelOrder(manual map[string][]string, expiries map[string]lane.QuotaExpiry) []string {
	wired := append(channels.WiredIDs(), s.aggExtraChannelIDs()...)
	handRank := map[string]int{}
	/* 渠道级顺序优先：面板「渠道的烧掉顺序」那 30 行箭头写的就是这一格。
	   它必须胜过下面按虚拟模型演出来的「最早位次」——原因是一次实测就露了馅：
	   30 家渠道里只有 2 家真的在某个虚拟模型的候选表里，于是拖动第 3 家往后的
	   箭头时，前端算出的 manualRank 只有那 2 家，位次等于没写，重新读回来又被
	   临期排序拉回原位，用户看到的是「点了没反应」。渠道级的先后与「哪些渠道有
	   候选」是两件事，不该由后者决定能不能排。 */
	if list, ok := manual[ChannelOrderKey]; ok {
		for i, id := range list {
			handRank[id] = i
		}
	} else {
		for _, list := range manual {
			for i, id := range list {
				if cur, ok := handRank[id]; !ok || i < cur {
					handRank[id] = i
				}
			}
		}
	}
	base := map[string]int{}
	for i, id := range wired {
		base[id] = i
	}
	ids := append([]string{}, wired...)
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		ra, oka := handRank[a]
		rb, okb := handRank[b]
		if oka != okb {
			return oka
		}
		if oka && okb && ra != rb {
			return ra < rb
		}
		ea, eb := expiries[a], expiries[b]
		if ea.Known && eb.Known && ea.ExpiryMS != eb.ExpiryMS {
			return ea.ExpiryMS < eb.ExpiryMS
		}
		if ea.Known != eb.Known {
			return ea.Known
		}
		return base[a] < base[b]
	})
	return ids
}

// --- HTTP ---------------------------------------------------------------------

func (s *Server) adminAggregateState(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, 200, s.buildAggregateState(force))
}

// aggSettingsIn 是 POST aggregate/settings 的请求体。三个字段都可缺省：
// 前端每次只发它改的那一项，其余保持原值。
type aggSettingsIn struct {
	Enabled        *bool                `json:"enabled"`
	Excluded       *[]string            `json:"excluded"`
	Manual         *map[string][]string `json:"manualRank"`
	ChannelEnabled map[string]bool      `json:"channelEnabled"`
}

func (s *Server) adminAggregateSettings(w http.ResponseWriter, r *http.Request) {
	var in aggSettingsIn
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for id := range in.ChannelEnabled {
		if !s.aggWired(id) {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "渠道尚未接入聚合: " + id})
			return
		}
	}
	s.Store.Mutate(func(cfg *store.Config) {
		if len(in.ChannelEnabled) > 0 && cfg.AggregateChannels == nil {
			cfg.AggregateChannels = map[string]bool{}
		}
		for id, enabled := range in.ChannelEnabled {
			cfg.AggregateChannels[id] = enabled
			if id == "workbuddy" && enabled {
				// Enabling participation also prepares the existing logged-in
				// account for inference. Disabling only changes membership.
				cfg.WorkBuddy.Enabled = true
			}
		}
		if in.Enabled != nil {
			cfg.AggregateEnabled = *in.Enabled
		}
		if in.Excluded != nil {
			// 全量覆盖：调用方每次写回整张名单，两处各持半张表才会出现
			// 「关掉一条、另一条自己回来了」。
			list := make([]string, 0, len(*in.Excluded))
			for _, k := range *in.Excluded {
				if k != "" {
					list = append(list, k)
				}
			}
			cfg.AggregateExclude = list
		}
		if in.Manual != nil {
			next := map[string][]string{}
			for key, ids := range *in.Manual {
				if key == "" || len(ids) == 0 {
					continue
				}
				next[key] = append([]string{}, ids...)
			}
			cfg.AggregateManualRank = next
		}
	})
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if in.ChannelEnabled["workbuddy"] {
		cfg := s.Store.Config()
		s.Lane.SetWorkBuddyChannels(cfg.ChannelAccounts["workbuddy"], cfg.WorkBuddy)
		s.Lane.RefreshWorkBuddyCatalog()
		go s.Lane.RefreshWorkBuddyModels(context.Background())
	}
	if s.logger != nil && in.Enabled != nil {
		s.logger.Infof("聚合路由 = %v", *in.Enabled)
	}
	// 候选停用、顺序和聚合开关都会改变目录或能力，保存后立即刷新并同步静态配置。
	if in.Enabled != nil || in.Excluded != nil || in.Manual != nil || len(in.ChannelEnabled) > 0 {
		if s.modelsSync != nil {
			s.modelsSync()
		}
		if s.registry != nil {
			if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
				s.logger.Infof("聚合设置变化，已重新注入 %d 个已开启 Agent 的配置", n)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": s.buildAggregateState(true)})
}

// adminAggregateRecheck 丢掉额度缓存并重读一次：面板上的「重算临期」就是它。
// 返回重算了多少条候选（前端把那句文案写进 toast）。
func (s *Server) adminAggregateRecheck(w http.ResponseWriter, r *http.Request) {
	st := s.buildAggregateState(true)
	n := 0
	if totals, ok := st["totals"].(map[string]any); ok {
		if v, ok := totals["candidates"].(int); ok {
			n = v
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "recomputed": n, "state": st})
}

// aggErrText 只在日志里用：把「读不到」与「读到了但不可用」分开写，
// 免得排查时把一次网络失败当成「这家没额度」。
func aggErrText(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "额度端点超时"
	}
	return err.Error()
}

// A model-specific order replaces the channel order as a whole, including
// candidates the user has not ranked. The panel and execution share this rule.
func aggregateManualPosition(manual map[string][]string, modelKey, channel string) (int, bool) {
	list := manual[modelKey]
	if len(list) == 0 {
		// 兼容归一规则升级前保存的规范键，候选排除键仍保留真实 channel|id。
		keys := make([]string, 0, len(manual))
		for key := range manual {
			if key != ChannelOrderKey && aggregate.NormalizeModelKey(key) == modelKey {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			if len(manual[key]) > 0 {
				list = manual[key]
				break
			}
		}
	}
	if len(list) == 0 {
		list = manual[ChannelOrderKey]
	}
	for i, id := range list {
		if id == channel {
			return i, true
		}
	}
	return 0, false
}
