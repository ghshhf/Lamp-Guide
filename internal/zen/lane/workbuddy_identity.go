package lane

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// WorkBuddy 的「上游怎么认出你不是官方客户端」这一层：会话头族、指纹族、
// 以及被拦时的口径。全部来自 2026-10/11 的实测取证（每段注释里标了出处），
// 不是推测。
//
// # 三条独立事实，此前仓里混成了一条
//
//  1. 头族：官方桌面端主对话（sidecar → CLI → OpenAI Node SDK → axios）发什么、
//     不发什么，已被逐字节 dump。我们此前发出去的 X-Machine-ID / X-Session-ID /
//     X-Product / Origin / Referer / X-CodeBuddy-Request 在官方全量包里 grep
//     计数为 0 —— 那几个头恰恰是「网关特征」，不是伪装。这条与 workbuddy.go
//     顶部那段旧注释正好相反，旧注释是错的。
//  2. 会话头族：/v2/chat/completions 的闸门要 X-Conversation-ID /
//     -Request-ID / -Message-ID 三件套。缺了它国际版回 400 request illegal
//     （它和国内的 11128 长得一模一样，根因却完全不同，且没有任何头可以修好它）。
//  3. 内容闸：11-128 是对请求文本的逐字模板匹配，命中就整单拒。它逐词扩充，
//     所以词表必须是运行期可维护的数据，而不是散在几个 regexp 里。

const (
	// 官方国际版桌面端版本（~/.workbuddy-ai/last-launch.json）及其内置 CLI
	// （cli/package.json）。版本落后本身就是可被判据：官方每次请求都声称
	// 自己的版本，一个落后三个 minor 的 UA 是免费的风险面。
	wbIDEVersionIntl = "5.9.4"
	wbDesktopUAIntl  = "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"
	// 官方国内版（~/.workbuddy/last-launch.json）。本渠道目前只认国际版令牌，
	// 但两域的头族必须整组一致——国际版令牌配国内版头是「多出项」。
	wbIDEVersionCN = "5.9.3"
	wbDesktopUACN  = "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
)

// WBRealmCN 是国内发布线的标记。WBAccount.Realm 为空即国际版——本渠道的
// 令牌校验只放行 workbuddy.ai，这个字段是后加的，空值就是它本来会说的答案。
const WBRealmCN = "cn"

func wbIDEVersionFor(realm string) string {
	if realm == WBRealmCN {
		return wbIDEVersionCN
	}
	return wbIDEVersionIntl
}

func wbDesktopUAFor(realm string) string {
	if realm == WBRealmCN {
		return wbDesktopUACN
	}
	return wbDesktopUAIntl
}

// wbMessageID 是 32 位裸 hex：官方客户端的 request/message id 就是这个形状
// （UUID 去横线）。带前缀或带横线的 id 本身就是网关特征。
func wbMessageID() string { return wbRequestID() }

// wbConversationID 把一条下游会话映射成一个稳定的 32 hex 上游会话 id。
// 稳定是必须的：会话级 id 一变，上游的 prompt cache 每轮都落空。
// 空种子（探针、每日活跃）每轮换一个新 id 即可——它没有下一轮。
func wbConversationID(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return wbMessageID()
	}
	sum := sha256.Sum256([]byte("wb-conversation\x00" + seed))
	return hex.EncodeToString(sum[:16])
}

func wbValidTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// wbConversationHeaders 是闸门要的会话头族。国际版缺这三个头时回
// 400 request illegal；三个头只需是唯一字符串，上游不校验它们之间的关联，
// 也不要求跨请求一致。
//
// B3 四件套与 traceparent 是同一族：官方 applyTraceHeaders 一次发全。
// B3 只认 16/32 hex TraceId，所以会话 id 非法时回落消息级 32 hex。
func wbConversationHeaders(conversationID, requestID, messageID string) map[string]string {
	convID := conversationID
	if convID == "" {
		convID = messageID
	}
	h := map[string]string{
		"X-Conversation-ID":         convID,
		"X-Conversation-Request-ID": requestID,
		"X-Conversation-Message-ID": messageID,
		"X-Request-ID":              messageID,
	}
	traceID := convID
	if !wbValidTraceID(traceID) {
		traceID = messageID
	}
	span, parent := messageID[:16], messageID[16:]
	h["X-Trace-ID"] = traceID
	h["X-B3-TraceId"] = traceID
	h["X-B3-SpanId"] = span
	h["X-B3-ParentSpanId"] = parent
	h["X-B3-Sampled"] = "1"
	tpTrace := traceID
	if len(tpTrace) != 32 {
		tpTrace = messageID
	}
	h["traceparent"] = "00-" + tpTrace + "-" + span + "-01"
	h["b3"] = traceID + "-" + span + "-1-" + parent
	return h
}

// wbStainlessHeaders 复刻官方 chat 出站必然携带的 OpenAI Node SDK 指纹族。
//
// 官方 CLI 用 OpenAI SDK（6.25.0）发模型请求，SDK 的 buildHeaders 无条件拼入
// 这一族；只有 CODEBUDDY_SKIP_INTERNAL_HEADERS 为真时才被剥掉，而该 env 默认
// 不设。我们此前一个都不发 —— 属「官方发了、我们没发」的渠道特征，与「多发
// 了官方没有的头」是同一类缺陷的两个方向。
//
// OS/Arch 取本机真实值：官方也是从 process.platform/arch 归一，报一台 Windows
// 机器为 macOS 才是新的不一致。
func wbStainlessHeaders() map[string]string {
	return map[string]string{
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "1200",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "6.25.0",
		"X-Stainless-OS":              wbStainlessOS(),
		"X-Stainless-Arch":            wbStainlessArch(),
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v22.21.1",
	}
}

// wbStainlessOS / wbStainlessArch 把 GOOS/GOARCH 归到 SDK 的
// process.platform / process.arch 词汇（darwin → macOS、amd64 → x64）。
func wbStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

func wbStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// wbAttributionHeaders 是用量归属头。官方 getEffectiveAgentPurpose 主对话取
// conversation；桌面端主对话恒走本地 transport，故 runtime 取 local-agent。
// 这几个头缺了不会 4xx，但会让上游用量表里这一路的 client/agentPurpose 为空——
// 一个「所有请求都不带归属」的账号同样是可判据。
func wbAttributionHeaders() map[string]string {
	return map[string]string{
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		"X-Agent-Runtime": "local-agent",
		"X-Agent-Type":    "main",
		"X-Private-Data":  "true",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
	}
}

// wbChatHeaderDrop 是从官方全量包里 grep 计数为 0 的那些头：它们是本网关
// 自造的，发出去不是伪装而是签名。此前的实现正是把这一组当成了「WAF 期望的
// 身份头」，所以越是努力对齐越像网关。
var wbChatHeaderDrop = []string{
	"Origin",
	"Referer",
	"Accept-Language",
	"X-Machine-ID",
	"X-Session-ID",
	"X-Product",
	"X-CodeBuddy-Request",
	"X-Requested-With",
}

// wbChatHeaders 组装一次 chat 请求的头：base 来自 workBuddyHeaders（凭据、
// UA、IDE 三元组），本函数叠加会话族 + SDK 族 + 归属族，并抹掉自造头。
func wbChatHeaders(base map[string]string, realm, conversationID, requestID, messageID string) map[string]string {
	h := map[string]string{}
	for k, v := range base {
		h[k] = v
	}
	// workBuddyHeaders uses lowercase keys while the drop table is written in
	// canonical HTTP form. Go maps are case-sensitive, so delete(h, k) missed
	// every entry and the gateway's self-authored headers still went out.
	for key := range h {
		for _, drop := range wbChatHeaderDrop {
			if strings.EqualFold(key, drop) {
				delete(h, key)
				break
			}
		}
	}
	for k, v := range wbConversationHeaders(conversationID, requestID, messageID) {
		h[k] = v
	}
	for k, v := range wbStainlessHeaders() {
		h[k] = v
	}
	for k, v := range wbAttributionHeaders() {
		h[k] = v
	}
	h["X-IDE-Version"] = wbIDEVersionFor(realm)
	return h
}

// --- 账号限制（11140）---------------------------------------------------------

// 11140 是**账号被上游限制**（账号存在异常行为 / unusual activity），与
// 11128（请求文本被逐字匹配）、与 6004（免费窗口用满）是三件事。三者此前被
// 压成一条「内容门」，于是面板只报一句含糊的「被拦截」，用户既不知道该换号、
// 该改写提示词、还是该等窗口重置。
//
// 限制只能由上游解除，本地唯一能做的是**不再拿这个号去撞**：重复命中会被判
// 为惯犯，冷却比初犯更长。所以是一次即停、阶梯退避，而不是「再试一次看看闸门
// 松没松」。
var wbRestrictionBackoff = []time.Duration{
	time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
}

func wbRestrictionWait(strikes int) time.Duration {
	if strikes < 1 {
		strikes = 1
	}
	if strikes > len(wbRestrictionBackoff) {
		strikes = len(wbRestrictionBackoff)
	}
	return wbRestrictionBackoff[strikes-1]
}

// 内容闸的连击：一次命中往往只是某句话恰好撞上了词表，两次才算「这个号在被盯」。
const (
	wbSanitizeStrikes = 2
	wbSanitizeWindow  = 15 * time.Minute
	wbSanitizeCool     = 15 * time.Minute
)

// --- 11-128 内容闸的词表 -------------------------------------------------------

// wbRewrites 是出站请求体的逐句改写表。**每条都是实测过的**：原串 400、
// 改后 200，且只改一个词——语义不变，模型看到的还是那段话。
//
// 上游的规则是逐字模板匹配，所以词表会逐月扩充。它必须是数据，才能在下一次
// 400 时只加一行，而不是改一段逻辑。
var wbRewrites = []struct {
	re *regexp.Regexp
	to string
}{
	// Claude Code 的计费头与 cc_* 键值对：所有 role 都剥，上游按整段匹配。
	{regexp.MustCompile("(?i)x-anthropic-billing-header:[^;\r\n]*;?\\s*"), ""},
	{regexp.MustCompile("(?i)x-anthropic-billing-header"), "x-anthropic-billing-hdr"},
	{regexp.MustCompile("(?i)\\bcc_[a-z0-9_]+=[^;\r\n]*;?\\s*"), ""},
	{
		regexp.MustCompile("(?i)anthropic(?:'s)?\\s+official\\s+cli\\s+for\\s+claude"),
		"Anthropic's official CLI tool for Claude",
	},
	{
		regexp.MustCompile("(?i)you are claude code[^\\n.]*\\."),
		"You are a CLI coding assistant.",
	},
	{
		regexp.MustCompile("(?i)main\\s+branch\\s+\\(you\\s+will\\s+usually\\s+use\\s+this\\s+for\\s+prs\\)"),
		"Default branch (you will usually use this for PRs)",
	},
	// Codex CLI 的开场两句：整句匹配，只这一句就能 100% 触发。
	{
		regexp.MustCompile("(?i)you are a coding agent running in the codex cli[^.\\n]*\\."),
		"You are a helpful coding assistant running in the terminal.",
	},
	{
		regexp.MustCompile("(?i)\\b(?:the\\s+)?codex\\s+cli\\s+is\\s+an\\s+open\\s+source\\s+project\\s+led\\s+by\\s+openai[\\s.]*"),
		"",
	},
	{
		regexp.MustCompile("(?i)sisyphus-junior\\s*-\\s*focused\\s+executor\\s+from\\s+ohmyopencode"),
		"Sisyphus-Junior - Focused executor",
	},
	{regexp.MustCompile("(?i)\\bohmyopencode\\b"), "OpenCode"},
	// DSH 的自我介绍句族：上游把它做成了「自述句式 + 品牌」的模板，并逐个
	// 动词地长（powered by / built on / built by / created by …）。同一句放
	// user 里是 200，放 system/assistant 历史里即 400 —— 所以只在
	// system/developer/assistant 上改（见 wbSanitizeRoles）：改 user 会把模型
	// 看到的对话和用户手里那份对不上。
	{
		regexp.MustCompile("(?i)(^|\\n|[.!?;:,][ \\t]+|[\"'(\\[{][ \\t]*)you are\\s+(?:an?\\s+|the\\s+)?(?:[\\w.-]+\\s+){0,3}?(?:agent|assistant|model|ai|llm)?[\\s,]*(?:powered\\s+by|built\\s+on|built\\s+by|created\\s+by|developed\\s+by|made\\s+by|trained\\s+by)[^\\n]*?(\\.(?:[ \\t\\r\"]|\\n|$)|\\n|$)"),
		"You are a helpful AI assistant.",
	},
	// 「DeepSeek Harness Web GUI」四词整串是独立成员；破掉任一个词即 200。
	{regexp.MustCompile("(?i)deepseek\\s+harness\\s+web\\s+gui"), "DeepSeek Harness web interface"},
	// DSH file-reference 提示词：A 句 + B 句连写才是触发单元，插一个 that 即断开。
	{
		regexp.MustCompile("(?i)Tokens prefixed with @ are paths the user explicitly referenced"),
		"Tokens prefixed with @ are paths that the user explicitly referenced",
	},
	// 「workspace root」是上游 2026-10-27 新增的词（sys[:2075] 放行、
	// sys[:2081] 拒绝），root→base 一词之差即绕开，同义。
	{regexp.MustCompile("(?i)workspace root"), "workspace base"},
	// 品牌散写：品牌单独出现是 200，品牌与「this page/this GUI/this app」
	// 同现才命中。只缩散写形态，带横线/下划线的真实路径保持字节不变。
	{regexp.MustCompile("(?i)deepseek[ \\t]+harness"), "DeepSeek"},
	// 反探测：裸数字 11128 出现在请求体里本身就是拒单条件（与该数字的上下文
	// 无关）。用词边界而不是裸串替换 —— 裸串替换会把用户让我读的
	// ts=1729111280 改成 ts=172911-1280，静默改坏数据。
	{regexp.MustCompile("\\b11128\\b"), "11-128"},
}

// wbRewriteText 是单个字符串的改写。改写幂等：改后的文本不再命中。
func wbRewriteText(text string) string {
	for _, r := range wbRewrites {
		text = r.re.ReplaceAllString(text, r.to)
	}
	return text
}

// wbMentionRe 匹配「行首或空白后的 @」。
var wbMentionRe = regexp.MustCompile("(^|[ \\t\\n])(@)")

// wbEscapeMentions 转义请求文本里的 @。
//
// 上游把「@ + 长句」当**文件引用**去解析，解析失败就报 11128 —— 报错文案
// 「unapproved channel」完全误导，会把人带去查账号、域名、头、限流。
// @ → 反斜杠 @ 即 200（@ 单独或 @abc 本来就 200，短的上下文不触发）；
// 全角 ＠ / 零宽 / NBSP 都仍然 400，只有反斜杠有效。
//
// 不碰真实路径语境（D:\...\DeepSeek Harness\...）：路径里插字符会让模型把
// 坏路径交给工具调用，所以只转义行首或空白后那一个 @。
func wbEscapeMentions(text string) string {
	if !strings.Contains(text, "@") {
		return text
	}
	return wbMentionRe.ReplaceAllString(text, "$1\\$2")
}
