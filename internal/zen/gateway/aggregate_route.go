package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"zenui/internal/zen/aggregate"
	"zenui/internal/zen/lane"
	"zenui/internal/zen/logx"
	"zenui/internal/zen/relay"
	"zenui/internal/zen/store"
)

// 聚合的**执行**侧：把一条规范名换成一串真实去处，逐个试。
//
// 与聚合面板共用同一份决策（aggregate.Rank），这一点是刻意的：面板上写「第 1 站
// 是这家、第 2 站是那家」，真实请求就必须按同一顺序走。两处各算一遍必然漂移，
// 而症状是静默的 —— 用户按面板判断「先烧哪家」，实际烧掉了另一家。
//
// # 模型 id 的三种形态
//
//	aggregate/auto          自动挑：在全部候选里按「临期先烧、同档挑便宜的」
//	aggregate/<规范键>       指定虚拟模型（例 aggregate/deepseek-v4-1-flash）
//	其余                     不经过本层，原样走各渠道自己的 id
//
// 认不出的规范键一律当「没有这一条」，而不是回落到 auto：回落到 auto 会让一个
// 打错的名字静默变成一个能用的请求，用户以为自己在用 A 模型，实际在用别的。

const aggregatePrefix = "aggregate/"

// aggTarget 是一条候选落到「真实怎么发」上：要么走免费车道的某个模型 id，
// 要么走某个自定义供应商的某个模型 id。
type aggTarget struct {
	channel string
	realID  string
	// laneModel 非空 = 走 lane（免费车道 / WorkBuddy 账号渠道）
	laneModel string
	// provider 非空 = 走 relay（qoder 系托管供应商）
	provider *store.Provider
	model    string
}

// servedID 是回给客户端的模型名：供应商目标带命名空间，与 /v1/models 一致。
func (t aggTarget) servedID() string {
	if t.provider != nil {
		return t.provider.ID + "/" + t.model
	}
	if t.model != "" {
		return t.channel + "/" + t.model
	}
	return t.laneModel
}

// isAggregateModel 报告某个请求 id 是否走聚合层。
func isAggregateModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), aggregatePrefix)
}

// aggregateKey 从请求 id 里取规范键；"auto" 表示自动挑。
func aggregateKey(model string) string {
	return aggregate.NormalizeModelKey(strings.TrimPrefix(strings.TrimSpace(model), aggregatePrefix))
}

// aggregateChain 把规范键换成一串候选去处，顺序即面板上的备用链。
//
// 只有三件事能进这条链：参与轮换（编译器清单）、有账号、临期读到了（含永久）。
// 其余一律剔除 —— 让「读不到额度」的渠道进链等于拿未知去排序，失败是静默的。
func (s *Server) aggregateChain(model string) []aggTarget {
	cfg := s.Store.Config()
	if !cfg.AggregateEnabled {
		return nil
	}
	return s.aggregateChainFrom(model, cfg, s.aggExpiries(false), aggregate.BuildVirtualModels(s.aggCatalogs()))
}

func (s *Server) aggregateChainFrom(model string, cfg *store.Config, expiries map[string]lane.QuotaExpiry, virtuals []aggregate.VirtualModel) []aggTarget {
	if !cfg.AggregateEnabled {
		return nil
	}
	key := aggregateKey(model)
	if key == "" {
		return nil
	}
	vetoed := map[string]bool{}
	for _, k := range cfg.AggregateExclude {
		vetoed[k] = true
	}

	var picked []aggregate.Candidate
	for _, vm := range virtuals {
		if key != "auto" && vm.Key != key {
			continue
		}
		for _, c := range vm.Candidates {
			exp := expiries[c.Channel]
			if !aggregateChannelEnabled(cfg, c.Channel) || !s.aggWired(c.Channel) || vetoed[c.Key()] || s.aggAccounts(c.Channel) == 0 {
				continue
			}
			switch {
			case exp.Known:
				if exp.ExpiryMS <= time.Now().UnixMilli() {
					continue
				}
				c.State = aggregate.ExpiryKnown
				c.Expiry = time.UnixMilli(exp.ExpiryMS)
			case exp.Never:
				c.State = aggregate.ExpiryNever
			default:
				continue // 读不到临期 ⇒ 不进链
			}
			c.Wired = true
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	// auto 跨虚拟模型挑：排序判据仍是「临期先烧 → 倍率升序」，与面板同一套。
	ordered, err := aggregate.Rank(picked, func(c aggregate.Candidate) (int, bool) {
		modelKey, _, _ := aggregate.CanonicalKey(c.Channel, c.RealID, c.RealName)
		return aggregateManualPosition(cfg.AggregateManualRank, modelKey, c.Channel)
	})
	if err != nil {
		return nil
	}
	// Known-bad stations go to the BACK of the chain, not out of it. Without
	// this the expiry ranking keeps a free station that just answered
	// "FreeUsageLimitError" in first place while a funded own account sits
	// behind it, so every turn collides with the exhausted station first.
	// The chain is never shortened: an empty chain hides the real problem.
	if s.Lane != nil {
		_, av, _ := s.Lane.Snapshot()
		healthy := make([]aggregate.Candidate, 0, len(ordered))
		unhealthy := make([]aggregate.Candidate, 0, len(ordered))
		for _, c := range ordered {
			state := lane.FreshState(av[c.RealID])
			if state == lane.StateThrottled || state == lane.StateUnavailable ||
				state == lane.StateRegionBlock || s.Lane.Throttled(c.RealID) {
				unhealthy = append(unhealthy, c)
				continue
			}
			healthy = append(healthy, c)
		}
		ordered = append(healthy, unhealthy...)
	}
	out := make([]aggTarget, 0, len(ordered))
	for _, c := range ordered {
		if t, ok := s.aggTargetOf(c); ok {
			out = append(out, t)
		}
	}
	return out
}

// channelRoutable 报告这一家**现在真的发得出模型请求**吗。
//
// # 为什么单独有这么一条判据
//
// 29 家渠道都能登录、都能读额度，但真正把一次对话发出去的路径只有两条：
// WorkBuddy 走 lane 的账号快照，Qoder 走内置转发层的托管供应商。其余各家在本
// 项目里**没有推理实现** —— 凭据在 ChannelAccounts 里躺着，没有任何代码拿它去
// 发 chat completions。
//
// 不给这件事一个显式判据，面板就会一直说那 27 家「参与轮换、有账号」，而候选表
// 里永远轮不到它们 —— 这正是本项目最在意的那种静默缺陷（面板有、链上永远没有、
// 零报错）。判据只有一个来源：aggTargetOf 用它，面板也用它。
func (s *Server) channelRoutable(id string) bool {
	if s.nativeChannel(id) {
		return len(s.nativeModels()[id]) > 0
	}
	if s.aggregateProvider(id) != nil {
		return true
	}
	switch id {
	case "workbuddy", "opencode", lane.ChannelKilo:
		// 模型就在 lane 目录里（Channel 标记为 workbuddy），凭据由 lane 的快照提供。
		// 账号没登录时它确实发不出去，但那是 aggAccounts 报的事，不在这里编。
		return true
	case "qoder":
		p := s.providerByID(qoderProviderID)
		return p != nil && p.Enabled && len(aggregateProviderModels(p)) > 0
	}
	return false
}

// aggTargetOf 把一条候选换成执行目标。
func (s *Server) aggTargetOf(c aggregate.Candidate) (aggTarget, bool) {
	if !s.channelRoutable(c.Channel) {
		return aggTarget{}, false
	}
	if s.nativeChannel(c.Channel) {
		for _, model := range s.nativeModels()[c.Channel] {
			if model.ID == c.RealID {
				return aggTarget{channel: c.Channel, realID: c.RealID, model: c.RealID}, true
			}
		}
		return aggTarget{}, false
	}
	if p := s.aggregateProvider(c.Channel); p != nil {
		for _, model := range aggregateProviderModels(p) {
			if model == c.RealID {
				return aggTarget{channel: c.Channel, realID: c.RealID, provider: p, model: c.RealID}, true
			}
		}
		return aggTarget{}, false
	}
	switch c.Channel {
	case "workbuddy", "opencode", lane.ChannelKilo:
		if s.Lane.ModelRequiresKey(c.Channel, c.RealID) {
			return aggTarget{}, false
		}
		// WorkBuddy 的模型就在免费车道的目录里（Channel 标记为 workbuddy），
		// 所以它走 Lane，凭据由 lane 自己的账号快照提供。
		return aggTarget{channel: c.Channel, realID: c.RealID, laneModel: c.RealID}, true
	case "qoder":
		// QoderCN uses one managed provider; legacy aliases do not add routes.
		p := s.providerByID(qoderProviderID)
		if p == nil || !p.Enabled {
			return aggTarget{}, false
		}
		for _, m := range aggregateProviderModels(p) {
			if m == c.RealID {
				return aggTarget{channel: c.Channel, realID: c.RealID, provider: p, model: c.RealID}, true
			}
		}
		return aggTarget{}, false
	}
	return aggTarget{}, false
}

func (s *Server) providerByID(id string) *store.Provider {
	cfg := s.Store.Config()
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			return &cfg.Providers[i]
		}
	}
	return nil
}

// aggregateTurn 依次试链上的每一站，第一站接住就返回。
//
// 判据与 lane 自己的 failover 一致：**吐出任何内容就不再换**（半句话收尾比换一家
// 重开更接近用户的预期，且换家会让用户看到两份开头）。没吐出内容才走下一位。
//
// 返回的 Outcome.ServedModel 是真正接住的那一站，客户端据此知道自己用的是谁。
func (s *Server) aggregateTurn(ctx context.Context, req lane.Request, chain []aggTarget, emit func(lane.Chunk)) (lane.Outcome, *lane.UpstreamError) {
	var lastErr *lane.UpstreamError
	sawContent := false
	feed := func(c lane.Chunk) {
		switch c.Kind {
		case lane.ChunkTextDelta, lane.ChunkReasonDelta, lane.ChunkToolCallDelta, lane.ChunkBlockStart:
			sawContent = true
		}
		emit(c)
	}
	for i, t := range chain {
		if i > 0 && sawContent {
			break
		}
		var (
			outcome lane.Outcome
			uerr    *lane.UpstreamError
		)
		if s.nativeChannel(t.channel) {
			outcome, uerr = s.nativeTurn(ctx, req, t.channel, t.model, feed)
		} else if t.provider != nil {
			usage, finish, rerr := relay.Complete(ctx, relay.Turn{
				Provider: t.provider, Model: t.model, Messages: req.Messages,
				Tools: req.Tools, MaxTokens: req.MaxTokens, Agent: req.Agent,
			}, feed)
			s.recordRelay(ctx, t.provider, t.model, req.Agent, finish != "", usage, 0, finish, rerr)
			outcome = lane.Outcome{Usage: usage, Finish: finish, ServedModel: t.servedID()}
			if rerr != nil {
				uerr = &lane.UpstreamError{Code: rerr.Code, Message: rerr.Message, RetryAfter: rerr.RetryAfter}
			}
		} else {
			lreq := req
			lreq.Model = t.laneModel
			lreq.Channel = t.channel
			lreq.PreserveModel = true
			outcome, uerr = s.Lane.Complete(ctx, lreq, feed)
		}
		if uerr == nil {
			if i > 0 && s.logger != nil {
				s.logger.Infof("聚合：前 %d 站未接住，已由 %s/%s 接住", i, t.channel, t.realID)
			}
			return outcome, nil
		}
		lastErr = uerr
		// 用户主动取消不算「这一站不行」：继续试下一站会把一次取消变成一串请求。
		if uerr.Code == lane.CodeAborted || ctx.Err() != nil {
			return outcome, uerr
		}
		s.logCat(logx.CatRouting, "warn", "聚合候选 %s/%s 失败: %s", t.channel, t.realID, uerr.Message)
	}
	if lastErr == nil {
		lastErr = &lane.UpstreamError{Code: lane.CodeQuota, Message: "聚合链上没有可用的渠道：先登录账号，或等额度探测跑完"}
	}
	return lane.Outcome{}, lastErr
}

// runTurn 是一个回合的**唯一入口**：aggregate/ 名字走聚合链，其余原样走 lane。
//
// 所有协议（chat / responses / messages）的三条路径都从这里进，这样「聚合」不是
// 某一家的私有功能：Claude 客户端发 aggregate/deepseek-v4-1-flash 与 OpenAI 客户端
// 发同一个名字，走的必须是同一条链、同一套判据。
//
// ⚠️ 开关关着、或这条规范名没有候选时，**立刻**返回错误而不是「悄悄当成普通模型
// 名发给 lane」：后者会让 lane 去查一个不存在的 id，上游回一句 404，用户看到的
// 是「模型不存在」——而真实原因是聚合没开。
func (s *Server) runTurn(ctx context.Context, model string, req lane.Request, emit func(lane.Chunk)) (lane.Outcome, *lane.UpstreamError) {
	if !isAggregateModel(model) {
		return s.Lane.Complete(ctx, req, emit)
	}
	chain := s.aggregateChain(model)
	if len(chain) == 0 {
		// Exhausted 必须为 false：这一层没有候选是**配置/登录**问题，让上层的
		// 「免费车道耗尽 → 回落自定义供应商」接手，会把一个没开的聚合静默变成
		// 另一家上游的回答。用户以为在用聚合，实际在用别的东西。
		return lane.Outcome{}, &lane.UpstreamError{
			Code: lane.CodeQuota, Message: s.errNoAggregate(model),
		}
	}
	// 请求里的 Model 换成链上第 1 站之前不动它：aggregateTurn 逐站替换。
	req.Model = lane.BaseModelId(req.Model)
	return s.aggregateTurn(ctx, req, chain, emit)
}

// aggregateCandidates 给错误提示用：链上还剩哪几家能接（面板上也显示同一份）。
func (s *Server) aggregateCandidates(model string, n int) []string {
	chain := s.aggregateChain(model)
	out := make([]string, 0, n)
	for _, t := range chain {
		if len(out) >= n {
			break
		}
		out = append(out, t.servedID())
	}
	return out
}

// failoverSuggestions 是 429 错误里那句 suggestions 的来源：先给免费车道自己
// 算出来的候选，再补上**聚合规范名**。
//
// 为什么要把聚合名放进 suggestions：用户开着聚合时，最省事的一步不是换一个
// 具体模型 id，而是换 aggregate/auto —— 它自己会挑一家能接的。只给具体 id
// 等于让用户手工做一遍聚合本来要做的事。聚合关着时不列（那时它选了也没用）。
//
// 去重按顺序做：同一个 id 出现两次会让客户端的选择器多一条假选项。
func (s *Server) failoverSuggestions(model string, needs lane.Needs, base string, n int) []string {
	out := append([]string{}, s.Lane.Candidates(needs, base, n)...)
	if !s.Store.Config().AggregateEnabled {
		return out
	}
	add := func(id string) {
		if id == "" {
			return
		}
		for _, v := range out {
			if v == id {
				return
			}
		}
		out = append(out, id)
	}
	/* 请求本身就是聚合名时，别再建议 aggregate/*：那句错误已经说过「没有可用
	   候选」，再推荐同一个东西等于让客户端自己绕圈。 */
	if !isAggregateModel(model) {
		add(aggregatePrefix + "auto")
		for _, key := range s.aggregateReadyKeys(n) {
			add(aggregatePrefix + key)
		}
	}
	if len(out) > n+2 {
		out = out[:n+2]
	}
	return out
}

// aggregateReadyKeys 列出当前**真能接**的规范名（按字典序，稳定可复现）。
// 用 aggregateChain 逐条试是为了和真实路由用同一套判据：这里列出来的名字，
// 客户端拿去用必须真的能走通，否则 suggestions 就成了另一份假承诺。
func (s *Server) aggregateReadyKeys(n int) []string {
	if n <= 0 {
		return nil
	}
	keys := []string{}
	seen := map[string]bool{}
	for _, vm := range aggregate.BuildVirtualModels(s.aggCatalogs()) {
		if seen[vm.Key] {
			continue
		}
		if len(s.aggregateChain(aggregatePrefix+vm.Key)) == 0 {
			continue
		}
		seen[vm.Key] = true
		keys = append(keys, vm.Key)
	}
	sort.Strings(keys)
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// errNoAggregate 是「开关没开」与「这条规范名不存在」两种情形的统一说法：
// 客户端拿到的是一个可直接执行的指路，而不是一句 not found。
func (s *Server) errNoAggregate(model string) string {
	if !s.Store.Config().AggregateEnabled {
		return "聚合路由未开启：到「聚合」面打开「按临期顺序自动挑渠道」，或改用具体模型 id"
	}
	if aggregateKey(model) == "auto" {
		return "聚合路由当前没有可用的候选渠道：先登录 WorkBuddy / Qoder 账号，或在「聚合」面重算临期"
	}
	return "没有规范名 " + aggregateKey(model) + "：模型选择器里的 aggregate/ 条目就是全部可选项"
}

var errAggregateOff = errors.New("aggregate: 没有可用候选")
