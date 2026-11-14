package tools

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const codemodeSnippetMax = 256 * 1024
const codemodeSnippetHeader = "# zut-snippet: "

var codemodeSnippetName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type codemodeSnippetInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type codemodeSnippetStore struct {
	home    func() string
	sandbox *Sandbox
}

func (s *codemodeSnippetStore) open(create bool) (*os.Root, error) {
	if s == nil || s.home == nil {
		return nil, errors.New("snippet storage unavailable")
	}
	if err := CodemodeAdmission(s.sandbox); err != nil {
		return nil, err
	}
	home := s.home()
	if home == "" {
		return nil, errors.New("snippet home unavailable")
	}
	if create {
		if err := os.MkdirAll(home, 0700); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	for _, part := range []string{"codemode", "snippets"} {
		if create {
			if err := root.Mkdir(part, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
		}
		info, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("snippet directory must not be a symlink")
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
		if create {
			dir, err := root.Open(".")
			if err != nil {
				root.Close()
				return nil, err
			}
			chmodErr := dir.Chmod(0700)
			dir.Close()
			if chmodErr != nil {
				root.Close()
				return nil, chmodErr
			}
		}
	}
	return root, nil
}

func readCodemodeSnippet(root *os.Root, name string) (codemodeSnippetInfo, string, error) {
	var info codemodeSnippetInfo
	if !codemodeSnippetName.MatchString(name) {
		return info, "", errors.New("invalid snippet name")
	}
	filename := name + ".py"
	stat, err := root.Lstat(filename)
	if err != nil {
		return info, "", err
	}
	if !stat.Mode().IsRegular() {
		return info, "", errors.New("snippet must be a regular file")
	}
	f, err := root.Open(filename)
	if err != nil {
		return info, "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, codemodeSnippetMax+8193))
	if err != nil {
		return info, "", err
	}
	header, code, found := bytes.Cut(raw, []byte("\n"))
	if !found || !bytes.HasPrefix(header, []byte(codemodeSnippetHeader)) || len(header) > 8192 || len(code) > codemodeSnippetMax {
		return info, "", errors.New("invalid or oversized snippet")
	}
	if err := json.Unmarshal(header[len(codemodeSnippetHeader):], &info); err != nil {
		return info, "", err
	}
	info.Name = name
	return info, string(code), nil
}

func (s *codemodeSnippetStore) list() ([]codemodeSnippetInfo, []string, error) {
	entries := []codemodeSnippetInfo{}
	warnings := []string{}
	root, err := s.open(false)
	if errors.Is(err, os.ErrNotExist) {
		return entries, warnings, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	files, err := dir.ReadDir(-1)
	if err != nil {
		return nil, nil, err
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".py") {
			continue
		}
		info, _, err := readCodemodeSnippet(root, strings.TrimSuffix(file.Name(), ".py"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping snippet %q: %v", file.Name(), err))
			continue
		}
		entries = append(entries, info)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	sort.Strings(warnings)
	return entries, warnings, nil
}

func (s *codemodeSnippetStore) save(name, code, description string) error {
	if !codemodeSnippetName.MatchString(name) {
		return errors.New("invalid snippet name (use 1-64 letters, digits, _, . or -)")
	}
	if len(code) > codemodeSnippetMax {
		return errors.New("snippet exceeds 256 KiB")
	}
	if len(description) > 1024 {
		return errors.New("snippet description exceeds 1024 bytes")
	}
	root, err := s.open(true)
	if err != nil {
		return err
	}
	defer root.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	info := codemodeSnippetInfo{Name: name, Description: description, CreatedAt: now, UpdatedAt: now}
	old, _, err := readCodemodeSnippet(root, name)
	if err == nil {
		info.CreatedAt = old.CreatedAt
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	header, _ := json.Marshal(info)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := ".tmp-" + hex.EncodeToString(random[:])
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, writeErr := fmt.Fprintf(f, "%s%s\n%s", codemodeSnippetHeader, header, code)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temp, name+".py")
}

func (s *codemodeSnippetStore) read(name string) (string, error) {
	root, err := s.open(false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	_, code, err := readCodemodeSnippet(root, name)
	return code, err
}

func (s *codemodeSnippetStore) delete(name string) error {
	if !codemodeSnippetName.MatchString(name) {
		return errors.New("invalid snippet name")
	}
	root, err := s.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, _, err := readCodemodeSnippet(root, name); err != nil {
		return err
	}
	return root.Remove(name + ".py")
}
