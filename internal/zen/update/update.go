// Package update checks a configurable feed for newer zen-gate builds.
// Two feed shapes are understood:
//   - GitHub Releases API ("https://api.github.com/repos/<o>/<r>/releases/latest")
//     — parsed natively (tag_name/html_url/body/assets)
//   - any URL returning {"version":"x.y.z","url":"https://…"}
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SelfUpdateSupported reports whether 一键更新 can replace the running binary
// in place. Only Windows ships that flow: the app is a single .exe that can
// be renamed while running. Everywhere else the build is a bundle/zip, so the
// dashboard links to the release page and the user installs by hand.
var SelfUpdateSupported = runtime.GOOS == "windows"

// assetName returns this platform's in-place update *family*. Matching is by
// prefix, not by exact filename: this project publishes versioned assets such
// as LampGuide-1.2.3-win-x64.exe, so the inherited exact "zen-gate.exe" could
// never match anything and silently disabled 一键更新. An empty family means
// the platform has no in-place update at all.
func assetName() string {
	if !SelfUpdateSupported {
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		return "LampGuide"
	case "darwin":
		return "LampGuide-darwin"
	default:
		return ""
	}
}

// inPlaceAsset reports whether an asset is a bare application binary that may
// replace the running exe. A Setup installer is also a PE with an MZ header
// and is far above minExeSize — without this filter the 221 MB installer the
// project actually publishes would be renamed over the app and brick it.
func inPlaceAsset(name, family string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if family == "" || !strings.HasPrefix(n, strings.ToLower(family)) {
		return false
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(n, ".exe") {
		return false
	}
	for _, bad := range []string{"setup", "installer", "install-", "webview2", "runtime", "symbols", ".sha256", ".zip", ".msi"} {
		if strings.Contains(n, bad) {
			return false
		}
	}
	return true
}

// FeedURL is the default update feed, overridable via -ldflags at release
// build time and per-install through settings. It points at the repository
// that CI publishes in-place binaries to; pointing at the upstream stream
// would only ever see Setup installers, which Apply must reject.
var FeedURL = "https://api.github.com/repos/ghshhf/Lamp-Guide/releases/latest"

// Current is the running version, overridable via -ldflags.
var Current = "1.2.4"

// ResolveFeed maps inherited defaults (the old zen-gate feed and the upstream
// Lamp Guide feed) onto this repository's release stream without rewriting the
// shared user config. Explicit custom feeds stay intact.
func ResolveFeed(configured string) string {
	feed := strings.TrimSpace(configured)
	trimmed := strings.TrimRight(feed, "/")
	if trimmed == "" ||
		trimmed == "https://api.github.com/repos/LAGcomcom/zen-gate/releases/latest" ||
		trimmed == "https://api.github.com/repos/LAGcomcom/Lamp-Guide/releases/latest" {
		return FeedURL
	}
	return feed
}

// FeedJSON is the expected remote shape for plain feeds.
type FeedJSON struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Notes   string `json:"notes"`
}

// Check fetches the feed and compares against Current.
// Returns (hasUpdate, version, downloadURL, notes, error).
func Check(feedURL string, client getJSONer) (bool, string, string, string, error) {
	if strings.TrimSpace(feedURL) == "" {
		return false, "", "", "", nil
	}
	data, err := client.Get(feedURL)
	if err != nil {
		return false, "", "", "", err
	}
	if IsGitHubFeed(feedURL) {
		return parseGitHubRelease(data)
	}
	var feed FeedJSON
	if err := json.Unmarshal(data, &feed); err != nil {
		return false, "", "", "", err
	}
	return Newer(feed.Version, Current), strings.TrimSpace(feed.Version), strings.TrimSpace(feed.URL), feed.Notes, nil
}

// IsGitHubFeed reports whether the feed URL is a GitHub Releases API endpoint.
func IsGitHubFeed(feedURL string) bool {
	u := strings.TrimSuffix(strings.TrimSpace(feedURL), "/")
	return strings.HasPrefix(u, "https://api.github.com/repos/") && strings.HasSuffix(u, "/releases/latest")
}

// githubRelease mirrors the slice of the Releases API response zen-gate needs.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// parseGitHubRelease maps a Releases/latest payload onto the feed shape,
// preferring this platform's binary asset. Without a matching asset the URL
// stays on the release page: the dashboard then shows the version and sends
// the user to the download, which is what a bundled build needs.
func parseGitHubRelease(data []byte) (bool, string, string, string, error) {
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return false, "", "", "", err
	}
	url := ""
	if want := assetName(); want != "" {
		// Never fall back to the release *page*: it is HTML, comfortably
		// larger than minExeSize, and the dashboard would otherwise feed it
		// to Apply() as a binary. A release shipping only the Setup installer
		// also yields no URL — the user installs it by hand instead.
		for _, a := range rel.Assets {
			if inPlaceAsset(a.Name, want) {
				url = a.BrowserDownloadURL
				break
			}
		}
	}
	return Newer(rel.TagName, Current), strings.TrimSpace(rel.TagName), url, rel.Body, nil
}

// LatestAssetURL returns this platform's binary download URL from a
// Releases/latest payload. It deliberately does *not* fall back to an arbitrary
// asset: handing a Windows exe to a macOS build would replace the app with
// something it cannot run.
func LatestAssetURL(data []byte) (string, int64, error) {
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", 0, err
	}
	want := assetName()
	if want == "" {
		return "", 0, fmt.Errorf("no in-place update is supported on %s", runtime.GOOS)
	}
	for _, a := range rel.Assets {
		if inPlaceAsset(a.Name, want) {
			return a.BrowserDownloadURL, a.Size, nil
		}
	}
	return "", 0, fmt.Errorf("release %s has no in-place %s binary asset", rel.TagName, want)
}

type getJSONer interface {
	Get(url string) ([]byte, error)
}

// Newer compares dotted versions; "1.2.10" > "1.2.9". A malformed or
// unparseable remote tag must never win: parseVersion used to return [0,0,0]
// for "1.2.10-rc1" and the len(a)==0 guard was unreachable on a fixed array.
func Newer(remote, current string) bool {
	a, ok := parseVersion(remote)
	if !ok {
		return false
	}
	b, ok := parseVersion(current)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // "1.2.10-rc1" → "1.2.10"
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// HTTP fetcher with a sane timeout. An optional Client (e.g. the lane's
// proxy-aware one) takes precedence over the default transport.
type HTTP struct {
	Timeout time.Duration
	Client  *http.Client
}

// Get fetches the feed. When the primary route answers 403 or 429 — a shared
// proxy exit exhausting GitHub's unauthenticated rate limit is the usual
// cause, and it flaps as the exit rotates — the fetch retries once with a
// no-proxy client, since api.github.com is reachable directly from networks
// where github.com itself is not.
func (h HTTP) Get(url string) ([]byte, error) {
	data, err := h.fetch(url, h.Client)
	if err != nil && isRateLimited(err) {
		var fallback *http.Client
		if h.Client != nil {
			fallback = &http.Client{Timeout: h.timeout(), Transport: &http.Transport{Proxy: nil}}
		}
		return h.fetch(url, fallback)
	}
	return data, err
}

func (h HTTP) timeout() time.Duration {
	if h.Timeout <= 0 {
		return 15 * time.Second
	}
	return h.Timeout
}

// fetch runs one GET and turns non-2xx statuses into a statusError.
func (h HTTP) fetch(url string, client *http.Client) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: h.timeout()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &statusError{Code: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// statusError carries the feed's HTTP status so the fallback can tell a rate
// limit or WAF refusal from a transport fault.
type statusError struct{ Code int }

func (e *statusError) Error() string { return fmt.Sprintf("update feed returned HTTP %d", e.Code) }

func isRateLimited(err error) bool {
	se, ok := err.(*statusError)
	return ok && (se.Code == 403 || se.Code == 429)
}
