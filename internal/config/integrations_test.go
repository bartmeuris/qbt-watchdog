package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// enabled renders a complete, valid service block so each test can vary one
// setting at a time.
func enabled(kind ArrKind, extra string) string {
	return minimal + "integrations:\n  " + string(kind) + ":\n    enabled: true\n" +
		"    url: http://" + string(kind) + ":8989\n    api_key: SECRET_KEY\n" + extra
}

func TestIntegrationsDefaultToDisabled(t *testing.T) {
	c := decode(t, minimal)
	for _, service := range c.Integrations.Services() {
		if service.Enabled || service.URL != nil || service.APIKey != "" {
			t.Fatal("an unmentioned integration is not disabled", service.Kind)
		}
		if service.Mode != BlocklistAndSearch || service.Timeout != 10*time.Second {
			t.Fatal("missing defaults for", service.Kind, service.Mode, service.Timeout)
		}
		if len(service.Categories) != 0 || !service.Handles("anything") {
			t.Fatal("an empty category list must mean every category")
		}
	}
	if c.Integrations.Sonarr.Kind != Sonarr || c.Integrations.Radarr.Kind != Radarr {
		t.Fatal("services do not know their own kind")
	}
	if c.Integrations.Any() || len(c.Integrations.Active()) != 0 {
		t.Fatal("cleanup must see no integrations by default")
	}
}

func TestIntegrationsParseACompleteBlock(t *testing.T) {
	c := decode(t, minimal+`
integrations:
  sonarr:
    enabled: true
    url: "https://media.example/sonarr"
    api_key: "sonarr-key"
    categories: ["tv", " tv-hd ", "", "TV"]
    mode: blocklist_and_search
    timeout: 25s
  radarr:
    enabled: true
    url: "http://radarr:7878/"
    api_key: "radarr-key"
    mode: search_only
`)
	sonarr := c.Integrations.Sonarr
	if sonarr.URL.String() != "https://media.example/sonarr/" {
		t.Fatal("reverse-proxy base path was not preserved", sonarr.URL)
	}
	if sonarr.APIKey != "sonarr-key" || sonarr.Timeout != 25*time.Second || !sonarr.Mode.Blocklists() {
		t.Fatal("service settings were not parsed", sonarr)
	}
	// Trimming is whitespace only: qBittorrent category names are case
	// sensitive, so "TV" and "tv" are different categories.
	if fmt.Sprint(sonarr.Categories) != "[tv tv-hd TV]" {
		t.Fatal("categories were not trimmed case sensitively", sonarr.Categories)
	}
	if !sonarr.Handles("tv") || !sonarr.Handles("TV") || sonarr.Handles("Tv") || sonarr.Handles("movies") {
		t.Fatal("category routing is not case sensitive")
	}
	radarr := c.Integrations.Radarr
	if radarr.URL.String() != "http://radarr:7878/" || radarr.Mode != SearchOnly || radarr.Mode.Blocklists() {
		t.Fatal("radarr settings were not parsed", radarr)
	}
	if !radarr.Handles("anything") {
		t.Fatal("radarr without categories must accept every category")
	}
	if len(c.Integrations.Active()) != 2 || !c.Integrations.Any() {
		t.Fatal("both services should be active")
	}
	if c.Integrations.Active()[0].Kind != Sonarr || c.Integrations.Active()[1].Kind != Radarr {
		t.Fatal("active services are not in a stable order")
	}
}

func TestIntegrationsValidationMatrix(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("file-key\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"unknown mode":            enabled(Sonarr, "    mode: delete_everything\n"),
		"empty mode":              enabled(Sonarr, "    mode: ''\n"),
		"inherit mode":            enabled(Sonarr, "    mode: inherit\n"),
		"none mode":               enabled(Sonarr, "    mode: none\n"),
		"unknown key":             enabled(Sonarr, "    verify_ssl: false\n"),
		"unknown service":         minimal + "integrations:\n  lidarr:\n    enabled: true\n",
		"unknown section":         minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://a\n    api_key: k\n    extra:\n      a: 1\n",
		"wrong type":              minimal + "integrations:\n  sonarr:\n    enabled: yes-please\n",
		"categories wrong type":   enabled(Sonarr, "    categories: tv\n"),
		"both credentials":        enabled(Sonarr, "    api_key_file: "+keyFile+"\n"),
		"no credential":           minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://sonarr\n",
		"no url":                  minimal + "integrations:\n  sonarr:\n    enabled: true\n    api_key: SECRET_KEY\n",
		"empty url":               minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: ''\n    api_key: SECRET_KEY\n",
		"missing key file":        minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://s\n    api_key_file: /missing/SECRET_PATH\n",
		"scheme":                  minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: ftp://sonarr\n    api_key: SECRET_KEY\n",
		"no host":                 minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'http://'\n    api_key: SECRET_KEY\n",
		"not a url":               minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'sonarr:8989'\n    api_key: SECRET_KEY\n",
		"credentials in url":      minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'http://user:SECRET_KEY@sonarr'\n    api_key: SECRET_KEY\n",
		"query in url":            minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'http://sonarr?apikey=SECRET_KEY'\n    api_key: SECRET_KEY\n",
		"fragment in url":         minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'http://sonarr#SECRET_KEY'\n    api_key: SECRET_KEY\n",
		"port":                    minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: 'http://sonarr:99999'\n    api_key: SECRET_KEY\n",
		"timeout not a duration":  enabled(Sonarr, "    timeout: 10\n"),
		"timeout zero":            enabled(Sonarr, "    timeout: 0s\n"),
		"timeout negative":        enabled(Sonarr, "    timeout: -5s\n"),
		"timeout too large":       enabled(Sonarr, "    timeout: 6m\n"),
		"key with whitespace":     minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://s\n    api_key: 'SECRET_KEY '\n",
		"key with newline":        minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://s\n    api_key: \"SECRET_KEY\\nX\"\n",
		"key non ascii":           minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://s\n    api_key: SECRET_KEYé\n",
		"radarr unknown mode":     enabled(Radarr, "    mode: blocklist\n"),
		"disabled but malformed":  minimal + "integrations:\n  sonarr:\n    url: 'not a url'\n",
		"disabled but bad mode":   minimal + "integrations:\n  sonarr:\n    mode: nonsense\n",
		"disabled both key forms": minimal + "integrations:\n  sonarr:\n    api_key: SECRET_KEY\n    api_key_file: " + keyFile + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(YAML, []byte(body))
			if err == nil {
				t.Fatal("invalid integration configuration was accepted")
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "file-key") {
				t.Fatal("validation error leaked a credential", err)
			}
		})
	}
}

// A disabled service may be left blank; only the presence of a URL and a
// credential is conditional on being enabled.
func TestDisabledServiceMayBeAStub(t *testing.T) {
	for name, body := range map[string]string{
		"empty block":  minimal + "integrations:\n  sonarr: {}\n",
		"null block":   minimal + "integrations:\n  sonarr:\n",
		"null section": minimal + "integrations:\n",
		"explicit off": minimal + "integrations:\n  sonarr:\n    enabled: false\n    mode: search_only\n    timeout: 30s\n",
		"url only":     minimal + "integrations:\n  radarr:\n    url: http://radarr:7878\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := decode(t, body)
			if c.Integrations.Any() {
				t.Fatal("a stub must not activate an integration")
			}
		})
	}
}

func TestIntegrationCredentialSources(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("file-key\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body := minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://s\n    api_key_file: " + keyFile + "\n"
	c := decode(t, body)
	if c.Integrations.Sonarr.APIKey != "file-key" {
		t.Fatal("api_key_file was not read or not trimmed", c.Integrations.Sonarr.APIKey)
	}
	// A rotated secret file must change the parsed key without the
	// configuration file itself changing.
	if err := os.WriteFile(keyFile, []byte("rotated-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if decode(t, body).Integrations.Sonarr.APIKey != "rotated-key" {
		t.Fatal("a rotated secret file was not re-read")
	}
	for _, contents := range []string{"", "\r\n", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(keyFile, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(YAML, []byte(body)); err == nil {
			t.Fatal("an unusable secret file silently enabled an integration")
		}
	}
}

func TestIntegrationEnvironmentExpansion(t *testing.T) {
	body := minimal + `
integrations:
  sonarr:
    enabled: true
    url: "http://${ARR_HOST}:8989/${ARR_BASE}"
    api_key: "${SONARR_API_KEY}"
    categories: ["${ARR_CATEGORY}"]
`
	environment := map[string]string{
		"ARR_HOST": "sonarr.internal", "ARR_BASE": "sonarr",
		"SONARR_API_KEY": "expanded-key", "ARR_CATEGORY": "tv",
	}
	c, err := DecodeWithEnvironment(YAML, []byte(body), environment)
	if err != nil {
		t.Fatal(err)
	}
	if c.Integrations.Sonarr.APIKey != "expanded-key" {
		t.Fatal("nested credential was not expanded", c.Integrations.Sonarr.APIKey)
	}
	if c.Integrations.Sonarr.URL.String() != "http://sonarr.internal:8989/sonarr/" {
		t.Fatal("nested URL was not expanded", c.Integrations.Sonarr.URL)
	}
	if fmt.Sprint(c.Integrations.Sonarr.Categories) != "[tv]" {
		t.Fatal("nested list was not expanded", c.Integrations.Sonarr.Categories)
	}
	// An unset reference must reject the candidate rather than
	// authenticate with a literal ${NAME}.
	delete(environment, "SONARR_API_KEY")
	if _, err = DecodeWithEnvironment(YAML, []byte(body), environment); err == nil {
		t.Fatal("an unset nested reference was accepted")
	}
	// An empty reference must not silently select a different credential
	// source, at any depth.
	environment["SONARR_API_KEY"] = ""
	_, err = DecodeWithEnvironment(YAML, []byte(body), environment)
	if err == nil || !strings.Contains(err.Error(), "nonempty") {
		t.Fatal("an empty nested credential reference was accepted", err)
	}
}

func TestIntegrationsAreAppliedLiveAndNeverResetPolicyTimers(t *testing.T) {
	base := decode(t, minimal)
	changed := decode(t, enabled(Sonarr, "    timeout: 20s\n"))
	if RestartRequiredError(base, changed) != nil {
		t.Fatal("integration changes must apply live")
	}
	if len(RestartRequiredChanges(base, changed)) != 0 {
		t.Fatal("integrations must not be restart-only")
	}
	// Integration settings can only ever add a recovery step; they can
	// never shorten a cleanup clock, so they must not reset the timers.
	if base.SafetyKey() != changed.SafetyKey() || base.EndpointKey() != changed.EndpointKey() {
		t.Fatal("an integration change reset the cleanup safety state")
	}
	if base.IntegrationsKey() == changed.IntegrationsKey() {
		t.Fatal("an integration change must be visible to durable recovery markers")
	}
}

// Repointing an integration must invalidate durable recovery markers, and the
// digest that does so must never carry the credential itself.
func TestIntegrationsKeyTracksIdentityWithoutLeakingIt(t *testing.T) {
	original := decode(t, enabled(Sonarr, ""))
	for name, body := range map[string]string{
		"rotated key": enabled(Sonarr, "") + "    \n",
		"other host":  minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://other:8989\n    api_key: SECRET_KEY\n",
		"other key":   minimal + "integrations:\n  sonarr:\n    enabled: true\n    url: http://sonarr:8989\n    api_key: SECRET_ROTATED\n",
		"other mode":  enabled(Sonarr, "    mode: search_only\n"),
		"categories":  enabled(Sonarr, "    categories: [tv]\n"),
		"timeout":     enabled(Sonarr, "    timeout: 30s\n"),
		"disabled":    minimal + "integrations:\n  sonarr:\n    enabled: false\n    url: http://sonarr:8989\n    api_key: SECRET_KEY\n",
	} {
		t.Run(name, func(t *testing.T) {
			candidate := decode(t, body)
			same := candidate.IntegrationsKey() == original.IntegrationsKey()
			if (name == "rotated key" || name == "other key") && !same {
				t.Fatal("an identical configuration produced a different digest")
			}
			if name != "rotated key" && name != "other key" && same {
				t.Fatal("a repointed integration kept its digest")
			}
			if len(candidate.IntegrationsKey()) != 64 || strings.Contains(candidate.IntegrationsKey(), "SECRET") {
				t.Fatal("the digest is not a secret-free hash")
			}
		})
	}
}

func TestIntegrationKeysNeverReachTheStatusPayload(t *testing.T) {
	c := decode(t, enabled(Sonarr, ""))
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET_KEY") {
		t.Fatal("marshalling the configuration leaked an integration key")
	}
	for _, warning := range c.Warnings() {
		if strings.Contains(warning.Message, "SECRET") {
			t.Fatal("a warning leaked an integration key")
		}
	}
}

// A clone must not share the URL or the category slice, or a caller mutating
// its snapshot would reach into another goroutine's configuration.
func TestIntegrationsCloneIsDeep(t *testing.T) {
	c := decode(t, enabled(Sonarr, "    categories: [tv]\n"))
	clone := c.Clone()
	clone.Integrations.Sonarr.URL.Path = "/moved/"
	clone.Integrations.Sonarr.Categories[0] = "movies"
	if c.Integrations.Sonarr.URL.Path != "/" {
		t.Fatal("clone shares its URL")
	}
	if c.Integrations.Sonarr.Categories[0] != "tv" {
		t.Fatal("clone shares its category list")
	}
}

func TestIntegrationsInTOML(t *testing.T) {
	c, err := Decode(TOML, []byte(`qbt_url = "http://host"
[integrations.sonarr]
enabled = true
url = "http://sonarr:8989"
api_key = "toml-key"
mode = "search_only"
timeout = "15s"
categories = ["tv"]
`))
	if err != nil {
		t.Fatal(err)
	}
	service := c.Integrations.Sonarr
	if !service.Enabled || service.APIKey != "toml-key" || service.Mode != SearchOnly || service.Timeout != 15*time.Second {
		t.Fatal("TOML integrations were not parsed identically", service)
	}
}

func TestArrModeAndKindVocabulary(t *testing.T) {
	if len(ArrModes()) != 3 || !BlocklistAndSearch.Valid() || !BlocklistOnly.Valid() || !SearchOnly.Valid() {
		t.Fatal("unexpected mode vocabulary")
	}
	if len(PolicyArrModes()) != 5 || !InheritArrMode.ValidForPolicy() || !NoArrMode.ValidForPolicy() || !BlocklistAndSearch.ValidForPolicy() || !BlocklistOnly.ValidForPolicy() || !SearchOnly.ValidForPolicy() {
		t.Fatal("unexpected policy mode vocabulary")
	}
	if ArrMode("").Valid() || ArrMode("BLOCKLIST_AND_SEARCH").Valid() {
		t.Fatal("mode matching must be exact")
	}
	if InheritArrMode.Valid() || NoArrMode.Valid() {
		t.Fatal("policy-only modes must not be valid service modes")
	}
	if !BlocklistAndSearch.Blocklists() || !BlocklistOnly.Blocklists() || SearchOnly.Blocklists() {
		t.Fatal("blocklist modes must be exactly blocklist_and_search and blocklist_only")
	}
	if !BlocklistAndSearch.Searches() || !SearchOnly.Searches() || BlocklistOnly.Searches() {
		t.Fatal("search modes must be exactly blocklist_and_search and search_only")
	}
	if string(Sonarr) != "sonarr" || string(Radarr) != "radarr" {
		t.Fatal("kind names are part of the configuration surface")
	}
}
