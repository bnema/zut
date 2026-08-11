package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// updateCheckTTL is how often we hit the GitHub API to look for a new
// release. Half a day is frequent enough to notice the same day a
// release ships without spamming the API on every zut launch.
const updateCheckTTL = 12 * time.Hour

// updateCheckFile is the on-disk cache, keyed to the current binary
// version. Writes a JSON blob with the last observed latest tag and
// when we checked, so we can skip the network call on most launches.
const updateCheckFile = "update-check.json"

// githubReleasesAPI is the REST endpoint we query. Using the API (not
// the HTML redirect) because the JSON response is stable and small.
const githubReleasesAPI = "https://api.github.com/repos/bnema/zut/releases/latest"

const devVersionNotice = "dev version detected; skipping update check"

// UpdateInfo describes the result of an update check. Zero-value means
// "no update available, no error, don't show anything".
type UpdateInfo struct {
	Current   string // e.g. "0.0.4"
	Latest    string // e.g. "0.0.5"
	Available bool   // true when latest > current
	URL       string // release page url for the changelog link
}

func isDevVersion(version string) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexAny(version, " ("); i > 0 {
		version = version[:i]
	}
	return version == "" || version == "dev" || version == "0.0.0" ||
		version == "0.0.0-dev" || strings.HasPrefix(version, "0.0.0-dev+")
}

// updateCache is the on-disk structure written to $ZUT_HOME.
type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	// The version that was current when we last checked. Invalidates
	// the cache if the binary itself has been updated since.
	CurrentAt string `json:"current_at"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
}

// CheckForUpdate returns info about a newer release, using a cached
// result when one is fresh enough. Designed to be called at tui
// startup and rendered as a dismissible banner.
//
// Always returns a usable UpdateInfo (zero-value on error). The
// banner renderer skips the display when Available is false, so a
// network failure silently no-ops; we never block startup on this.
func CheckForUpdate(ctx context.Context, zutHome, currentVersion string) UpdateInfo {
	// Development builds never have an update to offer. Skip without
	// reading the cache or touching the network.
	if isDevVersion(currentVersion) {
		return UpdateInfo{}
	}

	cachePath := filepath.Join(zutHome, updateCheckFile)
	if c, ok := readUpdateCache(cachePath); ok {
		// Cache is fresh and tracks the same binary version.
		// Additional guard: only trust the cache when it already
		// reports an available update. If the cache says "up to
		// date" (Latest <= Current) we re-check anyway so a
		// release published after the last launch is picked up
		// without waiting out the full TTL. The API call is a
		// single 4s request; cheap to do on every up-to-date
		// launch, and skips entirely for the common "installed a
		// new version, cache reports update available" path.
		if time.Since(c.CheckedAt) < updateCheckTTL && c.CurrentAt == currentVersion {
			info := buildInfo(currentVersion, c.Latest, c.URL)
			if info.Available {
				return info
			}
			// fall through to re-check
		}
	}

	latest, url, err := fetchLatestRelease(ctx)
	if err != nil {
		// Network or auth failure (common while the repo is private
		// and no GITHUB_TOKEN is set). Silent no-op; we'll try again
		// after the TTL on the next launch.
		return UpdateInfo{}
	}

	_ = writeUpdateCache(cachePath, updateCache{
		CheckedAt: time.Now().UTC(),
		CurrentAt: currentVersion,
		Latest:    latest,
		URL:       url,
	})

	return buildInfo(currentVersion, latest, url)
}

// CheckForUpdateAsync runs CheckForUpdate in a goroutine, delivers the
// result to the returned channel, and never blocks startup. The
// channel is always closed; receivers should `ok`-check.
func CheckForUpdateAsync(zutHome, currentVersion string) <-chan UpdateInfo {
	ch := make(chan UpdateInfo, 1)
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		ch <- CheckForUpdate(ctx, zutHome, currentVersion)
	}()
	return ch
}

func buildInfo(current, latest, url string) UpdateInfo {
	info := UpdateInfo{
		Current: current,
		Latest:  strings.TrimPrefix(latest, "v"),
		URL:     url,
	}
	info.Available = versionLess(info.Current, info.Latest)
	return info
}

// versionLess reports whether a is an older semantic version than b.
// Invalid versions return false so update checks never offer a downgrade.
func versionLess(a, b string) bool {
	comparison, err := compareVersions(a, b)
	return err == nil && comparison < 0
}

func compareVersions(a, b string) (int, error) {
	a = normalizedVersion(a)
	b = normalizedVersion(b)
	if !semver.IsValid(a) {
		return 0, fmt.Errorf("invalid semantic version %q", strings.TrimPrefix(a, "v"))
	}
	if !semver.IsValid(b) {
		return 0, fmt.Errorf("invalid semantic version %q", strings.TrimPrefix(b, "v"))
	}
	return semver.Compare(a, b), nil
}

func normalizedVersion(v string) string {
	v = versionOnly(v)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// fetchLatestRelease queries the GitHub API for the latest published
// release. Honours $GITHUB_TOKEN for private repos.
func fetchLatestRelease(ctx context.Context) (tag, url string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", githubReleasesAPI, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", "2022-11-28")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("github api %d", resp.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}
	return body.TagName, body.HTMLURL, nil
}

// readUpdateCache loads the last check result. Returns ok=false on
// any error (missing file, bad json) so callers just treat it as a
// cache miss and re-fetch.
func readUpdateCache(path string) (updateCache, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateCache{}, false
	}
	var c updateCache
	if err := json.Unmarshal(b, &c); err != nil {
		return updateCache{}, false
	}
	return c, true
}

func writeUpdateCache(path string, c updateCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
