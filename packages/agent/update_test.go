package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsDevVersion(t *testing.T) {
	for _, version := range []string{"", "dev", "0.0.0", "0.0.0-dev", "0.0.0-dev (abc123, today)", "0.0.0-dev+local"} {
		if !isDevVersion(version) {
			t.Errorf("isDevVersion(%q) = false, want true", version)
		}
	}
	for _, version := range []string{"0.0.1", "1.0.0", "0.0.0-rc1"} {
		if isDevVersion(version) {
			t.Errorf("isDevVersion(%q) = true, want false", version)
		}
	}
}

type countingRoundTripper struct {
	calls int
}

func (r *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("unexpected network request")
}

func TestCheckForUpdateSkipsDevVersionWithoutCacheOrNetwork(t *testing.T) {
	home := t.TempDir()
	transport := &countingRoundTripper{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := CheckForUpdate(ctx, home, "0.0.0-dev")
	if info != (UpdateInfo{}) {
		t.Fatalf("development check = %+v, want an empty result", info)
	}
	if _, err := os.Stat(filepath.Join(home, updateCheckFile)); !os.IsNotExist(err) {
		t.Fatalf("development check wrote a cache file: %v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("development check made %d network requests, want 0", transport.calls)
	}
}

func TestVersionLess(t *testing.T) {
	pseudo := "0.3.35-0.20260809165004-9e3c28d4b65"
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "older release", a: "0.3.33", b: "0.3.34", want: true},
		{name: "newer release", a: "0.3.35", b: "0.3.34", want: false},
		{name: "pseudo after prior release", a: pseudo, b: "0.3.34", want: false},
		{name: "pseudo before final release", a: pseudo, b: "0.3.35", want: true},
		{name: "prior release before pseudo", a: "0.3.34", b: pseudo, want: true},
		{name: "decorated build", a: "0.3.33 (abc1234, 2026-08-09)", b: "0.3.34", want: true},
		{name: "invalid current", a: "not-a-version", b: "0.3.34", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionLess(tc.a, tc.b); got != tc.want {
				t.Fatalf("versionLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestBuildInfoDoesNotOfferPseudoVersionDowngrade(t *testing.T) {
	current := "0.3.35-0.20260809165004-9e3c28d4b65"
	info := buildInfo(current, "v0.3.34", "https://example.com/release")
	if info.Available {
		t.Fatalf("buildInfo(%q, %q) offered a downgrade", current, info.Latest)
	}
}

func TestUpdateFlags(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		args                          []string
		check, restart, help, wantErr bool
	}{
		{name: "default"},
		{name: "restart", args: []string{"--restart-daemon"}, restart: true},
		{name: "check", args: []string{"--check"}, check: true},
		{name: "help", args: []string{"--restart-daemon", "--help"}, restart: true, help: true},
		{name: "conflict", args: []string{"--check", "--restart-daemon"}, check: true, restart: true, wantErr: true},
		{name: "reverse conflict", args: []string{"--restart-daemon", "--check"}, check: true, restart: true, wantErr: true},
		{name: "unknown", args: []string{"--unknown"}, wantErr: true},
		{name: "unknown after check", args: []string{"--check", "--unknown"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check, restart, help, err := parseUpdateFlags(tc.args)
			if check != tc.check || restart != tc.restart || help != tc.help || (err != nil) != tc.wantErr {
				t.Fatalf("got (%v, %v, %v, %v)", check, restart, help, err)
			}
		})
	}
}

func TestUpdateDaemonHint(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "absent"}[running], func(t *testing.T) {
			var out bytes.Buffer
			err := updateDaemonAfterInstall(&out, "1.2.3", false, func(ctx context.Context) bool {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > time.Second {
					t.Fatal("probe must have a short timeout")
				}
				return running
			}, func(io.Writer, string) error { t.Fatal("must not restart automatically"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if running {
				want = "zut: a daemon is running an older version; run 'zut daemon restart' to use the new version (sessions are restored idle)\n"
			}
			if out.String() != want {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestUpdateDaemonRestart(t *testing.T) {
	for _, restartErr := range []error{nil, errors.New("restart failed")} {
		var out bytes.Buffer
		calls := 0
		err := updateDaemonAfterInstall(&out, "1.2.3", true, func(context.Context) bool {
			t.Fatal("explicit restart does not need a probe")
			return false
		}, func(w io.Writer, version string) error {
			calls++
			if w != &out || version != "1.2.3" {
				t.Fatal("restart arguments not forwarded")
			}
			return restartErr
		})
		if calls != 1 || !errors.Is(err, restartErr) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		if out.Len() != 0 {
			t.Fatalf("unexpected hint: %q", out.String())
		}
	}
}

func TestUpdateDaemonProbeAbsent(t *testing.T) {
	t.Setenv("ZUT_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if updateDaemonRunning(ctx) {
		t.Fatal("absent daemon reported running")
	}
}
