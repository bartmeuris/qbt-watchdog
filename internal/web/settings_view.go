package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"qbt-watchdog/internal/config"
)

// This file adapts the typed config.Settings read model to the template. Every
// helper is total: an unexpected shape falls back to a truthful empty value
// rather than panicking, so a partially decoded document still renders a
// repairable form.

// durationParts splits a duration source value into a number and a unit for the
// structured editor. A value that is not a simple Go duration is returned raw
// so the template can fall back to a text input instead of guessing.
type durationParts struct {
	Value  int
	Unit   string
	Raw    string
	Simple bool
}

func durationPartsOf(value any) durationParts {
	text, _ := value.(string)
	if text == "" {
		return durationParts{Unit: "s", Simple: true}
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return durationParts{Raw: text}
	}
	seconds := int64(d / time.Second)
	switch {
	case seconds != 0 && seconds%3600 == 0:
		return durationParts{Value: int(seconds / 3600), Unit: "h", Simple: true}
	case seconds != 0 && seconds%60 == 0:
		return durationParts{Value: int(seconds / 60), Unit: "m", Simple: true}
	default:
		return durationParts{Value: int(seconds), Unit: "s", Simple: true}
	}
}

// settingString renders a scalar leaf as text. A nil leaf (an unset optional
// duration) renders empty rather than "<nil>".
func settingString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func settingBool(value any) bool {
	b, _ := value.(bool)
	return b
}

func settingInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

// settingList normalizes a list leaf to []string. Viper may hand back []any, so
// both shapes are accepted.
func settingList(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			result = append(result, fmt.Sprint(item))
		}
		return result
	case string:
		return splitList(v)
	default:
		return nil
	}
}

func settingListText(value any) string {
	return strings.Join(settingList(value), ", ")
}

// splitList parses the compact list input: comma or newline separated, trimmed,
// with empty entries dropped.
func splitList(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' })
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			result = append(result, field)
		}
	}
	return result
}

func sourceLabel(source config.SourceKind) string {
	switch source {
	case config.SourceExplicit:
		return "explicit"
	case config.SourceInherited:
		return "inherited"
	default:
		return "default"
	}
}

func secretSourceLabel(source config.SecretSource) string {
	switch source {
	case config.SecretValue:
		return "value"
	case config.SecretEnv:
		return "env"
	case config.SecretPath:
		return "path"
	default:
		return "unset"
	}
}

func secretFor(secrets []config.SecretSetting, key string) config.SecretSetting {
	for _, secret := range secrets {
		if secret.Key == key {
			return secret
		}
	}
	return config.SecretSetting{Key: key}
}

func envVarFor(environment []config.EnvVar, name string) config.EnvVar {
	for _, entry := range environment {
		if entry.Name == name {
			return entry
		}
	}
	return config.EnvVar{Name: name}
}

func policyIDs() []config.PolicyID { return config.PolicyIDs() }

func arrKinds() []config.ArrKind { return []config.ArrKind{config.Sonarr, config.Radarr} }

func actionOptions() []config.Action { return config.Actions() }

func arrModeOptions() []config.ArrMode { return config.ArrModes() }

func policyArrModeOptions() []config.ArrMode { return config.PolicyArrModes() }

func authModeOptions() []string { return []string{"none", "password", "api_key"} }

func authModeLabel(mode string) string {
	switch mode {
	case "api_key":
		return "API key"
	case "password":
		return "Username and password"
	default:
		return "None"
	}
}

func logLevelOptions() []string { return []string{"debug", "info", "warn", "error"} }

func logFormatOptions() []string { return []string{"json", "text", "console"} }

func logColorOptions() []string { return []string{"auto", "always", "never"} }

// secretTypeOptions is the closed set of secret source types the control offers.
func secretTypeOptions() []string { return []string{"value", "env", "path"} }

func secretTypeLabel(kind string) string {
	switch kind {
	case "env":
		return "Env var"
	case "path":
		return "Path"
	default:
		return "Value"
	}
}

// secretActiveType maps a secret source to the control group that is visible.
// An unset secret opens on the Value group so the operator can type one.
func secretActiveType(source config.SecretSource) string {
	if source == config.SecretEnv || source == config.SecretPath {
		return string(source)
	}
	return "value"
}

// option is one choice in a select control.
type option struct {
	Value string
	Label string
}

// settingField is the template-facing shape of one editable leaf. The value
// carriers are populated by the matching builder so a template never has to
// type-assert the untyped Setting.Value.
type settingField struct {
	Name    string
	Label   string
	Help    string
	Source  config.SourceKind
	Error   string
	Restart bool
	Anchor  string

	Bool     bool
	Int      int
	Text     string
	List     string
	Duration durationParts
	Options  []option
	Selected string
}

func boolField(name, label, help string, setting config.Setting, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, Bool: settingBool(setting.Value), Anchor: anchorOf(name)}
}

func intField(name, label, help string, setting config.Setting, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, Int: settingInt(setting.Value), Anchor: anchorOf(name)}
}

func durationField(name, label, help string, setting config.Setting, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, Duration: durationPartsOf(setting.Value), Anchor: anchorOf(name)}
}

func textField(name, label, help string, setting config.Setting, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, Text: settingString(setting.Value), Anchor: anchorOf(name)}
}

func listField(name, label, help string, setting config.Setting, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, List: settingListText(setting.Value), Anchor: anchorOf(name)}
}

func selectField(name, label, help string, setting config.Setting, options []option, restart bool) settingField {
	return settingField{Name: name, Label: label, Help: help, Source: setting.Source, Error: setting.Error, Restart: restart, Options: options, Selected: settingString(setting.Value), Anchor: anchorOf(name)}
}

// withAnchor overrides the derived anchor, used by policy fields whose id must
// keep its underscores (the Active policies page links to #policy-<id>).
func withAnchor(field settingField, anchor string) settingField {
	field.Anchor = anchor
	return field
}

func anchorOf(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

func actionOptionsView() []option {
	result := make([]option, 0, len(config.Actions()))
	for _, action := range config.Actions() {
		result = append(result, option{Value: string(action), Label: actionLabel(action)})
	}
	return result
}

func arrModeOptionsView() []option {
	result := make([]option, 0, len(config.ArrModes()))
	for _, mode := range config.ArrModes() {
		result = append(result, option{Value: string(mode), Label: modeLabel(mode)})
	}
	return result
}

func policyArrModeOptionsView() []option {
	result := make([]option, 0, len(config.PolicyArrModes()))
	for _, mode := range config.PolicyArrModes() {
		result = append(result, option{Value: string(mode), Label: modeLabel(mode)})
	}
	return result
}

func authModeOptionsView() []option {
	result := make([]option, 0, len(authModeOptions()))
	for _, mode := range authModeOptions() {
		result = append(result, option{Value: mode, Label: authModeLabel(mode)})
	}
	return result
}

func stringOptions(values []string) []option {
	result := make([]option, 0, len(values))
	for _, value := range values {
		result = append(result, option{Value: value, Label: value})
	}
	return result
}

// secretField is the template-facing shape of one secret source control.
type secretField struct {
	Key         string
	Label       string
	Help        string
	Source      config.SecretSource
	ActiveType  string
	Configured  bool
	EnvName     string
	Path        string
	Error       string
	Environment []config.EnvVar
}

func secretFieldView(key, label, help string, secret config.SecretSetting, environment []config.EnvVar) secretField {
	return secretField{
		Key:         key,
		Label:       label,
		Help:        help,
		Source:      secret.Source,
		ActiveType:  secretActiveType(secret.Source),
		Configured:  secret.Configured,
		EnvName:     secret.EnvName,
		Path:        secret.Path,
		Error:       secret.Error,
		Environment: environment,
	}
}
