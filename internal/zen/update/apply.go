package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Apply downloads the new binary from a release asset and swaps it with the
// running one. Windows refuses to overwrite a running exe but allows renaming
// it, so the swap is: current → .bak, .new → current, spawn the new exe,
// exit. The caller exits right after Apply returns success; the .bak is
// cleaned up on the next boot (CleanupBackup).
//
// Only Windows reaches this path — see SelfUpdateSupported. Everywhere else
// the release is a bundle the user installs by hand, because replacing a
// signed .app's inner binary in place would break the signature.
//
// download is the release asset URL (https only — the whole point is that the
// payload comes from the GitHub release this update check just authenticated
// against indirectly).

const (
	minExeSize = 1 << 20  // 1 MiB sanity floor
	maxExeSize = 64 << 20 // 64 MiB: the published Setup installer is ~221 MiB
)

// Apply performs the download-and-swap. After a true return the caller should
// start the new exe (Relaunch) and exit.
func Apply(downloadURL string, client *http.Client) (bool, error) {
	if downloadURL == "" {
		return false, fmt.Errorf("no download url")
	}
	if len(downloadURL) < 8 || downloadURL[:8] != "https://" {
		return false, fmt.Errorf("update asset url must be https")
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	exePath, err := filepath.Abs(exe)
	if err != nil {
		return false, err
	}
	newPath := exePath + ".new"
	bakPath := exePath + ".bak"

	// Download to <exe>.new with a generous timeout (exe ≈ 9 MB).
	timeout := 10 * time.Minute
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	resp, err := c.Get(downloadURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("update download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxExeSize {
		return false, fmt.Errorf("update asset is too large to be the application binary (%d bytes); only a bare exe may be swapped in place", resp.ContentLength)
	}
	f, err := os.Create(newPath)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(newPath)
		return false, err
	}
	if closeErr != nil {
		os.Remove(newPath)
		return false, closeErr
	}
	if n < minExeSize {
		os.Remove(newPath)
		return false, fmt.Errorf("downloaded update is suspiciously small (%d bytes)", n)
	}
	if n > maxExeSize {
		os.Remove(newPath)
		return false, fmt.Errorf("downloaded update is too large (%d bytes); refusing to replace the running exe with an installer", n)
	}
	if err := verifyChecksum(newPath, downloadURL, c); err != nil {
		os.Remove(newPath)
		return false, err
	}
	// Size alone is not a format check: a GitHub release *page* is well over
	// 1 MiB and would happily be renamed over the binary. Require the DOS/PE
	// signature before touching the running exe.
	if !looksLikePE(newPath) {
		os.Remove(newPath)
		return false, fmt.Errorf("downloaded update is not a Windows executable (no MZ header)")
	}

	// Only now, with a validated payload in hand, retire the previous
	// rollback copy. Windows refuses to rename onto an existing file.
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		// Keep the new download for inspection but do not swap: without a
		// free backup slot the running exe must not be moved aside.
		return false, fmt.Errorf("could not retire the old rollback copy: %w", err)
	}

	// Swap: running exe → .bak, .new → exe. Windows permits renaming a
	// running binary, which is what makes this work without a helper.
	if err := os.Rename(exePath, bakPath); err != nil {
		os.Remove(newPath)
		return false, fmt.Errorf("could not move the running exe aside: %w", err)
	}
	if err := os.Rename(newPath, exePath); err != nil {
		if rbErr := os.Rename(bakPath, exePath); rbErr != nil {
			// Both copies are still on disk. Never delete .new here: with the
			// rollback failed it may be the only runnable binary left.
			return false, fmt.Errorf("could not place the new exe: %v; rollback also failed: %v; backup=%s new=%s",
				err, rbErr, bakPath, newPath)
		}
		os.Remove(newPath)
		return false, fmt.Errorf("could not place the new exe: %w", err)
	}
	return true, nil
}

// looksLikePE reads the two-byte DOS signature every Windows executable
// carries. It is the cheapest guard against installing an HTML error page,
// an API JSON error, or a redirect landing page as the application binary.
func looksLikePE(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var sig [2]byte
	if _, err := io.ReadFull(f, sig[:]); err != nil {
		return false
	}
	return sig[0] == 'M' && sig[1] == 'Z'
}

// verifyChecksum compares the download against the release's <asset>.sha256
// sidecar when one exists. A missing or malformed sidecar is not fatal (some
// feeds do not publish one); a present-but-mismatched one always is.
func verifyChecksum(path, assetURL string, client *http.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL+".sha256", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), fields[0]) {
		return fmt.Errorf("update checksum mismatch (expected %s)", fields[0])
	}
	return nil
}

// Relaunch starts the freshly installed exe detached and returns an error if
// it could not be started. The caller exits afterwards.
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe).Start()
}

// CleanupBackup clears a leftover download. It deliberately keeps .bak: that
// copy is the only rollback point if the freshly installed binary turns out to
// be broken, and deleting it on the very first boot after an update made a bad
// update unrecoverable. The next successful Apply retires it — but only after
// the replacement has passed the size and PE-signature checks.
func CleanupBackup() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := filepath.Abs(exe)
	if err != nil {
		return
	}
	if _, err := os.Stat(p + ".bak"); err == nil {
		// An unfinished swap may have left the only runnable binary in .new.
		return
	}
	_ = os.Remove(p + ".new")
}
