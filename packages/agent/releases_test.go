package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseSources(t *testing.T) {
	for _, mode := range []string{"success", "network", "status", "json", "empty", "whitespace", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			primaryCalls, mirrorCalls := 0, 0
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls++
				if r.Header.Get("Authorization") != "" {
					t.Error("token sent to primary")
				}
				switch mode {
				case "status":
					w.WriteHeader(503)
				case "json":
					fmt.Fprint(w, "not json")
				case "empty":
					fmt.Fprint(w, `{}`)
				case "whitespace":
					fmt.Fprint(w, `{"tag_name":" "}`)
				case "trailing":
					fmt.Fprint(w, `{"tag_name":"v2.0.0"} garbage`)
				default:
					fmt.Fprint(w, `{"tag_name":"v2.0.0","html_url":"https://evil.example/tag","body":"Primary notes","assets":[{"browser_download_url":"https://evil.example/archive"}]}`)
				}
			}))
			defer primary.Close()
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mirrorCalls++
				if r.URL.Path != "/latest" && r.URL.Path != "/tags/v2.0.0" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				fmt.Fprint(w, `{"tag_name":"v2.0.0","body":"Mirror notes"}`)
			}))
			defer mirror.Close()
			old := releaseSources
			releaseSources = []releaseSource{{primary.URL, old[0].downloads}, {mirror.URL, old[1].downloads}}
			t.Cleanup(func() { releaseSources = old })
			t.Setenv("GITHUB_TOKEN", "synthetic")
			if mode == "network" {
				primary.Close()
			}
			release, err := fetchRelease(context.Background(), "latest")
			if err != nil {
				t.Fatal(err)
			}
			host, notes := "git.bnema.dev/zut", "Primary notes"
			if mode != "success" {
				host, notes = "github.com/bnema/zut", "Mirror notes"
			}
			archive, sums := release.downloadURLs("zut.tar.gz")
			base := "https://" + host + "/releases/download/v2.0.0/"
			if archive != base+"zut.tar.gz" || sums != base+"checksums.txt" {
				t.Fatalf("wrong source: %s %s", archive, sums)
			}
			if err := validateDownloadURL(archive); err != nil {
				t.Fatal(err)
			}
			info, err := FetchChangelog(context.Background(), "2.0.0")
			if err != nil || info.Body != notes || info.Version != "2.0.0" {
				t.Fatalf("changelog %+v, %v", info, err)
			}
			if mode == "success" && (mirrorCalls != 0 || primaryCalls != 2) {
				t.Fatalf("calls primary=%d mirror=%d", primaryCalls, mirrorCalls)
			}
			if mode != "success" && mirrorCalls != 2 {
				t.Fatalf("mirror calls=%d", mirrorCalls)
			}
		})
	}
}

func TestDownloadHostValidation(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/file", "https://evil.example/file", "https://github.com.evil.example/file",
		"https://user@github.com/file", "https://github.com:444/file", "//github.com/file", "https://git.bnema.dev@evil.example/file",
		"https://objects.githubusercontent.com/file", "https://release-assets.githubusercontent.com/file",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := validateDownloadURL(raw); err == nil {
				t.Fatal("accepted unsafe URL")
			}
			if err := downloadFile(context.Background(), raw, filepath.Join(t.TempDir(), "download")); err == nil || !strings.Contains(err.Error(), "download URL") {
				t.Fatalf("download did not reject URL: %v", err)
			}
		})
	}
	for _, raw := range []string{"https://git.bnema.dev/zut/file", "https://github.com/bnema/zut/file"} {
		if err := validateDownloadURL(raw); err != nil {
			t.Fatal(err)
		}
	}
}

// Route approved production URLs to a local server without relaxing validation.
type releaseTestTransport struct {
	server string
}

func (tr releaseTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target, err := url.Parse(tr.server)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
	return (&http.Transport{}).RoundTrip(clone)
}

func TestDownloadSameSource(t *testing.T) {
	for _, source := range releaseSources {
		t.Run(source.downloads, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Host+r.URL.Path)
				fmt.Fprint(w, "fixture")
			}))
			defer server.Close()
			old := http.DefaultTransport
			http.DefaultTransport = releaseTestTransport{server.URL}
			defer func() { http.DefaultTransport = old }()
			release := releaseMetadata{TagName: "v1.2.3", HTMLURL: "https://evil.example/latest", source: source}
			archive, sums := release.downloadURLs("zut.tar.gz")
			for _, raw := range []string{archive, sums} {
				if err := downloadFile(context.Background(), raw, filepath.Join(t.TempDir(), "asset")); err != nil {
					t.Fatal(err)
				}
			}
			base, _ := url.Parse(source.downloads)
			for _, path := range paths {
				if !strings.HasPrefix(path, base.Host+base.Path+"/v1.2.3/") {
					t.Fatalf("wrong download source: %s", path)
				}
			}
			if len(paths) != 2 {
				t.Fatalf("downloads = %v", paths)
			}
		})
	}
}

func TestDownloadRedirects(t *testing.T) {
	for _, asset := range []string{"archive.tar.gz", "checksums.txt"} {
		for _, target := range []string{
			"https://objects.githubusercontent.com/file", "https://release-assets.githubusercontent.com/file", "https://git.bnema.dev/file",
			"https://evil.example/file", "http://objects.githubusercontent.com/file", "https://user@release-assets.githubusercontent.com/file",
		} {
			t.Run(asset+target, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls == 1 {
						if r.Header.Get("Authorization") != "Bearer synthetic" {
							t.Error("missing initial GitHub token")
						}
						w.Header().Set("Location", target)
						w.WriteHeader(http.StatusFound)
						return
					}
					if r.Header.Get("Authorization") != "" {
						t.Error("token leaked to redirect host")
					}
					fmt.Fprint(w, "fixture")
				}))
				defer server.Close()
				old := http.DefaultTransport
				http.DefaultTransport = releaseTestTransport{server.URL}
				defer func() { http.DefaultTransport = old }()
				t.Setenv("GITHUB_TOKEN", "synthetic")
				err := downloadFile(context.Background(), "https://github.com/releases/"+asset, filepath.Join(t.TempDir(), "download"))
				allowed := target == "https://objects.githubusercontent.com/file" || target == "https://release-assets.githubusercontent.com/file" || target == "https://git.bnema.dev/file"
				if (err == nil) != allowed {
					t.Fatalf("allowed=%v error=%v", allowed, err)
				}
				if !allowed && calls != 1 {
					t.Fatal("unsafe redirect was requested")
				}
				if allowed && calls != 2 {
					t.Fatal("redirect was not requested")
				}
			})
		}
	}
}

func TestEscapedReleaseTag(t *testing.T) {
	r := releaseMetadata{TagName: "release/v1.2.3", source: releaseSources[0]}
	archive, sums := r.downloadURLs("zut.tar.gz")
	for _, raw := range []string{archive, sums} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(parsed.EscapedPath(), "/release%2Fv1.2.3/") {
			t.Fatalf("tag lost escaping: %s", raw)
		}
	}
}

func TestReleaseJSONLimitFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0"}`+strings.Repeat(" ", releaseMetadataLimit))
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v2.0.0"}`)
	}))
	defer mirror.Close()
	old := releaseSources
	releaseSources = []releaseSource{{primary.URL, old[0].downloads}, {mirror.URL, old[1].downloads}}
	defer func() { releaseSources = old }()
	if _, err := fetchReleaseFrom(context.Background(), releaseSources[0], "latest"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("overflow accepted: %v", err)
	}
	release, err := fetchRelease(context.Background(), "latest")
	if err != nil || release.TagName != "v2.0.0" {
		t.Fatalf("fallback failed: %+v %v", release, err)
	}
}

func TestChecksumSizeLimit(t *testing.T) {
	for _, size := range []int{checksumLimit, checksumLimit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.Repeat("x", size))
			}))
			defer server.Close()
			old := http.DefaultTransport
			http.DefaultTransport = releaseTestTransport{server.URL}
			defer func() { http.DefaultTransport = old }()
			err := downloadFile(context.Background(), "https://github.com/releases/checksums.txt", filepath.Join(t.TempDir(), "sums"))
			if size > checksumLimit {
				if err == nil || !strings.Contains(err.Error(), "size limit") {
					t.Fatalf("overflow accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

type releaseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f releaseRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReleaseTimeoutFallsBack(t *testing.T) {
	oldTransport, oldTimeout := http.DefaultTransport, releaseAttemptTimeout
	defer func() { http.DefaultTransport, releaseAttemptTimeout = oldTransport, oldTimeout }()
	releaseAttemptTimeout = time.Millisecond
	calls := 0
	http.DefaultTransport = releaseRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if r.Context().Err() != nil {
			t.Fatal("fallback inherited expired context")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v2.0.0"}`)), Header: make(http.Header), Request: r}, nil
	})
	release, err := fetchRelease(context.Background(), "latest")
	if err != nil || release.source.downloads != releaseSources[1].downloads || calls != 2 {
		t.Fatalf("release=%+v calls=%d error=%v", release, calls, err)
	}
}

func TestReleaseAPIRedirects(t *testing.T) {
	for _, target := range []string{"https://git.bnema.dev/release", "https://api.github.com/release", "https://evil.example/release", "http://git.bnema.dev/release", "https://user@api.github.com/release", "https://api.github.com:444/release"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wantToken := calls == 1 || target == "https://api.github.com/release"
				if (r.Header.Get("Authorization") == "Bearer synthetic") != wantToken {
					t.Error("incorrect API authorization")
				}
				if calls == 1 {
					w.Header().Set("Location", target)
					w.WriteHeader(302)
					return
				}
				fmt.Fprint(w, `{"tag_name":"v2.0.0"}`)
			}))
			defer server.Close()
			old := http.DefaultTransport
			http.DefaultTransport = releaseTestTransport{server.URL}
			defer func() { http.DefaultTransport = old }()
			t.Setenv("GITHUB_TOKEN", "synthetic")
			_, err := fetchReleaseFrom(context.Background(), releaseSource{api: "https://api.github.com/releases"}, "latest")
			allowed := target == "https://git.bnema.dev/release" || target == "https://api.github.com/release"
			if (err == nil) != allowed {
				t.Fatalf("allowed=%v err=%v", allowed, err)
			}
			if !allowed && calls != 1 {
				t.Fatal("unsafe redirect requested")
			}
		})
	}
}

func TestRedirectCountLimits(t *testing.T) {
	for _, api := range []bool{false, true} {
		t.Run(fmt.Sprint(api), func(t *testing.T) {
			calls := 0
			host := "github.com"
			if api {
				host = "api.github.com"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://"+host+"/loop")
				w.WriteHeader(302)
			}))
			defer server.Close()
			old := http.DefaultTransport
			http.DefaultTransport = releaseTestTransport{server.URL}
			defer func() { http.DefaultTransport = old }()
			var err error
			if api {
				_, err = fetchReleaseFrom(context.Background(), releaseSource{api: "https://" + host}, "latest")
			} else {
				err = downloadFile(context.Background(), "https://"+host+"/checksums.txt", filepath.Join(t.TempDir(), "sums"))
			}
			if err == nil || !strings.Contains(err.Error(), "too many redirects") || calls != 10 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestUpdateChecksumMismatchStopsInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest"):
			fmt.Fprint(w, `{"tag_name":"v99.0.0"}`)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			asset, _, err := releaseAssetName("99.0.0")
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), asset)
		default:
			fmt.Fprint(w, "not an archive: extraction must not run")
		}
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = releaseTestTransport{server.URL}
	defer func() { http.DefaultTransport = old }()
	err := runUpdate("1.0.0")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum rejection before extraction/replacement: %v", err)
	}
}
