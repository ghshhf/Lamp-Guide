package update

import (
	"strings"
	"testing"
)

// The published release ships a 221 MB Setup installer. It has an MZ header
// and passes the size floor, so the *name* filter is the only thing standing
// between 一键更新 and a bricked install.
func TestInPlaceAssetRejectsInstallerAndForeignNames(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"LampGuide-Setup-1.2.1-win-x64.exe", false},
		{"LampGuide-Setup-1.2.1-win-x64.exe.sha256", false},
		{"LampGuide-1.2.3-win-x64.exe", true},
		{"LampGuide.exe", true},
		{"zen-gate.exe", false},
		{"release.html", false},
		{"LampGuide-WebView2RuntimeInstaller.exe", false},
	}
	for _, c := range cases {
		if got := inPlaceAsset(c.name, "LampGuide"); got != c.want {
			t.Errorf("inPlaceAsset(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseGitHubReleaseNeverHandsInstallerToApply(t *testing.T) {
	payload := `{"tag_name":"v1.2.9","html_url":"https://github.com/LAGcomcom/Lamp-Guide/releases/tag/v1.2.9","body":"notes","assets":[{"name":"LampGuide-Setup-1.2.9-win-x64.exe","browser_download_url":"https://example.test/setup.exe","size":221574551},{"name":"LampGuide-Setup-1.2.9-win-x64.exe.sha256","browser_download_url":"https://example.test/setup.exe.sha256","size":100}]}`
	has, ver, url, notes, err := parseGitHubRelease([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !has || ver != "v1.2.9" || notes != "notes" {
		t.Fatalf("release metadata lost: has=%v ver=%q notes=%q", has, ver, notes)
	}
	if url != "" {
		t.Fatalf("installer-only release must not produce a download URL, got %q", url)
	}
}

func TestParseGitHubReleasePicksBareExeOverInstaller(t *testing.T) {
	payload := `{"tag_name":"v1.2.9","html_url":"https://github.com/LAGcomcom/Lamp-Guide/releases/tag/v1.2.9","assets":[{"name":"LampGuide-Setup-1.2.9-win-x64.exe","browser_download_url":"https://example.test/setup.exe","size":221574551},{"name":"LampGuide-1.2.9-win-x64.exe","browser_download_url":"https://example.test/LampGuide.exe","size":12681472}]}`
	_, _, url, _, err := parseGitHubRelease([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(url, "/LampGuide.exe") {
		t.Fatalf("expected the bare exe asset, got %q", url)
	}
	assetURL, size, err := LatestAssetURL([]byte(payload))
	if err != nil || !strings.HasSuffix(assetURL, "/LampGuide.exe") || size != 12681472 {
		t.Fatalf("LatestAssetURL = %q %d %v", assetURL, size, err)
	}
}

func TestLatestAssetURLRejectsInstallerOnlyRelease(t *testing.T) {
	payload := `{"tag_name":"v1.2.9","assets":[{"name":"LampGuide-Setup-1.2.9-win-x64.exe","browser_download_url":"https://example.test/setup.exe","size":221574551}]}`
	if _, _, err := LatestAssetURL([]byte(payload)); err == nil {
		t.Fatal("an installer-only release must not be reported as an in-place update")
	}
}
