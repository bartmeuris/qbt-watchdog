package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// effectiveEnvironment merges the process snapshot over the adjacent .env
// fallback, with process values (including empty ones) taking precedence. It is
// the single definition of the environment the loader sees, shared by Load and
// the editor's environment metadata.
func effectiveEnvironment(path string, process map[string]string) (map[string]string, error) {
	environment, err := readDotEnv(filepath.Join(filepath.Dir(path), ".env"))
	if err != nil {
		return nil, err
	}
	for name, value := range process {
		environment[name] = value
	}
	return environment, nil
}

func environmentSnapshot(entries []string) map[string]string {
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name != "" {
			snapshot[name] = value
		}
	}
	return snapshot
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		ch := name[i]
		if ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return false
	}
	return true
}

// expandString scans only the original string. Substituted values are opaque:
// even ${OTHER}, $$, backticks and shell expressions in a secret stay literal.
// A dollar not followed by { or $ is also literal, preserving existing secrets.
func expandString(value string, environment map[string]string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 == len(value) || value[i+1] != '$' && value[i+1] != '{' {
			result.WriteByte(value[i])
			i++
			continue
		}
		if value[i+1] == '$' {
			result.WriteByte('$')
			i += 2
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 || !validEnvironmentName(value[i+2:i+2+end]) {
			return "", errors.New("malformed environment reference in configuration")
		}
		replacement, ok := environment[value[i+2:i+2+end]]
		if !ok {
			return "", errors.New("configuration references an unset environment variable")
		}
		if !utf8.ValidString(replacement) || strings.ContainsRune(replacement, 0) {
			return "", errors.New("invalid environment value")
		}
		result.WriteString(replacement)
		i += end + 3
	}
	return result.String(), nil
}

// expandValues accepts the JSON-shaped tree produced after YAML/TOML parsing.
// Keys and non-string scalars are never rewritten. New nested configuration
// sections and lists share this hook without needing field-specific expansion.
func expandValues(value any, environment map[string]string) (any, error) {
	switch value := value.(type) {
	case string:
		return expandString(value, environment)
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, entry := range value {
			expanded, err := expandValues(entry, environment)
			if err != nil {
				return nil, err
			}
			result[key] = expanded
		}
		return result, nil
	case []any:
		result := make([]any, len(value))
		for i, entry := range value {
			expanded, err := expandValues(entry, environment)
			if err != nil {
				return nil, err
			}
			result[i] = expanded
		}
		return result, nil
	default:
		return value, nil
	}
}

// credentialKeys are the settings where an empty expansion result would
// silently change which authentication source is used. The names are matched
// at every depth, so a nested integration credential is protected by the same
// rule as a top-level one.
var credentialKeys = map[string]bool{
	"qbt_username": true, "qbt_password": true, "qbt_password_file": true,
	"qbt_api_key": true, "qbt_api_key_file": true,
	"api_key": true, "api_key_file": true,
}

// guardCredentials walks the parsed tree beside its expanded copy. Literal
// empty credentials retain the existing authentication-bypass mode, but an
// empty *reference* must not silently disable authentication or select a
// different source.
func guardCredentials(original, resolved map[string]any) error {
	for key, before := range original {
		if nested, isSection := before.(map[string]any); isSection {
			expanded, stillSection := resolved[key].(map[string]any)
			if !stillSection {
				continue
			}
			if err := guardCredentials(nested, expanded); err != nil {
				return err
			}
			continue
		}
		if !credentialKeys[key] {
			continue
		}
		text, wasString := before.(string)
		after, isString := resolved[key].(string)
		if wasString && isString && text != "" && after == "" {
			return errors.New("credential reference must resolve to a nonempty value")
		}
	}
	return nil
}

func expandSettings(settings []byte, environment map[string]string) ([]byte, error) {
	var original map[string]any
	decoder := json.NewDecoder(bytes.NewReader(settings))
	decoder.UseNumber()
	if err := decoder.Decode(&original); err != nil {
		return nil, errors.New("invalid configuration values")
	}
	expanded, err := expandValues(original, environment)
	if err != nil {
		return nil, err
	}
	resolved := expanded.(map[string]any)
	if err = guardCredentials(original, resolved); err != nil {
		return nil, err
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		return nil, errors.New("invalid configuration values")
	}
	return data, nil
}
