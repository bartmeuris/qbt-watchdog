package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestArrReloadEqualityAndDurableEndpointIdentity(t *testing.T) {
	original := decode(t, enabled(Sonarr, "    categories: [tv]\n"))
	clone := original.Clone()
	if !original.Integrations.Equal(clone.Integrations) {
		t.Fatal("deep clone differs")
	}
	for name, change := range map[string]func(*ArrService){
		"credential": func(s *ArrService) { s.APIKey = "rotated-secret" },
		"categories": func(s *ArrService) { s.Categories[0] = "movies" },
		"mode":       func(s *ArrService) { s.Mode = SearchOnly },
		"timeout":    func(s *ArrService) { s.Timeout *= 2 },
		"enabled":    func(s *ArrService) { s.Enabled = false },
		"endpoint":   func(s *ArrService) { s.URL.Path = "/other/" },
	} {
		t.Run(name, func(t *testing.T) {
			next := original.Clone()
			change(&next.Integrations.Sonarr)
			if original.Integrations.Equal(next.Integrations) {
				t.Fatal("reload change not detected")
			}
			if original.SafetyKey() != next.SafetyKey() || original.EndpointKey() != next.EndpointKey() {
				t.Fatal("Arr change resets qbt state")
			}
			if err := RestartRequiredError(original, next); err != nil {
				t.Fatal(err)
			}
			sameEndpoint := original.Integrations.Sonarr.EndpointKey() == next.Integrations.Sonarr.EndpointKey()
			if sameEndpoint != (name != "endpoint") {
				t.Fatal("durable endpoint fingerprint has wrong scope")
			}
		})
	}
	if original.Integrations.Sonarr.EndpointKey() == original.Integrations.Radarr.EndpointKey() {
		t.Fatal("service identities collide")
	}
}

func TestArrSecretsExcludedFromFormattingAndLogs(t *testing.T) {
	c := decode(t, enabled(Sonarr, ""))
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("configuration", "service", c.Integrations.Sonarr, "integrations", c.Integrations)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("configuration", "service", c.Integrations.Sonarr)
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%v %+v %#v", c.Integrations, c.Integrations, c.Integrations)
	if strings.Contains(string(encoded)+printed+logs.String(), "SECRET_KEY") {
		t.Fatal("Arr credential leaked")
	}
}

func TestDisabledArrStillValidatesCredentialsAndTimeout(t *testing.T) {
	for _, field := range []string{"api_key: 'bad key'", "timeout: -1s", "url: 'http://host?apikey=SECRET'"} {
		_, err := Decode(YAML, []byte(minimal+"integrations:\n  radarr:\n    enabled: false\n    "+field+"\n"))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("disabled malformed service accepted or leaked")
		}
	}
}
