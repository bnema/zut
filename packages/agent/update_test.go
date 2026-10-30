package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
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

func fixedProbe(t *testing.T, version string, running bool) daemonProbe {
	return func(ctx context.Context) (string, bool) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("probe must have a short timeout")
		}
		return version, running
	}
}

func noRestart(t *testing.T) func(io.Writer, string) error {
	return func(io.Writer, string) error { t.Fatal("must not restart"); return nil }
}

const staleHint = "zut: the running daemon uses version 1.2.2, not 1.2.3; run 'zut daemon restart' to use 1.2.3 (sessions are restored idle)\n"

func TestUpdateDaemonHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		running bool
		want    string
	}{
		{name: "older daemon", version: "1.2.2 (abc, today)", running: true, want: staleHint},
		{name: "matching daemon", version: "1.2.3 (def, today)", running: true},
		{name: "absent", running: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := updateDaemonAfterInstall(&out, "1.2.3", false, fixedProbe(t, tc.version, tc.running), noRestart(t)); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestUpdateDaemonRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{name: "success"}, {name: "failure", err: errors.New("restart failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := updateDaemonAfterInstall(&out, "1.2.3", true, func(context.Context) (string, bool) {
				t.Fatal("explicit restart does not need a probe")
				return "", false
			}, func(w io.Writer, version string) error {
				calls++
				if w != &out || version != "1.2.3" {
					t.Fatal("restart arguments not forwarded")
				}
				return tc.err
			})
			if calls != 1 || !errors.Is(err, tc.err) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected hint: %q", out.String())
			}
		})
	}
}

func TestUpdateDaemonWhenCurrent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		restart     bool
		version     string
		running     bool
		wantRestart bool
		want        string
	}{
		{name: "restart stale daemon", restart: true, version: "1.2.2", running: true, wantRestart: true},
		{name: "restart current daemon", restart: true, version: "1.2.3 (abc, today)", running: true, want: "zut: daemon already runs 1.2.3; nothing to restart\n"},
		{name: "restart absent daemon", restart: true, want: "zut: no daemon is running; nothing to restart\n"},
		{name: "hint stale daemon", version: "1.2.2", running: true, want: staleHint},
		{name: "no hint for current daemon", version: "1.2.3", running: true},
		{name: "no hint for absent daemon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := updateDaemonWhenCurrent(&out, "1.2.3", tc.restart, fixedProbe(t, tc.version, tc.running), func(w io.Writer, version string) error {
				calls++
				if version != "1.2.3" {
					t.Fatalf("restart version = %q", version)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if (calls == 1) != tc.wantRestart || calls > 1 {
				t.Fatalf("restart calls = %d, want restart %v", calls, tc.wantRestart)
			}
			if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestUpdateDaemonProbeAbsent(t *testing.T) {
	t.Setenv("ZUT_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, running := updateDaemonProbe(ctx); running {
		t.Fatal("absent daemon reported running")
	}
}

func TestUpdateDaemonProbeRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test server")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZUT_HOME", t.TempDir())
	sock := filepath.Join(dir, "d.sock")
	t.Setenv("ZUT_DAEMON_SOCKET", sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			conn := daemonproto.NewConn(nc, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) {
				switch r.Type {
				case "hello":
					return daemonproto.HelloData{ProtocolVersion: daemonproto.ProtocolVersion, DaemonVersion: "9.8.7"}, nil
				case "daemon_status":
					return daemonproto.DaemonStatusData{PID: os.Getpid(), Version: "9.8.7 (abc, today)"}, nil
				}
				return nil, errors.New("unsupported")
			}})
			go func() { _ = conn.Serve(context.Background()) }()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, running := updateDaemonProbe(ctx)
	if !running || version != "9.8.7 (abc, today)" {
		t.Fatalf("probe = (%q, %v), want running 9.8.7", version, running)
	}
}
