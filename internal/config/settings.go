package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/viper"
)

// SourceKind describes where an editable setting's value comes from. It is the
// source/default/inheritance information the structured editor renders.
type SourceKind string

const (
	// SourceExplicit means the document writes the value.
	SourceExplicit SourceKind = "explicit"
	// SourceDefault means the document omits the value and the built-in
	// default applies.
	SourceDefault SourceKind = "default"
	// SourceInherited means the value is not written and is resolved from a
	// broader scope (a policy arr_mode inheriting the integration mode).
	SourceInherited SourceKind = "inherited"
)

// Setting is one editable leaf. Value is the source value exactly as written
// (before ${ENV} expansion); Effective is the resolved non-secret value when
// the document decodes. A Setting never carries a resolved secret.
type Setting struct {
	Value     any        `json:"value"`
	Effective any        `json:"effective,omitempty"`
	Source    SourceKind `json:"source"`
	Error     string     `json:"error,omitempty"`
}

// FieldError is a validation problem attributed to one setting path.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// GeneralSettings is the General section of the structured editor.
type GeneralSettings struct {
	DryRun            Setting `json:"dry_run"`
	MaxActionsPerPoll Setting `json:"max_actions_per_poll"`
	PollInterval      Setting `json:"poll_interval"`
	UIRefreshInterval Setting `json:"ui_refresh_interval"`
	HistoryLimit      Setting `json:"history_limit"`
}

// PolicySettings is the editable subset of one cleanup policy.
type PolicySettings struct {
	Action           Setting `json:"action"`
	ThresholdSeconds Setting `json:"threshold_seconds"`
	ArrMode          Setting `json:"arr_mode"`
	MatchTags        Setting `json:"match_tags"`
}

// ArrConnectionSettings is the editable subset of one media-manager endpoint.
type ArrConnectionSettings struct {
	Enabled    Setting `json:"enabled"`
	URL        Setting `json:"url"`
	Mode       Setting `json:"mode"`
	Timeout    Setting `json:"timeout"`
	Categories Setting `json:"categories"`
}

// ConnectionSettings is the Connections section of the structured editor.
type ConnectionSettings struct {
	QBTURL      Setting                          `json:"qbt_url"`
	QBTUsername Setting                          `json:"qbt_username"`
	QBTAuthMode Setting                          `json:"qbt_auth_mode"`
	Arr         map[string]ArrConnectionSettings `json:"arr"`
}

// ScopeSettings is the Scope section of the structured editor.
type ScopeSettings struct {
	IncludeCategories Setting `json:"include_categories"`
	ExcludeCategories Setting `json:"exclude_categories"`
	ExcludeTags       Setting `json:"exclude_tags"`
}

// AdvancedSettings is the Advanced section of the structured editor.
type AdvancedSettings struct {
	MaxObservationGap         Setting `json:"max_observation_gap"`
	DeleteConfirmationTimeout Setting `json:"delete_confirmation_timeout"`
	HTTPTimeout               Setting `json:"http_timeout"`
	ReadinessMaxAge           Setting `json:"readiness_max_age"`
	TagSyncEnabled            Setting `json:"tag_sync_enabled"`
	TagSyncPrefix             Setting `json:"tag_sync_prefix"`
	TagSyncMaxWritesPerPoll   Setting `json:"tag_sync_max_writes_per_poll"`
	TLSInsecure               Setting `json:"tls_insecure_skip_verify"`
	TLSCAFile                 Setting `json:"tls_ca_file"`
	LogLevel                  Setting `json:"log_level"`
	LogFormat                 Setting `json:"log_format"`
	LogColor                  Setting `json:"log_color"`
	StateFile                 Setting `json:"state_file"`
	Listen                    Setting `json:"listen"`
}

// SecretSource is the inferred origin of a secret, never its value.
type SecretSource string

const (
	SecretUnset SecretSource = "unset"
	SecretValue SecretSource = "value"
	SecretEnv   SecretSource = "env"
	SecretPath  SecretSource = "path"
)

// SecretSetting reports where a secret comes from without ever returning the
// secret itself. EnvName and Path are metadata, not credentials.
type SecretSetting struct {
	Key        string       `json:"key"`
	Source     SecretSource `json:"source"`
	Configured bool         `json:"configured"`
	EnvName    string       `json:"env_name,omitempty"`
	Path       string       `json:"path,omitempty"`
	Error      string       `json:"error,omitempty"`
}

// EnvVar is the availability metadata for one environment variable name. It
// never carries a value.
type EnvVar struct {
	Name       string `json:"name"`
	Available  bool   `json:"available"`
	Empty      bool   `json:"empty"`
	Configured bool   `json:"configured"`
}

// Settings is the typed, source-preserving read model the structured editor
// consumes. It is built from the raw document, so it remains readable even when
// a referenced environment variable is missing and Decode fails.
type Settings struct {
	Stamp       string                    `json:"stamp"`
	Format      Format                    `json:"format"`
	General     GeneralSettings           `json:"general"`
	Policies    map[string]PolicySettings `json:"policies"`
	Connections ConnectionSettings        `json:"connections"`
	Scope       ScopeSettings             `json:"scope"`
	Advanced    AdvancedSettings          `json:"advanced"`
	Secrets     []SecretSetting           `json:"secrets"`
	Environment []EnvVar                  `json:"environment"`
	Errors      []FieldError              `json:"errors,omitempty"`
}

// Settings reads the raw document and builds the typed model. A decode failure
// is reported as a field error but never prevents the source values from being
// returned, so a broken reference stays visible and repairable.
func (e *Editor) Settings() (Settings, error) {
	raw, stamp, err := e.Read()
	if err != nil {
		return Settings{}, err
	}
	format, err := FormatFor(e.path)
	if err != nil {
		return Settings{}, err
	}
	model := Settings{Stamp: stamp, Format: format}
	tree, parseErr := parseSourceTree(format, raw)
	if parseErr != nil {
		model.Errors = []FieldError{{Field: "config", Message: parseErr.Error()}}
		return model, nil
	}
	process := environmentSnapshot(os.Environ())
	environment, envErr := effectiveEnvironment(e.path, process)
	var effective Config
	hasEffective := false
	if envErr == nil {
		if effective, err = DecodeWithEnvironment(format, raw, environment); err == nil {
			hasEffective = true
		} else {
			model.Errors = append(model.Errors, fieldError(err))
		}
	} else {
		model.Errors = append(model.Errors, fieldError(envErr))
	}
	model.General = generalSettings(tree, effective, hasEffective)
	model.Policies = policySettings(tree, effective, hasEffective)
	model.Connections = connectionSettings(tree, effective, hasEffective)
	model.Scope = scopeSettings(tree, effective, hasEffective)
	model.Advanced = advancedSettings(tree, effective, hasEffective)
	model.Secrets = secretSettings(tree, environment)
	model.Environment = envMetadata(environment, referencedEnvNames(format, raw))
	return model, nil
}

// Environment returns the availability metadata for every environment variable
// the loader can see, plus every variable the document references. It is
// independent of a successful Decode.
func (e *Editor) Environment() ([]EnvVar, error) {
	raw, _, err := e.Read()
	if err != nil {
		return nil, err
	}
	format, err := FormatFor(e.path)
	if err != nil {
		return nil, err
	}
	process := environmentSnapshot(os.Environ())
	environment, err := effectiveEnvironment(e.path, process)
	if err != nil {
		return nil, err
	}
	return envMetadata(environment, referencedEnvNames(format, raw)), nil
}

func generalSettings(tree map[string]any, c Config, ok bool) GeneralSettings {
	return GeneralSettings{
		DryRun:            setting(tree, []string{"dry_run"}, true, c.DryRun, ok),
		MaxActionsPerPoll: setting(tree, []string{"max_actions_per_poll"}, 10, c.MaxDeletions, ok),
		PollInterval:      setting(tree, []string{"poll_interval"}, "30s", durationValue(c.PollInterval, ok), ok),
		UIRefreshInterval: setting(tree, []string{"ui_refresh_interval"}, "5s", durationValue(c.UIRefreshInterval, ok), ok),
		HistoryLimit:      setting(tree, []string{"history_limit"}, 100, c.HistoryLimit, ok),
	}
}

func policySettings(tree map[string]any, c Config, ok bool) map[string]PolicySettings {
	result := make(map[string]PolicySettings, len(PolicyIDs()))
	for _, id := range PolicyIDs() {
		action, threshold, arrMode, matchTags := defaultPolicy(id)
		policy := c.Policies[id]
		path := []string{"policies", string(id)}
		settings := PolicySettings{
			Action:           setting(tree, append(path, "action"), string(action), string(policy.Action), ok),
			ThresholdSeconds: setting(tree, append(path, "threshold"), threshold, int(policy.Threshold.Seconds()), ok),
			ArrMode:          setting(tree, append(path, "arr_mode"), string(arrMode), string(policy.ArrMode), ok),
			MatchTags:        setting(tree, append(path, "match_tags"), matchTags, policy.MatchTags, ok),
		}
		if _, present := lookupPath(tree, append(path, "arr_mode")); !present {
			settings.ArrMode.Source = SourceInherited
		}
		result[string(id)] = settings
	}
	return result
}

func connectionSettings(tree map[string]any, c Config, ok bool) ConnectionSettings {
	settings := ConnectionSettings{
		QBTURL:      setting(tree, []string{"qbt_url"}, "", urlValue(c.URL, ok), ok),
		QBTUsername: setting(tree, []string{"qbt_username"}, "", c.Username, ok),
		QBTAuthMode: authModeSetting(tree, c, ok),
		Arr:         map[string]ArrConnectionSettings{},
	}
	for _, kind := range []ArrKind{Sonarr, Radarr} {
		service := c.Integrations.Sonarr
		if kind == Radarr {
			service = c.Integrations.Radarr
		}
		path := []string{"integrations", string(kind)}
		settings.Arr[string(kind)] = ArrConnectionSettings{
			Enabled:    setting(tree, append(path, "enabled"), false, service.Enabled, ok),
			URL:        setting(tree, append(path, "url"), "", urlValue(service.URL, ok), ok),
			Mode:       setting(tree, append(path, "mode"), string(BlocklistAndSearch), string(service.Mode), ok),
			Timeout:    setting(tree, append(path, "timeout"), "10s", durationValue(service.Timeout, ok), ok),
			Categories: setting(tree, append(path, "categories"), []string{}, service.Categories, ok),
		}
	}
	return settings
}

// authModeSetting derives the qBittorrent authentication mode from which
// credential keys the document writes. It is a view, not a stored key.
func authModeSetting(tree map[string]any, c Config, ok bool) Setting {
	mode := "none"
	source := SourceDefault
	if _, present := lookupPath(tree, []string{"qbt_api_key"}); present {
		mode, source = "api_key", SourceExplicit
	} else if _, present := lookupPath(tree, []string{"qbt_api_key_file"}); present {
		mode, source = "api_key", SourceExplicit
	} else if _, present := lookupPath(tree, []string{"qbt_username"}); present {
		mode, source = "password", SourceExplicit
	} else if _, present := lookupPath(tree, []string{"qbt_password"}); present {
		mode, source = "password", SourceExplicit
	} else if _, present := lookupPath(tree, []string{"qbt_password_file"}); present {
		mode, source = "password", SourceExplicit
	}
	effective := any(nil)
	if ok {
		switch {
		case c.APIKey != "":
			effective = "api_key"
		case c.Username != "" || c.Password != "":
			effective = "password"
		default:
			effective = "none"
		}
	}
	return Setting{Value: mode, Effective: effective, Source: source}
}

func scopeSettings(tree map[string]any, c Config, ok bool) ScopeSettings {
	return ScopeSettings{
		IncludeCategories: setting(tree, []string{"include_categories"}, []string{}, c.IncludeCategories, ok),
		ExcludeCategories: setting(tree, []string{"exclude_categories"}, []string{}, c.ExcludeCategories, ok),
		ExcludeTags:       setting(tree, []string{"exclude_tags"}, []string{"keep", "qbt-watchdog-ignore"}, c.ExcludeTags, ok),
	}
}

func advancedSettings(tree map[string]any, c Config, ok bool) AdvancedSettings {
	return AdvancedSettings{
		MaxObservationGap:         setting(tree, []string{"max_observation_gap"}, nil, durationValue(c.MaxObservationGap, ok), ok),
		DeleteConfirmationTimeout: setting(tree, []string{"delete_confirmation_timeout"}, "2m", durationValue(c.DeleteConfirmationTimeout, ok), ok),
		HTTPTimeout:               setting(tree, []string{"http_timeout"}, "10s", durationValue(c.HTTPTimeout, ok), ok),
		ReadinessMaxAge:           setting(tree, []string{"readiness_max_age"}, "2m", durationValue(c.ReadinessMaxAge, ok), ok),
		TagSyncEnabled:            setting(tree, []string{"tag_sync", "enabled"}, false, c.TagSync.Enabled, ok),
		TagSyncPrefix:             setting(tree, []string{"tag_sync", "prefix"}, "qbtw-", c.TagSync.Prefix, ok),
		TagSyncMaxWritesPerPoll:   setting(tree, []string{"tag_sync", "max_writes_per_poll"}, 20, c.TagSync.MaxWritesPerPoll, ok),
		TLSInsecure:               setting(tree, []string{"tls_insecure_skip_verify"}, false, c.TLSInsecure, ok),
		TLSCAFile:                 setting(tree, []string{"tls_ca_file"}, "", c.TLSCAFile, ok),
		LogLevel:                  setting(tree, []string{"log_level"}, "info", c.LogLevel, ok),
		LogFormat:                 setting(tree, []string{"log_format"}, "json", c.LogFormat, ok),
		LogColor:                  setting(tree, []string{"log_color"}, "auto", c.LogColor, ok),
		StateFile:                 setting(tree, []string{"state_file"}, "/data/state.json", c.StateFile, ok),
		Listen:                    setting(tree, []string{"listen"}, ":8080", c.Listen, ok),
	}
}

// setting builds one leaf from the source tree, falling back to def when the
// document omits the key. Effective is only attached when the document decoded.
func setting(tree map[string]any, path []string, def any, effective any, hasEffective bool) Setting {
	value, present := lookupPath(tree, path)
	result := Setting{Value: def, Source: SourceDefault}
	if present {
		result.Value = value
		result.Source = SourceExplicit
	}
	if hasEffective {
		result.Effective = effective
	}
	return result
}

func durationValue(d interface{ String() string }, ok bool) any {
	if !ok {
		return nil
	}
	return d.String()
}

func urlValue(u *url.URL, ok bool) any {
	if !ok || u == nil {
		return nil
	}
	return u.String()
}

// parseSourceTree decodes the document into a plain tree without expanding any
// ${ENV} reference, so the editor sees values exactly as written.
func parseSourceTree(format Format, data []byte) (map[string]any, error) {
	v := viper.New()
	v.SetConfigType(string(format))
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("invalid %s syntax", format)
	}
	tree := v.AllSettings()
	if tree == nil {
		tree = map[string]any{}
	}
	return tree, nil
}

func lookupPath(tree map[string]any, path []string) (any, bool) {
	var current any = tree
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, present := mapping[key]
		if !present {
			return nil, false
		}
		current = value
	}
	return current, true
}

// fieldError attributes a validation message to the leading setting path.
func fieldError(err error) FieldError {
	message := err.Error()
	field := message
	if index := strings.IndexAny(message, " .:"); index > 0 {
		field = message[:index]
	}
	return FieldError{Field: field, Message: message}
}

// referencedEnvNames lists every ${NAME} reference in the document, sorted.
func referencedEnvNames(format Format, raw []byte) []string {
	tree, err := parseSourceTree(format, raw)
	if err != nil {
		return sortedNames(scanEnvReferences(string(raw)))
	}
	names := map[string]bool{}
	walkStrings(tree, func(value string) {
		for _, name := range scanEnvReferences(value) {
			names[name] = true
		}
	})
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func walkStrings(value any, visit func(string)) {
	switch v := value.(type) {
	case string:
		visit(v)
	case map[string]any:
		for _, entry := range v {
			walkStrings(entry, visit)
		}
	case []any:
		for _, entry := range v {
			walkStrings(entry, visit)
		}
	}
}

// scanEnvReferences mirrors expandString's scanner but collects names instead
// of substituting them. $$ is an escaped literal and never a reference.
func scanEnvReferences(value string) []string {
	names := []string{}
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 == len(value) || value[i+1] != '$' && value[i+1] != '{' {
			i++
			continue
		}
		if value[i+1] == '$' {
			i += 2
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			break
		}
		name := value[i+2 : i+2+end]
		if validEnvironmentName(name) {
			names = append(names, name)
		}
		i += end + 3
	}
	return names
}

func sortedNames(names []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(names))
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

// envMetadata merges the effective environment with the referenced names and
// reports availability only. A referenced name that is absent is still listed.
func envMetadata(environment map[string]string, referenced []string) []EnvVar {
	configured := map[string]bool{}
	names := map[string]bool{}
	for name := range environment {
		names[name] = true
	}
	for _, name := range referenced {
		names[name] = true
		configured[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	result := make([]EnvVar, 0, len(sorted))
	for _, name := range sorted {
		value, present := environment[name]
		result = append(result, EnvVar{
			Name:       name,
			Available:  present,
			Empty:      present && value == "",
			Configured: configured[name],
		})
	}
	return result
}
