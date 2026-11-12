package chprov

import (
	"regexp"
	"strings"
)

// scrubSecrets strips credential-shaped values out of text before it reaches
// an error, a log line, or the dashboard. Login / token / STS endpoints echo
// back access tokens, AK/SK pairs, session tickets and signed URLs; short()
// and statusText() used to splice those bodies verbatim into errors that
// gateway/channels.go forwards to the browser. Redacting at the single
// formatting choke point covers every caller instead of patching ~30 sites.
var (
	scrubBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{6,}`)
	scrubJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)
	scrubKey    = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|access[_-]?key|secret[_-]?key|security[_-]?token|credential|authorization|cookie|signature|nonce|state|cosy[_-]?key|session)`)
	scrubJSON   = regexp.MustCompile(`"([^"\\]{1,80})"\s*:\s*"([^"\\]*)"`)
	scrubKV     = regexp.MustCompile(`([A-Za-z0-9_.\-]{2,40})=([^\s&"',;}\]]{3,})`)
	scrubURL    = regexp.MustCompile(`https?://[^\s"'<>),\]}]+`)
)

func scrubSecrets(s string) string {
	s = scrubBearer.ReplaceAllString(s, "${1}***")
	s = scrubJWT.ReplaceAllString(s, "***")
	s = scrubJSON.ReplaceAllStringFunc(s, func(m string) string {
		p := scrubJSON.FindStringSubmatch(m)
		if scrubKey.MatchString(p[1]) {
			return `"` + p[1] + `":"***"`
		}
		return m
	})
	s = scrubKV.ReplaceAllStringFunc(s, func(m string) string {
		p := scrubKV.FindStringSubmatch(m)
		if scrubKey.MatchString(p[1]) {
			return p[1] + "=***"
		}
		return m
	})
	return scrubURL.ReplaceAllStringFunc(s, func(u string) string {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			return u[:i+1] + "***"
		}
		return u
	})
}
