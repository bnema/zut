package mcp

import (
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file holds the diagnostics boundary: everything that originates from a
// server (stderr, HTTP error bodies, JSON-RPC error messages) or from the HTTP
// client (url.Error carries the full request URL) is scrubbed here before it
// reaches an error value, the terminal, or a log.

const (
	redactedMarker = "[redacted]"
	// minSecretLen keeps one- and two-character configuration values (flags
	// such as "1") from turning every diagnostic into noise.
	minSecretLen = 4
)

// redactor removes configured secret values from server-provided text.
// A nil *redactor still strips URLs' credentials and control characters.
type redactor struct {
	secrets []string
	// maxLen is the length of the longest secret, used to size the overlap
	// when long lines are cut for display.
	maxLen int
}

// newRedactor collects the secret-bearing values of one server configuration:
// env and header values, the bearer token, values passed through from the host
// environment, and credentials embedded in the URL.
func newRedactor(cfg ServerConfig) *redactor {
	set := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen {
			return
		}
		set[v] = true
		// The control-stripped form too: text is sanitized before it is
		// matched, so a secret that itself contains escape bytes must match
		// in its sanitized shape.
		if clean := sanitizeText(v, true); len(clean) >= minSecretLen {
			set[clean] = true
		}
		// "Bearer abc123" is also worth redacting as just "abc123".
		if strings.ContainsAny(v, " \t") {
			for _, field := range strings.Fields(v) {
				if len(field) >= 8 {
					set[field] = true
				}
			}
		}
	}
	for _, v := range cfg.Env {
		add(v)
	}
	for _, v := range cfg.Headers {
		add(v)
	}
	for _, name := range cfg.EnvHeaders {
		if v, ok := os.LookupEnv(name); ok {
			add(v)
		}
	}
	for _, name := range cfg.EnvVars {
		if v, ok := os.LookupEnv(name); ok {
			add(v)
		}
	}
	if cfg.BearerTokenEnvVar != "" {
		if v, ok := os.LookupEnv(cfg.BearerTokenEnvVar); ok {
			add(v)
		}
	}
	if cfg.URL != "" {
		for _, v := range urlSecrets(cfg.URL) {
			add(v)
		}
	}
	r := &redactor{}
	for v := range set {
		r.secrets = append(r.secrets, v)
		r.maxLen = max(r.maxLen, len(v))
	}
	// Longest first so a secret containing another is replaced whole.
	sort.Slice(r.secrets, func(i, j int) bool {
		if len(r.secrets[i]) != len(r.secrets[j]) {
			return len(r.secrets[i]) > len(r.secrets[j])
		}
		return r.secrets[i] < r.secrets[j]
	})
	return r
}

// urlSecrets returns the secret-bearing parts of raw: userinfo (user and
// password) and query values, both as written and percent-decoded. It splits
// the text itself instead of requiring url.Parse to succeed, because the URLs
// that matter most here are the malformed ones that fail to parse.
func urlSecrets(raw string) []string {
	var out []string
	add := func(v string) {
		if v == "" {
			return
		}
		out = append(out, v)
		if d, err := url.PathUnescape(v); err == nil && d != v {
			out = append(out, d)
		}
		if d, err := url.QueryUnescape(v); err == nil && d != v {
			out = append(out, d)
		}
	}
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	authority := rest
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		user, pass, _ := strings.Cut(authority[:i], ":")
		add(user)
		add(pass)
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		query := rest[i+1:]
		if j := strings.Index(query, "#"); j >= 0 {
			query = query[:j]
		}
		for _, pair := range strings.FieldsFunc(query, func(r rune) bool { return r == '&' || r == ';' }) {
			if _, v, ok := strings.Cut(pair, "="); ok {
				add(v)
			}
		}
		if values, err := url.ParseQuery(query); err == nil {
			for _, vs := range values {
				for _, v := range vs {
					add(v)
				}
			}
		}
	}
	return out
}

// scrub redacts configured secrets, strips credentials and query strings from
// URLs, and removes terminal control sequences. Newlines are kept.
func (r *redactor) scrub(s string) string {
	if s == "" {
		return s
	}
	// Redact, strip control sequences, then redact again: a secret that has
	// escape bytes inserted into it only becomes matchable once they are gone.
	s = r.redact(s)
	s = sanitizeText(s, true)
	s = r.redact(s)
	return scrubURLs(s)
}

// redact replaces configured secret values. A nil redactor changes nothing.
func (r *redactor) redact(s string) string {
	if r == nil {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redactedMarker)
	}
	return s
}

// trimPartialSecret removes a trailing fragment that is a proper prefix of a
// secret. It is for text that was cut at an arbitrary byte: a secret that
// straddles the cut cannot be matched as a whole, and its prefix must not
// survive either.
func (r *redactor) trimPartialSecret(s string) string {
	if r == nil {
		return s
	}
	drop := 0
	for _, secret := range r.secrets {
		for k := min(len(secret)-1, len(s)); k > drop; k-- {
			if strings.HasSuffix(s, secret[:k]) {
				drop = k
				break
			}
		}
	}
	return s[:len(s)-drop]
}

// scrubOneLine is scrub with newlines collapsed to spaces.
func (r *redactor) scrubOneLine(s string) string {
	return sanitizeText(r.scrub(s), false)
}

var urlInText = regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.-]*://[^\\s\"'<>`]+")

// scrubURLs rewrites every URL in s to scheme://host/path, dropping userinfo,
// query, and fragment.
func scrubURLs(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlInText.ReplaceAllStringFunc(s, stripURL)
}

func stripURL(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	scheme, rest := raw[:schemeEnd+3], raw[schemeEnd+3:]
	end := len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		end = i
	}
	authority, tail := rest[:end], rest[end:]
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if i := strings.IndexAny(tail, "?#"); i >= 0 {
		tail = tail[:i]
	}
	return scheme + authority + tail
}

// scrubbedError carries a sanitized message while keeping the original error
// reachable through errors.Is/As.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubError returns err with a scrubbed message, or nil for nil. Context
// errors are returned unchanged so callers keep comparing them directly.
func (r *redactor) scrubError(err error) error {
	if err == nil {
		return nil
	}
	msg := r.scrub(err.Error())
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, err: err}
}

// sanitizeText removes terminal control sequences (CSI, OSC, DCS/APC/PM/SOS
// strings, other escapes), C0/C1 control characters, and bidirectional
// overrides. Tabs become spaces. Newlines are kept when keepNewlines is true
// and become spaces otherwise; carriage returns are dropped.
func sanitizeText(s string, keepNewlines bool) string {
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\n' || c == 0x7f || c >= 0x80 || (c == '\n' && !keepNewlines) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == 0x1b:
			i = skipEscape(s, i)
		case r == 0x9b:
			i = skipCSI(s, i)
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f:
			i = skipControlString(s, i)
		case r == '\n':
			if keepNewlines {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// dropped (includes \r)
		case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f:
			// bidirectional controls can reorder terminal text
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscape returns the index after the escape sequence whose ESC byte was
// just consumed (i points at the byte after ESC).
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch c := s[i]; {
	case c == '[':
		return skipCSI(s, i+1)
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		return skipControlString(s, i+1)
	case c >= 0x20 && c <= 0x2f:
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) {
			i++ // final byte
		}
		return i
	default:
		return i + 1
	}
}

func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

// skipControlString consumes an OSC/DCS/APC/PM/SOS payload up to and
// including its terminator (BEL, ESC \, or C1 ST). An ESC that does not start
// ST cancels the string without being consumed.
func skipControlString(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			return i
		case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c:
			return i + 2
		}
		i++
	}
	return i
}
