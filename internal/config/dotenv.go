// Package config implements layered configuration for the command line tool.
//
// Precedence, highest first:
//
//  1. command line flags
//  2. real environment variables (KUBE_EMULATOR_*)
//  3. the .env file
//  4. the built-in defaults (the flags' default values)
package config

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ParseDotenv parses KEY=VALUE lines. It supports blank lines, `#` comments
// (also after an unquoted value), an optional `export ` prefix, double-quoted
// values (with \n, \t, \", \\ escapes), single-quoted literal values and
// Windows (CRLF) line endings.
func ParseDotenv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // UTF-8 BOM
		}
		line = strings.TrimSpace(line) // also removes a trailing \r
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val, err := parseValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", lineNo, key, err)
		}
		out[key] = val
	}
	return out, sc.Err()
}

func parseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			switch c := raw[i]; c {
			case '\\':
				if i+1 >= len(raw) {
					return "", fmt.Errorf("unterminated escape")
				}
				i++
				switch raw[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(raw[i])
				}
			case '"':
				return b.String(), nil
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("unterminated double quote")
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single quote")
		}
		return raw[1 : 1+end], nil
	}
	if i := strings.Index(raw, " #"); i >= 0 { // inline comment
		raw = raw[:i]
	}
	return strings.TrimSpace(raw), nil
}

// LoadFile reads and parses a .env file. A missing file yields an error that
// satisfies errors.Is(err, fs.ErrNotExist).
func LoadFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := ParseDotenv(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Apply fills flags from the environment and the .env file. keys maps a flag
// name to its variable name. Flags listed in skip (those given on the command
// line) are never touched, which is what gives the command line precedence.
// For the others a real environment variable beats the .env file. It returns
// a human readable list of what was applied.
func Apply(fs *flag.FlagSet, keys map[string]string, dotenv map[string]string, skip map[string]bool) ([]string, error) {
	var applied []string
	for name, key := range keys {
		if skip[name] || fs.Lookup(name) == nil {
			continue
		}
		val, source := "", ""
		if v, ok := os.LookupEnv(key); ok {
			val, source = v, "environment"
		} else if v, ok := dotenv[key]; ok {
			val, source = v, ".env"
		} else {
			continue
		}
		if err := fs.Set(name, val); err != nil {
			return applied, fmt.Errorf("%s=%q (from %s) is not valid for --%s: %w", key, val, source, name, err)
		}
		applied = append(applied, fmt.Sprintf("--%s=%s (from %s via %s)", name, val, source, key))
	}
	sort.Strings(applied)
	return applied, nil
}
