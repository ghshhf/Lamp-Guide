package update

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewerComparesSemverCorrectly(t *testing.T) {
	cases := []struct {
		remote, current string
		want            bool
	}{
		{"1.5.0", "1.4.11", true}, // minor beats patch depth
		{"1.4.11", "1.4.2", true}, // numeric, not string compare ("11" < "2")
		{"1.4.11", "1.4.11", false},
		{"1.4.0", "1.4.11", false},
		{"v1.6.0", "1.5.0", true}, // tag prefix tolerated
		{"", "1.4.11", false},     // unparseable remote never updates
	}
	for _, c := range cases {
		if got := Newer(c.remote, c.current); got != c.want {
			t.Fatalf("Newer(%q, %q) = %v, want %v", c.remote, c.current, got, c.want)
		}
	}
}

func TestCheckAnswersWithoutFeed(t *testing.T) {
	// The pre-fix default: an empty feed must report "no update" without
	// erroring — but the shipped default now points at this repository.
	has, _, _, _, err := Check("", nil)
	if has || err != nil {
		t.Fatalf("empty feed must be a silent no: has=%v err=%v", has, err)
	}
	if FeedURL == "" {
		t.Fatal("FeedURL default must not be empty: the update check would never look anywhere")
	}
}

func TestResolveFeedSeparatesDesktopReleaseStream(t *testing.T) {
	for _, configured := range []string{"", "  ", "https://api.github.com/repos/LAGcomcom/zen-gate/releases/latest", " https://api.github.com/repos/LAGcomcom/zen-gate/releases/latest/ ", "https://api.github.com/repos/LAGcomcom/Lamp-Guide/releases/latest"} {
		if got := ResolveFeed(configured); got != FeedURL {
			t.Errorf("inherited feed resolved to %q, want %q", got, FeedURL)
		}
	}
	custom := "https://example.com/my-release-feed.json"
	if got := ResolveFeed(" " + custom + " "); got != custom {
		t.Errorf("custom feed changed to %q", got)
	}
}

func TestGetRetriesWithoutProxyOnRateLimit(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{"version":"1.6.1"}`))
	}))
	defer srv.Close()
	h := HTTP{Timeout: 5 * time.Second, Client: srv.Client()}
	data, err := h.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "1.6.1") {
		t.Fatalf("fallback did not fetch the feed: %s", data)
	}
	if n != 2 {
		t.Fatalf("expected a fallback retry after 403, got %d requests", n)
	}
}

func TestGetDoesNotRetryOtherStatuses(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	h := HTTP{Timeout: 5 * time.Second, Client: srv.Client()}
	if _, err := h.Get(srv.URL); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if n != 1 {
		t.Fatalf("non-rate-limit statuses must not be retried, got %d requests", n)
	}
}

func TestNewerHandlesPrereleaseAndGarbage(t *testing.T) {
	if !Newer("1.2.10-rc1", "1.2.2") {
		t.Fatal("a prerelease suffix must not collapse the tag to 0.0.0")
	}
	if Newer("1.2.2", "1.2.10-rc1") {
		t.Fatal("an older version must not win")
	}
	if Newer("not-a-version", "1.2.2") {
		t.Fatal("a malformed remote tag must never trigger an update")
	}
}

// A release page is HTML and comfortably exceeds the 1 MiB size floor. It
// must never be handed to Apply() as if it were the Windows binary.
func TestParseGitHubReleaseNeverHandsOutHTMLPage(t *testing.T) {
	data := []byte(`{"tag_name":"v1.9.0","html_url":"https://github.com/o/r/releases/tag/v1.9.0","body":"notes","assets":[]}`)
	_, _, url, _, err := parseGitHubRelease(data)
	if err != nil {
		t.Fatal(err)
	}
	if assetName() != "" && url != "" {
		t.Fatalf("no matching asset must not fall back to the release page, got %q", url)
	}
	if assetName() == "" && url == "" {
		t.Fatal("bundled platforms still need the release page link")
	}
}

func TestLooksLikePERejectsHTMLPage(t *testing.T) {
	dir := t.TempDir()
	html := filepath.Join(dir, "page.html")
	if err := os.WriteFile(html, []byte("<!DOCTYPE html><html>release page</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if looksLikePE(html) {
		t.Fatal("an HTML release page must not pass as an exe")
	}
	pe := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(pe, []byte("MZ\x90\x00fake-pe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !looksLikePE(pe) {
		t.Fatal("a real PE must pass")
	}
}
