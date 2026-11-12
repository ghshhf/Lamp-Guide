// Package core owns the process wiring of the zen-gate gateway: store, logger,
// free-lane client, HTTP gateway, capability tagger, subscription manager and
// the embedded Qoder hub, plus every background loop. It is deliberately free
// of desktop UI dependencies (window/tray), so the same code backs the Windows
// tray app (cmd/zen-gate) and the Android binding (mobile/): the host supplies
// the two UI touchpoints it still needs — a tray-status sink and a
// default-browser opener — through Options.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zenui/internal/zen/agents"
	"zenui/internal/zen/announce"
	"zenui/internal/zen/autotag"
	"zenui/internal/zen/chprov"
	"zenui/internal/zen/gateway"
	"zenui/internal/zen/lane"
	"zenui/internal/zen/logx"
	"zenui/internal/zen/notify"
	"zenui/internal/zen/qoderhub"
	"zenui/internal/zen/qoderhub/remote"
	"zenui/internal/zen/store"
	"zenui/internal/zen/subs"
	"zenui/internal/zen/update"
)

// Options configures a Core.
type Options struct {
	// Home overrides the data directory (ZEN_GATE_HOME). Empty keeps the
	// per-OS default. Android passes the app's private filesDir here.
	Home string
	// Port overrides the configured listen port when > 0.
	Port int
	// NoTray routes the logger to stdout (console/dev mode).
	NoTray bool
	// DisableDesktop crops the features that only make sense on a desktop
	// host: agent-config injection, the self-update check, and the sing-box
	// subscription apply loop. Notifications become the host's job.
	DisableDesktop bool
	// OnTrayStatus receives the tray tooltip text on every endpoint sync.
	// nil on hosts with no tray.
	OnTrayStatus func(string)
	// OpenExternal opens a URL in the host's default browser. nil drops it.
	OpenExternal func(string)
	// OnNotify surfaces user-facing toasts (model state change, new release).
	// Desktop supplies a closure that respects the window-hidden state; nil
	// on Android, where the foreground-service notification is the channel.
	OnNotify func(title, body string)
}

// Core is a running gateway and everything it owns.
type Core struct {
	Lane    *lane.Lane
	Store   *store.Store
	Gateway *gateway.Server
	Logger  *logx.Logger

	opts     Options
	ctx      context.Context
	cancel   context.CancelFunc
	subs     *subs.Manager
	tagger   *autotag.Tagger
	qoderHub *qoderhub.Hub
}

// Start wires and starts every subsystem. The returned Core is live; call Stop
// to shut it down. A listen failure is returned (nothing to stop in that case).
func Start(ctx context.Context, opts Options) (*Core, error) {
	if opts.Home != "" {
		// store.Open reads this override, so Android needs no store change.
		_ = os.Setenv("ZEN_GATE_HOME", opts.Home)
	}
	st, err := store.Open()
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	logger := logx.New(st.Home)
	logger.SetStdout(opts.NoTray)

	cfg := st.Config()
	if opts.Port > 0 {
		// Publish the override through Mutate and re-read, rather than writing
		// into the snapshot: everything below (and the gateway's own bind) reads
		// the port back through Config().
		st.Mutate(func(c *store.Config) { c.Port = opts.Port })
		cfg = st.Config()
	}
	// v5: providers saved before model selection existed hold full catalog
	// dumps — trim them to the recommended picks so the 模型 page shows only
	// what was actually chosen.
	if n := gateway.MigrateProviderSelections(st); n > 0 {
		logger.Infof("已按推荐列表精简 %d 个自定义供应商的模型（旧版保存了整个目录）", n)
	}
	if !opts.DisableDesktop {
		update.CleanupBackup() // remove the previous binary left by 一键更新
	}
	logger.Infof("zen-gate %s 启动 (port %d, proxy %s)", gateway.Version, cfg.Port, cfg.ProxyMode)

	cctx, cancel := context.WithCancel(ctx)
	c := &Core{Store: st, Logger: logger, opts: opts, ctx: cctx, cancel: cancel}

	ln := lane.NewLane()
	ln.SetDefaultMaxTokens(cfg.DefaultMaxTokens)
	ln.SetExposeRegion(cfg.ExposeRegion)
	ln.SetFailover(cfg.FailoverEnabled, cfg.FailoverMax)
	// Smart routing: master switch + failover ordering strategy + the runtime
	// first-token sample ring that the latency strategy ranks by.
	ln.SetSmartRouting(cfg.SmartRouting)
	ln.SetStrategy(cfg.RoutingStrategy)
	ln.SetTTFTSource(func(model string) int64 {
		avg, _ := st.TTFTStats(model)
		return avg
	})
	// Persisted capability tags (AI tagger / live probes / the public capacity
	// reference) are replayed into the catalog inside gateway.New, which also
	// guards the CapsOnly rows and retries effort-suffixed ids — no boot loop
	// here.
	ln.LoadThrottleNotes(quotaNotesFromStore(st.SnapshotQuota()))
	ln.SetThrottleUpdate(func(model string, note lane.ThrottleNote) {
		qn := store.QuotaNote{ThrottledAt: note.ThrottledAt, CooldownUntil: note.CooldownUntil, LastOK: note.LastOK}
		for _, e := range note.Episodes {
			qn.Episodes = append(qn.Episodes, store.QuotaEpisode{Start: e.Start, End: e.End})
		}
		st.SetQuotaNote(model, qn)
	})
	lane.SetProxy(cfg.ProxyMode, cfg.ProxyURL)
	// WorkBuddy 账号渠道：config 是账号的落盘处，lane 持快照；上游轮换
	// refresh token，lane 通过 persist 回调把新 token 对写回 config。
	ln.SetWorkBuddyChannels(cfg.ChannelAccounts["workbuddy"], cfg.WorkBuddy)
	ln.SetWorkBuddyPersist(func(a lane.WBAccount) {
		st.Mutate(func(c *store.Config) { persistWorkBuddyAccount(c, a) })
		// Every refresh rotates the refresh token. A swallowed write error
		// leaves the disk holding the consumed pair → silent logout next boot.
		// Retry briefly, then make the failure loud instead of invisible.
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = st.Save(); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		logger.Warnf("WorkBuddy 凭据写盘失败（新 token 仅在内存中，重启会掉登录）: %v", err)
	})
	ln.SetWorkBuddyLoginPersist(func(a lane.WBAccount) {
		st.Mutate(func(c *store.Config) { persistWorkBuddyLogin(c, a) })
		_ = st.Save()
		current := st.Config()
		ln.SetWorkBuddyChannels(current.ChannelAccounts["workbuddy"], current.WorkBuddy)
	})
	c.Lane = ln

	// 订阅轮询:sidecar manager wires into the gateway, the lane rotator and
	// every shutdown path below. On Android the feature is cropped: the manager
	// is built (so the dashboard card renders) but Apply never runs, because a
	// sing-box sidecar has no place on the device.
	mgr := subs.NewManager(st, logger.Infof)
	c.subs = mgr
	if !opts.DisableDesktop && cfg.SubsEnabled && len(cfg.Subscriptions) > 0 {
		go func() {
			actx, acancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer acancel()
			if err := mgr.Apply(actx); err != nil {
				logger.Errorf("订阅轮询启动失败: %v", err)
				return
			}
			if cfg.ProxyMode == "rotate" {
				lane.SetRotator(mgr)
				logger.Infof("订阅轮询已启动(%d 个健康节点)", mgr.Healthy())
			}
		}()
	}
	// Probe first-token samples feed the persisted per-model average.
	ln.OnProbeResult = func(r lane.ProbeResult) {
		st.AddTTFTSample(r.Model, r.TTFTMs)
	}
	// Desktop notifications stay gated by the settings switch; Android crops
	// the channel entirely (the foreground-service notification is the UI).
	if !opts.DisableDesktop {
		notify.SetEnabled(cfg.Notifications)
	}
	gw := gateway.New(ln, st)
	gw.SetSubs(mgr)
	gw.SetLogger(logger)
	gw.SetOpenExternal(opts.OpenExternal)
	gw.ApplyLogSettings(cfg)
	c.Gateway = gw

	// Agent-config injection is a desktop feature: adapters write into the
	// host's ~/.config files for editors/CLIs that do not exist on Android.
	var reg *agents.Registry
	if !opts.DisableDesktop {
		reg = agents.NewRegistry(st)
		gw.SetAgents(reg)
	}

	// AI capability tagger: classifies provider model ids (and fills the lane
	// catalog's unverified audio/file fields) using the free lane itself.
	tagger := autotag.New(ln, st, logger.Infof)
	tagger.SetEnabled(cfg.AutoTagEnabled)
	tagger.Start(cctx)
	gw.SetTagger(tagger)
	c.tagger = tagger
	enqueueUntagged := func() {
		ids := []string{}
		for _, p := range st.Config().Providers {
			for _, m := range p.Models {
				if _, ok := st.ModelTagOf(m); !ok {
					ids = append(ids, m)
				}
			}
		}
		// Lane models are deliberately NOT tagged: their ids are private, the
		// LLM cannot know them, and asking only burns thinking budget — the
		// curated table plus live probes own the lane's verdicts.
		if len(ids) > 0 {
			tagger.Enqueue(ids...)
		}
	}
	enqueueUntagged()
	gw.SetAutostartState(agents.AutostartEnabled)

	stateText := func(s string) string {
		return map[string]string{lane.StateAvailable: "可用", lane.StateUnknown: "未知",
			lane.StateThrottled: "暂时限流·可重试", lane.StateRegionBlock: "地区受限",
			lane.StateUnavailable: "不可用"}[s]
	}
	notifyUser := func(title, body string) {
		if opts.OnNotify != nil {
			opts.OnNotify(title, body)
		}
	}
	ln.OnProbeEdge = func(model, from, to string) {
		if model == "" {
			logger.Warnf("免费车道整轮 429 限额，探测进入指数退避")
			notifyUser("免费车道已达限额", "本轮探测全部 429，稍后自动恢复")
			return
		}
		logger.Infof("模型状态变化: %s %s → %s", model, stateText(from), stateText(to))
		notifyUser("模型状态变化", fmt.Sprintf("%s: %s → %s", model, stateText(from), stateText(to)))
	}

	syncEndpoints := func() {
		if reg != nil {
			reg.SetEndpoints(gw.BaseURL(), gw.InjectableModels())
			// Issue #26: the in-memory refresh alone never touched agent config
			// files, so a model joining the catalog stayed invisible to every
			// agent until the user manually toggled visibility. ResyncEnabled
			// writes only when the roster/capability fingerprint changed since
			// its last successful pass (each needless rewrite stacks another
			// timestamped backup — backups are never pruned), so firing this on
			// every probe round is cheap. It also injects once at boot, which
			// propagates rule-shape fixes to agents enabled under an earlier build.
			if n := reg.ResyncEnabled(); n > 0 {
				logger.Infof("模型清单更新，已重新注入 %d 个已开启 Agent 的配置", n)
			}
		}
		if opts.OnTrayStatus != nil {
			opts.OnTrayStatus(trayStatus(st, ln))
		}
	}
	ln.OnChange = syncEndpoints
	gw.SetModelsSync(syncEndpoints)
	// Read the interval from the live config on every scheduler tick: saving
	// 探测间隔 in the dashboard must take effect without restarting the app.
	ln.StartLoops(cctx, func() time.Duration {
		mins := st.Config().ProbeIntervalMinutes
		if mins < 1 {
			mins = 15
		}
		return time.Duration(mins) * time.Minute
	})
	// Daily rewards use verified claim endpoints; background chat traffic is not
	// evidence of a reward and must not be reported as a successful check-in.
	gw.StartOpenRefLoop(cctx)
	syncEndpoints()
	gw.StartCheckins(cctx)

	if err := gw.Start(); err != nil {
		logger.Errorf("listen on port %d: %v", cfg.Port, err)
		c.Stop()
		return nil, fmt.Errorf("listen on port %d: %w", cfg.Port, err)
	}
	dashURL := strings.TrimSuffix(gw.BaseURL(), "/v1")
	logger.Infof("dashboard ready at %s", dashURL)

	// Embedded QoderCN gateway (Qoder 账号渠道): rides this process, serves
	// 127.0.0.1:8095 for the managed provider. A busy port is not fatal — an
	// external qodercn-gateway the user runs themselves keeps serving.
	//
	// The credential file is zen-gate's own, not the IDE's cache: the 渠道面's
	// 「登录」 writes it through chprov. Both sides take the path from the same
	// place (store.Home), so a login is immediately what the model calls use.
	qoderAuthFile := filepath.Join(st.Home, "qoder-credential.json")
	chprov.QoderSidecarAuthFile = qoderAuthFile
	qoderHub, qoderHubErr := qoderhub.Start(qoderAuthFile, func() []remote.Credential {
		return chprov.QoderCredentials(st.Config().ChannelAccounts)
	})
	if qoderHubErr != nil {
		logger.Infof("内置 Qoder 网关未启动: %v", qoderHubErr)
	} else {
		c.qoderHub = qoderHub
		logger.Infof("内置 Qoder 网关就绪 (127.0.0.1:8095)")
	}
	// The managed Qoder provider lists through the local hub, so discover its
	// complete catalog only after that listener has been started.
	go func() {
		catalogCtx, cancel := context.WithTimeout(cctx, time.Minute)
		defer cancel()
		if _, err := gw.RefreshProviderCatalogs(catalogCtx); err != nil {
			logger.Warnf("完整模型目录保存失败: %v", err)
		}
	}()
	if lan := gw.LANBaseURL(); lan != "" {
		logger.Infof("局域网 API 可用: %s", lan)
	}

	// periodic stats flush
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				_ = st.FlushStats()
				st.FlushPerf()
			}
		}
	}()

	// update check loop — desktop only: Android installs through the store and
	// has no in-place binary swap.
	if !opts.DisableDesktop {
		checkUpdates := func() (bool, string) {
			cfg := st.Config()
			feed := update.ResolveFeed(cfg.UpdateFeed)
			if feed == "" {
				return false, ""
			}
			has, ver, url, _, err := update.Check(feed, update.HTTP{Timeout: 15 * time.Second, Client: lane.Client()})
			if err != nil {
				logger.Warnf("update check failed: %v", err)
				return false, ""
			}
			gw.SetUpdateState(has, ver, url)
			if has && cfg.LastVersion != ver {
				logger.Infof("发现新版本 %s (当前 %s)", ver, gateway.Version)
				notifyUser("Zen Gate 有新版本 "+ver, "当前 "+gateway.Version+" · 打开管理页查看下载链接")
				st.Mutate(func(c *store.Config) { c.LastVersion = ver })
				_ = st.Save()
				syncEndpoints()
			}
			return has, ver
		}
		gw.SetUpdateCheck(checkUpdates)
		go func() {
			select {
			case <-cctx.Done():
				return
			case <-time.After(45 * time.Second):
			}
			checkUpdates()
			t := time.NewTicker(6 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-cctx.Done():
					return
				case <-t.C:
					checkUpdates()
				}
			}
		}()
	}

	// announcement feed: pull the author's notices at boot and every 6h.
	// An empty feed URL disables the pull entirely.
	pullAnnouncements := func() {
		feed := strings.TrimSpace(st.Config().AnnouncementFeed)
		if feed == "" {
			gw.SetAnnouncements(nil)
			return
		}
		items, err := announce.Fetch(cctx, feed, lane.Client())
		if err != nil {
			logger.Warnf("公告拉取失败: %v", err)
			return
		}
		gw.SetAnnouncements(items)
		if len(items) > 0 {
			logger.Infof("公告已更新: %d 条生效中", len(items))
		}
	}
	gw.SetAnnouncementPull(pullAnnouncements)
	go func() {
		select {
		case <-cctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		pullAnnouncements()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				pullAnnouncements()
			}
		}
	}()

	// anonymous usage heartbeat: fires only when a stats server is configured.
	// Payload is {installId, version, country} — nothing else leaves the machine.
	go func() {
		ping := func() {
			url := strings.TrimSpace(st.Config().StatsServerURL)
			if url == "" {
				return
			}
			_, _, eg := ln.Snapshot()
			body, _ := json.Marshal(map[string]any{
				"installId": st.Config().InstallID,
				"version":   gateway.Version,
				"country":   eg.Country,
			})
			req, err := http.NewRequest(http.MethodPost, strings.TrimRight(url, "/")+"/api/ping", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("content-type", "application/json")
			client := *lane.Client()
			client.Timeout = 10 * time.Second
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
		select {
		case <-cctx.Done():
			return
		case <-time.After(90 * time.Second): // let the first egress detect finish
		}
		ping()
		pt := time.NewTicker(6 * time.Hour)
		defer pt.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-pt.C:
				ping()
			}
		}
	}()

	return c, nil
}

// Stop tears everything down in the reverse order of Start.
func (c *Core) Stop() {
	if c == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.Gateway != nil {
		c.Gateway.Stop()
	}
	if c.subs != nil {
		c.subs.Stop()
	}
	if c.qoderHub != nil {
		c.qoderHub.Stop()
	}
	if c.Store != nil {
		_ = c.Store.FlushStats()
	}
	if c.Logger != nil {
		c.Logger.Close()
	}
}

// DashboardURL is the loopback admin page address (no /v1 suffix).
func (c *Core) DashboardURL() string {
	if c.Gateway == nil {
		return ""
	}
	return strings.TrimSuffix(c.Gateway.BaseURL(), "/v1")
}

// BaseURL is the OpenAI-compatible endpoint (…/v1).
func (c *Core) BaseURL() string {
	if c.Gateway == nil {
		return ""
	}
	return c.Gateway.BaseURL()
}

// quotaNotesFromStore converts persisted quota notes back into the lane's
// shape for boot-time seeding.
func quotaNotesFromStore(in map[string]store.QuotaNote) map[string]lane.ThrottleNote {
	out := map[string]lane.ThrottleNote{}
	for m, n := range in {
		note := lane.ThrottleNote{ThrottledAt: n.ThrottledAt, CooldownUntil: n.CooldownUntil, LastOK: n.LastOK}
		for _, e := range n.Episodes {
			note.Episodes = append(note.Episodes, lane.ThrottleEpisode{Start: e.Start, End: e.End})
		}
		out[m] = note
	}
	return out
}

// trayStatus is the tooltip / status-bar summary: today's output tokens plus
// how many models are currently servable.
func trayStatus(st *store.Store, ln *lane.Lane) string {
	days, _ := st.SnapshotStats()
	today := time.Now().Format("2006-01-02")
	tok := 0
	if d, ok := days[today]; ok {
		tok = d.Output
	}
	return fmt.Sprintf("运行中 · 今日输出 %d tok · %d 模型可用", tok, len(ln.ServableModels()))
}
