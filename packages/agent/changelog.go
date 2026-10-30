package agent

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// ChangelogInfo is what FetchChangelog returns. Body is the markdown
// from the release page; URL points back to that page so the
// dialog can offer "open in browser".
type ChangelogInfo struct {
	Version string
	Body    string
	URL     string
}

// FetchChangelog queries ermite, falling back to GitHub, for the given version
// (must already include the leading "v") and returns the release
// notes body. Returns an empty ChangelogInfo on any failure or when
// the body is empty; the caller treats either as "skip silently".
//
// Honours $GITHUB_TOKEN for private-repo access. Times out at 10s so
// startup never blocks on a flaky network.
// semverOnly strips commit hash and date suffixes from version strings
// like "0.1.12 (25b2bd4, 2026-04-25T09:25:45Z)" to get just "0.1.12".
func semverOnly(v string) string {
	if i := strings.IndexByte(v, ' '); i > 0 {
		return v[:i]
	}
	return v
}

func FetchChangelog(ctx context.Context, version string) (ChangelogInfo, error) {
	if isDevVersion(version) {
		return ChangelogInfo{}, nil
	}
	version = semverOnly(version)

	tag := version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	body, err := fetchRelease(ctx, "tags/"+url.PathEscape(tag))
	if err != nil {
		return ChangelogInfo{}, err
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		return ChangelogInfo{}, nil
	}
	// Extract only the changelog section and strip markdown headers.
	body.Body = extractChangelog(body.Body)
	if body.Body == "" {
		return ChangelogInfo{}, nil
	}
	return ChangelogInfo{
		Version: strings.TrimPrefix(body.TagName, "v"),
		Body:    body.Body,
		URL:     body.HTMLURL,
	}, nil
}

// extractChangelog pulls the content starting from "## Changelog"
// (or the whole body if no such header exists) and strips markdown
// heading markers (# ## ###) from every line so the TUI renders
// clean text.
func extractChangelog(body string) string {
	lines := strings.Split(body, "\n")

	// Find the "## Changelog" line.
	start := -1
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.EqualFold(trimmed, "## changelog") ||
			strings.EqualFold(trimmed, "## Changelog") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		// No changelog header found; use the whole body.
		start = 0
	}

	// Process remaining lines: strip # markers but mark headings
	// so the renderer can color them.
	var out []string
	for _, l := range lines[start:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		// Detect markdown headings and strip the # but keep text.
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.TrimLeft(trimmed, "#")
			heading = strings.TrimSpace(heading)
			if heading != "" {
				// Mark headings with a sentinel the dialog can detect.
				out = append(out, "\x00H:"+heading)
			}
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// FetchChangelogAsync runs FetchChangelog on a goroutine and delivers
// the result on the returned channel. Channel always closes.
func FetchChangelogAsync(version string) <-chan ChangelogInfo {
	ch := make(chan ChangelogInfo, 1)
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, _ := FetchChangelog(ctx, version)
		ch <- info
	}()
	return ch
}

// ShouldShowChangelog reports whether the running binary version
// differs from the last version whose changelog the user dismissed.
// Returns false on development builds and on the first-ever launch (no
// LastChangelogShown stored — we don't dump release notes at someone who
// just installed).
func ShouldShowChangelog(currentVersion string, cfg Config) bool {
	if isDevVersion(currentVersion) {
		return false
	}
	currentVersion = semverOnly(currentVersion)
	if cfg.LastChangelogShown == "" {
		return false
	}
	return semverOnly(cfg.LastChangelogShown) != currentVersion
}

// MarkChangelogShown persists the version whose changelog the user
// just dismissed. Idempotent; safe to call when the dialog wasn't
// actually shown (e.g. fetch failed) so we don't keep retrying.
func MarkChangelogShown(version string) error {
	v := semverOnly(version)
	return updateConfig(func(cfg *Config) error {
		if semverOnly(cfg.LastChangelogShown) == v {
			return errConfigUnchanged
		}
		cfg.LastChangelogShown = v
		return nil
	})
}

// SeedChangelogVersion sets LastChangelogShown if it's currently
// empty. Called once on first-ever launch so future upgrades
// correctly trigger the dialog while THIS launch (which is also
// "first-ever") doesn't.
func SeedChangelogVersion(version string) {
	if isDevVersion(version) {
		return
	}
	version = semverOnly(version)
	_ = updateConfig(func(cfg *Config) error {
		if cfg.LastChangelogShown != "" {
			return errConfigUnchanged
		}
		cfg.LastChangelogShown = version
		return nil
	})
}
