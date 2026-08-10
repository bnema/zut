package extensions

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	maxExtensionLogBytes = 1 << 20
	maxHostEnvNames      = 64
	maxHostEnvNameBytes  = 128
	maxHostEnvTotalBytes = 4096
	maxDiagnosticEntries = 100
	maxDiagnosticBytes   = 2048
)

var baselineEnvironment = []string{
	"PATH", "HOME", "PWD", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"USERPROFILE", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
}

var dangerousHostEnv = map[string]struct{}{
	"BASH_ENV": {}, "ENV": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_CONFIG_GLOBAL": {},
	"JAVA_TOOL_OPTIONS": {}, "NODE_OPTIONS": {}, "PERL5OPT": {}, "PYTHONHOME": {},
	"PYTHONPATH": {}, "RUBYOPT": {},
}

func extensionEnvironment(requested []string, lookup func(string) (string, bool)) ([]string, error) {
	if len(requested) > maxHostEnvNames {
		return nil, fmt.Errorf("host_env has %d entries; maximum is %d", len(requested), maxHostEnvNames)
	}
	seen := make(map[string]struct{}, len(baselineEnvironment)+len(requested))
	values := make(map[string]string, len(baselineEnvironment)+len(requested))
	add := func(name string) {
		key := environmentKey(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		if value, ok := lookup(name); ok {
			values[name] = value
		}
	}
	for _, name := range baselineEnvironment {
		add(name)
	}

	total := 0
	for _, name := range requested {
		if err := validateHostEnvName(name); err != nil {
			return nil, err
		}
		total += len(name)
		if total > maxHostEnvTotalBytes {
			return nil, fmt.Errorf("host_env names exceed %d bytes", maxHostEnvTotalBytes)
		}
		key := environmentKey(name)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("host_env contains duplicate or baseline variable %q", name)
		}
		seen[key] = struct{}{}
		if value, ok := lookup(name); ok {
			values[name] = value
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return environmentKey(names[i]) < environmentKey(names[j]) })
	env := make([]string, 0, len(names)+1)
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env, nil
}

func appendBoundedDiagnostic(messages []string, message string) []string {
	if len(message) > maxDiagnosticBytes {
		message = message[:maxDiagnosticBytes]
	}
	if len(messages) == maxDiagnosticEntries {
		copy(messages, messages[1:])
		messages = messages[:maxDiagnosticEntries-1]
	}
	return append(messages, message)
}

func validateExtensionName(name string) error {
	if len(name) > 64 {
		return fmt.Errorf("name %q exceeds 64 bytes", name)
	}
	for i, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-'))) {
			return fmt.Errorf("name %q must start with a letter or digit and contain only letters, digits, dots, underscores, or hyphens", name)
		}
	}
	return nil
}

func validateHostEnvName(name string) error {
	if name == "" || len(name) > maxHostEnvNameBytes {
		return fmt.Errorf("host_env variable name %q must contain 1-%d bytes", name, maxHostEnvNameBytes)
	}
	for i, r := range name {
		if !((r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')) {
			return fmt.Errorf("host_env variable name %q must use portable uppercase identifier syntax", name)
		}
	}
	upper := strings.ToUpper(name)
	if _, denied := dangerousHostEnv[upper]; denied || strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
		return fmt.Errorf("host_env variable %q can alter executable loading and is not allowed", name)
	}
	return nil
}

func environmentKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

type boundedLog struct {
	mu        sync.Mutex
	file      *os.File
	remaining int64
	closed    bool
}

func openBoundedLog(path string) (*boundedLog, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink log path %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &boundedLog{file: file, remaining: maxExtensionLogBytes}, nil
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	original := len(p)
	if l.closed {
		return 0, os.ErrClosed
	}
	if l.remaining <= 0 {
		return original, nil
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.file.Write(p)
	l.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	// Always report the complete input as consumed so a noisy child cannot
	// block after the diagnostic budget is exhausted.
	return original, nil
}

func (l *boundedLog) WriteString(s string) (int, error) {
	return l.Write([]byte(s))
}

func (l *boundedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close()
}

var _ io.WriteCloser = (*boundedLog)(nil)
