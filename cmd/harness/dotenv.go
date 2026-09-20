package main

import (
	"bufio"
	"os"
	"strings"
)

// loadDotenv reads KEY=VALUE lines from path into the process environment
// without overriding variables that are already set. Missing file is fine.
// Supports comments, blank lines, an optional `export ` prefix, and single or
// double quotes around the value.
func loadDotenv(path string) (loaded []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i]) // trailing comment on an unquoted value
		}
		if key == "" || val == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return loaded, err
		}
		loaded = append(loaded, key)
	}
	return loaded, sc.Err()
}
