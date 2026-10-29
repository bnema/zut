package agent

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
)

func TestBotStartSpecCapturesClientContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZUT_BOT_SPEC_TEST", "1")
	spec, err := botStartSpec("telegram", []string{"--cwd", dir, "--model", "m"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "telegram" || !filepath.IsAbs(spec.CWD) || filepath.Clean(spec.CWD) != filepath.Clean(dir) {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Args) != 4 || spec.Args[3] != "m" {
		t.Fatalf("args = %v", spec.Args)
	}
	found := false
	for _, e := range spec.Env {
		found = found || e == "ZUT_BOT_SPEC_TEST=1"
	}
	if !found {
		t.Fatal("client environment not captured")
	}
	if _, err := botStartSpec("telegram", []string{"--no-such-flag"}); err == nil {
		t.Fatal("invalid flag accepted")
	}
}

func TestBotLogFilterKeepsBotLines(t *testing.T) {
	var out bytes.Buffer
	f := &botLogFilter{w: &out, kind: "telegram"}
	_, _ = f.Write([]byte("level=INFO msg=\"daemon started\"\nlevel=INFO msg=\"bot started\" kind=tele"))
	_, _ = f.Write([]byte("gram session=s1\nlevel=INFO msg=x bot=telegram\nlevel=INFO msg=other\n"))
	want := "level=INFO msg=\"bot started\" kind=telegram session=s1\nlevel=INFO msg=x bot=telegram\n"
	if out.String() != want {
		t.Fatalf("filtered = %q", out.String())
	}
}

func TestWriteBotServiceStatus(t *testing.T) {
	var out bytes.Buffer
	writeBotServiceStatus(&out, daemonproto.BotStatusData{LastError: "getMe: unauthorized", Bridge: &daemonproto.TelegramBridgeData{Active: true, Session: "s2"}})
	want := "service:      stopped (last error: getMe: unauthorized)\nbridge:       mirroring session s2\n"
	if out.String() != want {
		t.Fatalf("status = %q", out.String())
	}
}
