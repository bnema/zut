package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bnema/zut/packages/provider"
)

const (
	maxSessionSearchRowBytes = 1 << 20
	maxSessionSearchRunes    = 512
)

// SessionSearchSegment is one bounded, display-safe user or assistant text
// excerpt from a persisted session. It deliberately never contains tool,
// developer, image, or raw JSON data.
type SessionSearchSegment struct {
	Path       string
	Text       string
	Normalized string
	Role       provider.Role
	Time       time.Time
	Order      int
}

// ListSessionPathsContext lists eligible session files for cwd or, when all is
// true, for every cwd bucket in root's active session namespace. It ignores
// symlinks, malformed buckets, files with invalid metadata, and files whose
// stored cwd does not hash to the directory that contains them.
func ListSessionPathsContext(ctx context.Context, root, cwd string, all bool) []string {
	if err := contextErr(ctx); err != nil || strings.TrimSpace(root) == "" {
		return nil
	}

	var dirs []string
	if all {
		sessionsDir := filepath.Join(root, "sessions")
		entries, err := os.ReadDir(sessionsDir)
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			if err := contextErr(ctx); err != nil {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !isSessionBucketName(entry.Name()) {
				continue
			}
			dirs = append(dirs, filepath.Join(sessionsDir, entry.Name()))
		}
	} else {
		dir := SessionsDir(root, cwd)
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil
		}
		dirs = []string{dir}
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var candidates []candidate
	for _, dir := range dirs {
		if err := contextErr(ctx); err != nil {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if err := contextErr(ctx); err != nil {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			meta, err := scanSessionMetaContext(ctx, path)
			if err != nil || meta.ID == "" || meta.CWD == "" || meta.HideFromSessions {
				continue
			}
			if filepath.Clean(SessionsDir(root, meta.CWD)) != filepath.Clean(dir) {
				continue
			}
			if !all && meta.CWD != cwd {
				continue
			}
			candidates = append(candidates, candidate{path: path, mod: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].mod.Equal(candidates[j].mod) {
			return candidates[i].mod.After(candidates[j].mod)
		}
		return candidates[i].path > candidates[j].path
	})
	paths := make([]string, len(candidates))
	for i, candidate := range candidates {
		paths[i] = candidate.path
	}
	return paths
}

// ManagedSessionMeta validates that path is a regular session file directly
// owned by root's active namespace and returns its latest metadata. It is the
// trust boundary for a picker selection before a host changes workspace state.
func ManagedSessionMeta(ctx context.Context, root, path string) (SessionMeta, error) {
	if err := contextErr(ctx); err != nil {
		return SessionMeta{}, err
	}
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return SessionMeta{}, errors.New("managed session: root or path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return SessionMeta{}, fmt.Errorf("managed session: stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SessionMeta{}, errors.New("managed session: path is not a regular file")
	}
	meta, err := scanSessionMetaContext(ctx, path)
	if err != nil {
		return SessionMeta{}, err
	}
	if meta.ID == "" || meta.CWD == "" {
		return SessionMeta{}, errors.New("managed session: invalid session metadata")
	}
	wantDir := filepath.Clean(SessionsDir(root, meta.CWD))
	dirInfo, err := os.Lstat(wantDir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return SessionMeta{}, errors.New("managed session: cwd bucket is not a directory")
	}
	if filepath.Clean(filepath.Dir(path)) != wantDir {
		return SessionMeta{}, errors.New("managed session: cwd bucket does not match metadata")
	}
	return meta, nil
}

// ReadSessionSearchSegments extracts bounded searchable text from persisted
// message rows. It keeps historical user/assistant rows after compaction but
// excludes compaction payloads and every non-text content block.
func ReadSessionSearchSegments(ctx context.Context, path string) ([]SessionSearchSegment, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read session search text: open: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	var segments []SessionSearchSegment
	order := 0
	for {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		line, err := readBoundedSessionSearchLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read session search text: %w", err)
		}
		if len(line) > 0 {
			var head sessionLineHead
			if jsonErr := json.Unmarshal(line, &head); jsonErr == nil && head.Type == "message" {
				message, messageErr := readSearchMessage(line)
				if messageErr == nil && (message.Role == provider.RoleUser || message.Role == provider.RoleAssistant) {
					for _, rawText := range message.Texts {
						for _, text := range splitSessionSearchText(rawText) {
							segments = append(segments, SessionSearchSegment{Path: path, Text: text, Normalized: NormalizeSessionSearchText(text), Role: message.Role, Time: message.Time, Order: order})
							order++
						}
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return segments, nil
}

type sessionSearchMessage struct {
	Role  provider.Role
	Time  time.Time
	Texts []string
}

func readSearchMessage(line []byte) (sessionSearchMessage, error) {
	var row struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return sessionSearchMessage{}, err
	}
	if len(row.Message) == 0 || bytes.Equal(bytes.TrimSpace(row.Message), []byte("null")) {
		return sessionSearchMessage{}, errors.New("message row has no message")
	}
	var message struct {
		Role    provider.Role     `json:"role"`
		Time    time.Time         `json:"time"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(row.Message, &message); err != nil {
		return sessionSearchMessage{}, err
	}
	if message.Role != provider.RoleUser && message.Role != provider.RoleAssistant && message.Role != provider.RoleTool && message.Role != provider.RoleDeveloper {
		return sessionSearchMessage{}, fmt.Errorf("message has invalid role %q", message.Role)
	}
	if len(message.Content) == 0 {
		return sessionSearchMessage{}, errors.New("message has no content")
	}
	result := sessionSearchMessage{Role: message.Role, Time: message.Time}
	for _, rawContent := range message.Content {
		var textBlock struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rawContent, &textBlock); err != nil {
			return sessionSearchMessage{}, err
		}
		if textBlock.Text != "" {
			result.Texts = append(result.Texts, textBlock.Text)
		}
	}
	return result, nil
}

func readBoundedSessionSearchLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > maxSessionSearchRowBytes {
			return nil, fmt.Errorf("JSONL row exceeds %d bytes", maxSessionSearchRowBytes)
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return bytes.TrimRight(line, "\r\n"), io.EOF
		default:
			return nil, err
		}
	}
}

func splitSessionSearchText(text string) []string {
	text = sanitizeSessionSearchText(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= maxSessionSearchRunes {
		return []string{text}
	}
	segments := make([]string, 0, len(runes)/maxSessionSearchRunes+1)
	for len(runes) > 0 {
		end := min(len(runes), maxSessionSearchRunes)
		if end < len(runes) {
			for split := end - 1; split > end/2; split-- {
				if unicode.IsSpace(runes[split]) || unicode.Is(unicode.Sentence_Terminal, runes[split]) {
					end = split + 1
					break
				}
			}
		}
		segment := strings.TrimSpace(string(runes[:end]))
		if segment != "" {
			segments = append(segments, segment)
		}
		runes = runes[end:]
	}
	return segments
}

// NormalizeSessionSearchText returns the display-safe case-folded form used
// by the picker matcher. It removes terminal and bidi controls and collapses
// whitespace without retaining any raw transcript representation.
func NormalizeSessionSearchText(text string) string {
	return strings.ToLower(sanitizeSessionSearchText(text))
}

func sanitizeSessionSearchText(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	space := true
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsSpace(r) {
			if !space {
				out.WriteByte(' ')
				space = true
			}
			continue
		}
		out.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(out.String())
}
