package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"qbt-watchdog/internal/config"
)

// parseSettingsForm converts a submitted structured form into a config.Patch.
// It diffs every field against the current read model, so only genuinely
// changed leaves are sent: an untouched default stays a default instead of
// being frozen into the document. Field-specific problems are returned as
// config.FieldError values and never reach the editor, so a bad draft cannot
// overwrite a good configuration.
func parseSettingsForm(r *http.Request, current config.Settings) (config.Patch, []config.FieldError) {
	if err := r.ParseForm(); err != nil {
		return config.Patch{}, []config.FieldError{{Field: "form", Message: "invalid form body"}}
	}
	var patch config.Patch
	var errs []config.FieldError
	var err error
	record := func(field string, problem error) {
		if problem != nil {
			errs = append(errs, config.FieldError{Field: field, Message: problem.Error()})
		}
	}

	patch.DryRun = formBool(r, "dry_run", settingBool(current.General.DryRun.Value))
	patch.MaxActionsPerPoll, err = formInt(r, "max_actions_per_poll", settingInt(current.General.MaxActionsPerPoll.Value))
	record("max_actions_per_poll", err)
	patch.PollInterval, err = formDuration(r, "poll_interval", settingString(current.General.PollInterval.Value))
	record("poll_interval", err)
	patch.UIRefreshInterval, err = formDuration(r, "ui_refresh_interval", settingString(current.General.UIRefreshInterval.Value))
	record("ui_refresh_interval", err)
	patch.HistoryLimit, err = formInt(r, "history_limit", settingInt(current.General.HistoryLimit.Value))
	record("history_limit", err)

	patch.Policies = policyPatches(r, current, record)
	patch.Integrations = integrationPatches(r, current, record)

	patch.QBTURL = formString(r, "qbt_url", settingString(current.Connections.QBTURL.Value))
	patch.QBTUsername = formString(r, "qbt_username", settingString(current.Connections.QBTUsername.Value))
	patch.QBTAuthMode = formString(r, "qbt_auth_mode", settingString(current.Connections.QBTAuthMode.Value))

	patch.IncludeCategories = formList(r, "include_categories", settingList(current.Scope.IncludeCategories.Value))
	patch.ExcludeCategories = formList(r, "exclude_categories", settingList(current.Scope.ExcludeCategories.Value))
	patch.ExcludeTags = formList(r, "exclude_tags", settingList(current.Scope.ExcludeTags.Value))

	patch.MaxObservationGap, err = formDuration(r, "max_observation_gap", settingString(current.Advanced.MaxObservationGap.Value))
	record("max_observation_gap", err)
	patch.DeleteConfirmationTimeout, err = formDuration(r, "delete_confirmation_timeout", settingString(current.Advanced.DeleteConfirmationTimeout.Value))
	record("delete_confirmation_timeout", err)
	patch.HTTPTimeout, err = formDuration(r, "http_timeout", settingString(current.Advanced.HTTPTimeout.Value))
	record("http_timeout", err)
	patch.ReadinessMaxAge, err = formDuration(r, "readiness_max_age", settingString(current.Advanced.ReadinessMaxAge.Value))
	record("readiness_max_age", err)
	patch.TagSyncEnabled = formBool(r, "tag_sync_enabled", settingBool(current.Advanced.TagSyncEnabled.Value))
	patch.TagSyncPrefix = formString(r, "tag_sync_prefix", settingString(current.Advanced.TagSyncPrefix.Value))
	patch.TagSyncMaxWritesPerPoll, err = formInt(r, "tag_sync_max_writes_per_poll", settingInt(current.Advanced.TagSyncMaxWritesPerPoll.Value))
	record("tag_sync_max_writes_per_poll", err)
	patch.TLSInsecure = formBool(r, "tls_insecure_skip_verify", settingBool(current.Advanced.TLSInsecure.Value))
	patch.TLSCAFile = formString(r, "tls_ca_file", settingString(current.Advanced.TLSCAFile.Value))
	patch.LogLevel = formString(r, "log_level", settingString(current.Advanced.LogLevel.Value))
	patch.LogFormat = formString(r, "log_format", settingString(current.Advanced.LogFormat.Value))
	patch.LogColor = formString(r, "log_color", settingString(current.Advanced.LogColor.Value))

	secrets, secretErrs := secretPatches(r, current.Secrets)
	errs = append(errs, secretErrs...)
	if len(secrets) > 0 {
		patch.Secrets = secrets
	}
	return patch, errs
}

func policyPatches(r *http.Request, current config.Settings, record func(string, error)) map[string]config.PolicyPatch {
	patches := map[string]config.PolicyPatch{}
	for _, id := range config.PolicyIDs() {
		policy := current.Policies[string(id)]
		prefix := "policy." + string(id) + "."
		var patch config.PolicyPatch
		var err error
		patch.Action = formString(r, prefix+"action", settingString(policy.Action.Value))
		patch.ThresholdSeconds, err = formDurationSeconds(r, prefix+"threshold", settingString(policy.ThresholdSeconds.Value))
		record(prefix+"threshold", err)
		patch.ArrMode = formString(r, prefix+"arr_mode", settingString(policy.ArrMode.Value))
		patch.MatchTags = formList(r, prefix+"match_tags", settingList(policy.MatchTags.Value))
		if patch.Action != nil || patch.ThresholdSeconds != nil || patch.ArrMode != nil || patch.MatchTags != nil {
			patches[string(id)] = patch
		}
	}
	if len(patches) == 0 {
		return nil
	}
	return patches
}

func integrationPatches(r *http.Request, current config.Settings, record func(string, error)) map[string]config.IntegrationPatch {
	patches := map[string]config.IntegrationPatch{}
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		arr := current.Connections.Arr[string(kind)]
		prefix := "integration." + string(kind) + "."
		var patch config.IntegrationPatch
		var err error
		patch.Enabled = formBool(r, prefix+"enabled", settingBool(arr.Enabled.Value))
		patch.URL = formString(r, prefix+"url", settingString(arr.URL.Value))
		patch.Mode = formString(r, prefix+"mode", settingString(arr.Mode.Value))
		patch.Timeout, err = formDuration(r, prefix+"timeout", settingString(arr.Timeout.Value))
		record(prefix+"timeout", err)
		patch.Categories = formList(r, prefix+"categories", settingList(arr.Categories.Value))
		if patch.Enabled != nil || patch.URL != nil || patch.Mode != nil || patch.Timeout != nil || patch.Categories != nil {
			patches[string(kind)] = patch
		}
	}
	if len(patches) == 0 {
		return nil
	}
	return patches
}

// secretPatches derives one patch per secret from the submitted control. A
// secret whose source is unchanged and whose draft is empty is left untouched
// (mode keep), so an untouched form can never erase a credential. Switching
// source sends replace with the new source; the backend removes the conflicting
// key atomically.
func secretPatches(r *http.Request, current []config.SecretSetting) (map[string]config.SecretPatch, []config.FieldError) {
	patches := map[string]config.SecretPatch{}
	var errs []config.FieldError
	for _, secret := range current {
		prefix := "secret." + secret.Key + "."
		if r.FormValue(prefix+"clear") == "true" {
			patches[secret.Key] = config.SecretPatch{Mode: "clear"}
			continue
		}
		kind := r.FormValue(prefix + "type")
		if kind == "" {
			continue
		}
		original := string(secret.Source)
		switch kind {
		case "value":
			value := r.FormValue(prefix + "value")
			if value != "" {
				patches[secret.Key] = config.SecretPatch{Mode: "replace", Source: "value", Value: &value}
				continue
			}
			if original == "value" || original == "unset" {
				continue
			}
			errs = append(errs, config.FieldError{Field: secret.Key, Message: "a secret value is required"})
		case "env":
			name := r.FormValue(prefix + "env")
			if name == "" {
				if original == "env" || original == "unset" {
					continue
				}
				errs = append(errs, config.FieldError{Field: secret.Key, Message: "an environment variable name is required"})
				continue
			}
			if original == "env" && name == secret.EnvName {
				continue
			}
			patches[secret.Key] = config.SecretPatch{Mode: "replace", Source: "env", Env: &name}
		case "path":
			path := r.FormValue(prefix + "path")
			if path == "" {
				if original == "path" || original == "unset" {
					continue
				}
				errs = append(errs, config.FieldError{Field: secret.Key, Message: "a secret file path is required"})
				continue
			}
			if original == "path" && path == secret.Path {
				continue
			}
			patches[secret.Key] = config.SecretPatch{Mode: "replace", Source: "path", Path: &path}
		default:
			errs = append(errs, config.FieldError{Field: secret.Key, Message: "secret source must be value, env or path"})
		}
	}
	return patches, errs
}

// formBool reads a boolean control. A hidden "false" precedes the checkbox, so
// the field is always present and the last "true" wins. An unchanged value is
// reported as nil so it never enters the patch.
func formBool(r *http.Request, name string, current bool) *bool {
	values, present := r.Form[name]
	if !present {
		return nil
	}
	next := false
	for _, value := range values {
		if value == "true" {
			next = true
		}
	}
	if next == current {
		return nil
	}
	return &next
}

func formInt(r *http.Request, name string, current int) (*int, error) {
	values, present := r.Form[name]
	if !present || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return nil, nil
	}
	next, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return nil, fmt.Errorf("%s must be a whole number", name)
	}
	if next == current {
		return nil, nil
	}
	return &next, nil
}

func formString(r *http.Request, name, current string) *string {
	values, present := r.Form[name]
	if !present {
		return nil
	}
	next := values[0]
	if next == current {
		return nil
	}
	return &next
}

func formList(r *http.Request, name string, current []string) *[]string {
	values, present := r.Form[name]
	if !present {
		return nil
	}
	next := splitList(values[0])
	if equalStrings(next, current) {
		return nil
	}
	return &next
}

// formDuration reads a number+unit pair (or a raw fallback) and returns a Go
// duration string. A value semantically equal to the current one is nil, so a
// normalized representation never rewrites the document.
func formDuration(r *http.Request, name, current string) (*string, error) {
	if raw, present := r.Form[name+"_raw"]; present && len(raw) > 0 && strings.TrimSpace(raw[0]) != "" {
		text := strings.TrimSpace(raw[0])
		if _, err := time.ParseDuration(text); err != nil {
			return nil, fmt.Errorf("%s must be a duration such as 30s or 5m", name)
		}
		if sameDuration(text, current) {
			return nil, nil
		}
		return &text, nil
	}
	values, present := r.Form[name+"_value"]
	if !present || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return nil, nil
	}
	amount, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || amount < 0 {
		return nil, fmt.Errorf("%s must be a non-negative whole number", name)
	}
	unit := "s"
	if units, ok := r.Form[name+"_unit"]; ok && len(units) > 0 {
		unit = units[0]
	}
	if unit != "s" && unit != "m" && unit != "h" {
		return nil, fmt.Errorf("%s unit must be seconds, minutes or hours", name)
	}
	text := strconv.Itoa(amount) + unit
	if sameDuration(text, current) {
		return nil, nil
	}
	return &text, nil
}

// formDurationSeconds is formDuration for a policy threshold, which the patch
// carries as whole seconds.
func formDurationSeconds(r *http.Request, name, current string) (*int, error) {
	text, err := formDuration(r, name, current)
	if err != nil || text == nil {
		return nil, err
	}
	d, err := time.ParseDuration(*text)
	if err != nil {
		return nil, fmt.Errorf("%s must be a duration such as 30s or 5m", name)
	}
	seconds := int(d / time.Second)
	return &seconds, nil
}

func sameDuration(a, b string) bool {
	// An unset optional duration and an explicit zero are indistinguishable in
	// the number+unit control, so they are treated as equal: an untouched
	// optional field must not be frozen into the document as "0s".
	if a == "" || b == "" {
		return zeroDuration(a) && zeroDuration(b)
	}
	first, errA := time.ParseDuration(a)
	second, errB := time.ParseDuration(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return first == second
}

func zeroDuration(text string) bool {
	if text == "" {
		return true
	}
	d, err := time.ParseDuration(text)
	return err == nil && d == 0
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
