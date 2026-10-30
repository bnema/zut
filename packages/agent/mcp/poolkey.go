package mcp

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"path/filepath"
	"slices"
)

// poolKey identifies one shareable stdio server process: two sessions whose
// configs produce the same key may use the same process.
type poolKey [32]byte

// sharedKey hashes everything that makes a stdio process observably
// different: transport kind, resolved absolute command, arguments, built
// child environment (sorted), resolved process directory and the roots cwd
// the server will be told about. rootsCWD is the session cwd when cfg.CWD is
// empty or relative (the process depends on the session), and "" when cfg.CWD
// is absolute (the process is session-independent). Name, required, tool
// filters, timeouts and config source are excluded: they are per-session
// views over the same process.
func sharedKey(cfg ServerConfig, sessionCWD string) (key poolKey, rootsCWD string, err error) {
	if kind := cfg.Kind(); kind != "stdio" {
		return key, "", fmt.Errorf("shared key: %s servers are not shareable", kind)
	}
	launch, err := resolveStdioLaunch(cfg, sessionCWD)
	if err != nil {
		return key, "", err
	}
	command, err := filepath.Abs(launch.Command)
	if err != nil {
		return key, "", fmt.Errorf("shared key: resolve command: %w", err)
	}
	dir, err := filepath.Abs(launch.Dir)
	if err != nil {
		return key, "", fmt.Errorf("shared key: resolve dir: %w", err)
	}
	if cfg.CWD == "" || !filepath.IsAbs(cfg.CWD) {
		rootsCWD = sessionCWD
	}
	env := slices.Clone(launch.Env)
	slices.Sort(env)

	h := sha256.New()
	writeField(h, "stdio")
	writeField(h, command)
	writeList(h, launch.Args)
	writeList(h, env)
	writeField(h, dir)
	writeField(h, rootsCWD)
	copy(key[:], h.Sum(nil))
	return key, rootsCWD, nil
}

// writeField writes a length-prefixed string so field boundaries are
// unambiguous ("a","bc" never collides with "ab","c").
func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// writeList writes a count followed by each length-prefixed element.
func writeList(h hash.Hash, items []string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(items)))
	h.Write(n[:])
	for _, s := range items {
		writeField(h, s)
	}
}
