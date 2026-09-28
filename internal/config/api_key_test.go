package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyConfiguration(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(secretFile, []byte("test-api-key\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []Format{YAML, TOML} {
		for _, source := range []string{"qbt_api_key", "qbt_api_key_file"} {
			t.Run(string(format)+"/"+source, func(t *testing.T) {
				value := "test-api-key"
				if source == "qbt_api_key_file" {
					value = secretFile
				}
				separator := ": "
				if format == TOML {
					separator = " = "
				}
				data := "qbt_url" + separator + "\"http://localhost\"\n" + source + separator + fmt.Sprintf("%q\n", value)
				c, err := Decode(format, []byte(data))
				if err != nil || c.APIKey != "test-api-key" || c.Username != "" || c.Password != "" {
					t.Fatal("API key not resolved correctly", err)
				}
				encoded, err := json.Marshal(c)
				if err != nil || bytes.Contains(encoded, []byte(c.APIKey)) {
					t.Fatal("configuration JSON leaked API key", err)
				}
			})
		}
	}
	for name, extra := range map[string]string{
		"both sources":          "qbt_api_key: SECRET\nqbt_api_key_file: SECRET_PATH\n",
		"username":              "qbt_api_key: SECRET\nqbt_username: user\n",
		"password":              "qbt_api_key: SECRET\nqbt_password: SECRET_PASSWORD\n",
		"password file":         "qbt_api_key: SECRET\nqbt_password_file: SECRET_PATH\n",
		"key file and username": "qbt_api_key_file: SECRET_PATH\nqbt_username: user\n",
		"key file and password": "qbt_api_key_file: SECRET_PATH\nqbt_password: SECRET_PASSWORD\n",
		"both files":            "qbt_api_key_file: SECRET_PATH\nqbt_password_file: SECRET_PASSWORD_PATH\n",
		"missing file":          "qbt_api_key_file: /missing/SECRET_PATH\n",
		"wrong type":            "qbt_api_key: [SECRET]\n",
		"newline":               "qbt_api_key: \"SECRET\\nINJECTED\"\n",
		"space":                 "qbt_api_key: ' SECRET '\n",
		"tab":                   "qbt_api_key: \"SECRET\\t\"\n",
		"non ASCII":             "qbt_api_key: SECRETé\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(YAML, []byte("qbt_url: http://localhost\n"+extra))
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid authentication was accepted or leaked", err)
			}
		})
	}
	for _, contents := range []string{"", "\r\n", " \n", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(secretFile, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Decode(YAML, []byte(fmt.Sprintf("qbt_url: http://localhost\nqbt_api_key_file: %q\n", secretFile)))
		if err == nil {
			t.Fatal("invalid secret file silently enabled bypass")
		}
	}
	c, err := Decode(YAML, []byte("qbt_url: http://localhost\nqbt_api_key: ''\nqbt_api_key_file: ''\n"))
	if err != nil || c.APIKey != "" || c.Username != "" || c.Password != "" {
		t.Fatal("empty credentials did not retain bypass", err)
	}
}

func TestCredentialSafetyKey(t *testing.T) {
	c, err := Decode(YAML, []byte("qbt_url: http://localhost\nqbt_api_key: SECRET_ORIGINAL\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, credentials := range [][3]string{{"", "", "SECRET_ROTATED"}, {"user", "SECRET_PASSWORD", ""}, {"", "", ""}} {
		next := c.Clone()
		next.Username, next.Password, next.APIKey = credentials[0], credentials[1], credentials[2]
		if next.SafetyKey() == c.SafetyKey() || next.EndpointKey() != c.EndpointKey() {
			t.Fatal("credential change must reset timers but preserve endpoint identity")
		}
		if len(next.SafetyKey()) != 64 || strings.Contains(next.SafetyKey()+next.EndpointKey(), "SECRET") {
			t.Fatal("persistent keys are not secret-free digests")
		}
	}
	var help bytes.Buffer
	_, err = Parse([]string{"--help"}, func(string) (string, bool) { return "SECRET", true }, &help)
	if !errors.Is(err, flag.ErrHelp) || strings.Contains(help.String(), "SECRET") {
		t.Fatal("help leaked credentials", err)
	}
}

func TestAPIKeyFileRotationWithoutConfigChange(t *testing.T) {
	dir := t.TempDir()
	keyFile, configFile := filepath.Join(dir, "key"), filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(keyFile, []byte("original-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("qbt_url: http://localhost\nqbt_api_key_file: %q\n", keyFile)
	if err := os.WriteFile(configFile, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	initial, err := Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(configFile, initial)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan error, 1)
	go func() {
		done <- m.Run(ctx, nil, func(err error) {
			select {
			case ready <- err:
			default:
			}
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Directory registration can enumerate entries on macOS. Do not remove
	// a temporary entry mid-registration; first establish an active watcher.
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initial watch load timed out")
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("rotated-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, keyFile); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for m.Current().APIKey != "rotated-key" {
		select {
		case <-deadline:
			t.Fatal("secret-only rotation was not detected")
		case <-ticker.C:
		}
	}
	if m.Status().Generation != 2 || m.Current().SafetyKey() == initial.SafetyKey() {
		t.Fatal("rotation did not publish new safety snapshot")
	}
	if err := os.WriteFile(keyFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configFile); err == nil || m.Current().APIKey != "rotated-key" {
		t.Fatal("empty rotation replaced last known good credentials")
	}
}
