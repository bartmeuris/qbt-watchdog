package config

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

const maxDotEnvBytes = 1024 * 1024

func readDotEnv(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		// A dangling symlink is a broken source, not an absent optional file.
		if _, linkErr := os.Lstat(path); errors.Is(linkErr, os.ErrNotExist) {
			return map[string]string{}, nil
		}
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDotEnvBytes {
		return nil, errors.New("dotenv file unreadable or too large")
	}
	data, err := readBounded(path, maxDotEnvBytes)
	if err != nil {
		return nil, errors.New("dotenv file unreadable or too large")
	}
	return parseDotEnv(data)
}

// parseDotEnv is deliberately literal, not a shell or interpolation language.
// One assignment per line; optional export, spaces around =, CRLF, comments,
// and single/double quotes are supported. Duplicate names use the last value.
// Single quotes are fully literal. Double quotes decode only \n, \r, \t, \\,
// \" and \$; other escapes, multiline quotes and trailing tokens are errors.
// Unquoted # begins a comment only at the start or after horizontal whitespace.
// No form expands $NAME, ${NAME}, $$, backticks or command substitutions.
func parseDotEnv(data []byte) (map[string]string, error) {
	invalid := errors.New("invalid dotenv syntax")
	if len(data) > maxDotEnvBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, invalid
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.Trim(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[len("export"):], " \t")
		}
		name, raw, ok := strings.Cut(line, "=")
		name = strings.Trim(name, " \t")
		if !ok || !validEnvironmentName(name) {
			return nil, invalid
		}
		value, err := parseDotEnvValue(strings.Trim(raw, " \t"))
		if err != nil {
			return nil, invalid
		}
		values[name] = value
	}
	return values, nil
}

func parseDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '\'' || raw[0] == '"' {
		return parseQuotedDotEnvValue(raw)
	}
	for i := range len(raw) {
		if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimRight(raw[:i], " \t"), nil
		}
		if raw[i] == '\'' || raw[i] == '"' || raw[i] == '\r' {
			return "", errors.New("invalid dotenv syntax")
		}
	}
	return raw, nil
}

func parseQuotedDotEnvValue(raw string) (string, error) {
	var value strings.Builder
	quote := raw[0]
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if ch == quote {
			rest := strings.TrimLeft(raw[i+1:], " \t")
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", errors.New("invalid dotenv syntax")
			}
			return value.String(), nil
		}
		if ch == '\r' {
			return "", errors.New("invalid dotenv syntax")
		}
		if ch != '\\' || quote == '\'' {
			value.WriteByte(ch)
			continue
		}
		i++
		if i == len(raw) {
			break
		}
		switch raw[i] {
		case 'n':
			value.WriteByte('\n')
		case 'r':
			value.WriteByte('\r')
		case 't':
			value.WriteByte('\t')
		case '\\', '"', '$':
			value.WriteByte(raw[i])
		default:
			return "", errors.New("invalid dotenv syntax")
		}
	}
	return "", errors.New("invalid dotenv syntax")
}
