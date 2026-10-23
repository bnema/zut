package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const releaseMetadataLimit = 1 << 20
const checksumLimit = 1 << 20

var releaseAttemptTimeout = 4 * time.Second

type releaseSource struct{ api, downloads string }

// Mutable only to inject local API servers in tests. Download hosts remain fixed.
var releaseSources = []releaseSource{
	{"https://git.bnema.dev/-/github/api/repos/bnema/zut/releases", "https://git.bnema.dev/zut/releases/download"},
	{"https://api.github.com/repos/bnema/zut/releases", "https://github.com/bnema/zut/releases/download"},
}

type releaseMetadata struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	source  releaseSource
}

func fetchRelease(ctx context.Context, endpoint string) (releaseMetadata, error) {
	var failures []error
	for _, source := range releaseSources {
		release, err := fetchReleaseFrom(ctx, source, endpoint)
		if err == nil {
			return release, nil
		}
		failures = append(failures, err)
	}
	return releaseMetadata{}, errors.Join(failures...)
}

func fetchReleaseFrom(ctx context.Context, source releaseSource, endpoint string) (releaseMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseAttemptTimeout)
	defer cancel()
	var release releaseMetadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.api+"/"+endpoint, nil)
	if err != nil {
		return release, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	setReleaseAuthorization(req)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		setReleaseAuthorization(req)
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil || (req.URL.Host != "git.bnema.dev" && req.URL.Host != "api.github.com") {
			return errors.New("release API redirect must use HTTPS on git.bnema.dev or api.github.com without userinfo")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return release, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return release, fmt.Errorf("release API %s: HTTP %d", source.api, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, releaseMetadataLimit+1))
	if err != nil {
		return release, err
	}
	if len(data) > releaseMetadataLimit {
		return release, errors.New("release JSON exceeds size limit")
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return release, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return release, errors.New("release API: empty tag_name")
	}
	release.source = source
	return release, nil
}

// Never derive download locations from untrusted html_url or asset metadata.
func (r releaseMetadata) downloadURLs(asset string) (archive, checksums string) {
	base := r.source.downloads + "/" + url.PathEscape(r.TagName) + "/"
	return base + url.PathEscape(asset), base + "checksums.txt"
}

func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.User != nil || (u.Host != "git.bnema.dev" && u.Host != "github.com") {
		return errors.New("download URL must use HTTPS on git.bnema.dev or github.com without userinfo")
	}
	return nil
}

// Redirects may additionally reach the two GitHub release CDN hosts.
func validateDownloadRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "https" && u.User == nil && (u.Host == "objects.githubusercontent.com" || u.Host == "release-assets.githubusercontent.com") {
		return nil
	}
	return validateDownloadURL(raw)
}

func setDownloadAuthorization(req *http.Request) {
	req.Header.Del("Authorization")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && req.URL.Host == "github.com" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

func setReleaseAuthorization(req *http.Request) {
	req.Header.Del("Authorization")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && req.URL.Host == "api.github.com" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
