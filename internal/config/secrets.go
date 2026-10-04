package config

import (
	"errors"
	"strings"
)

// secretSpec names one supported secret and the two document keys that can
// carry it: a direct value (literal or ${ENV} reference) and a secret-file
// path. The two are mutually exclusive in the document.
type secretSpec struct {
	Key    string
	Direct []string
	File   []string
}

// secretSpecList is the closed set of secrets the structured editor may patch.
// The order is stable so the read model is deterministic.
var secretSpecList = []secretSpec{
	{Key: "qbt_api_key", Direct: []string{"qbt_api_key"}, File: []string{"qbt_api_key_file"}},
	{Key: "qbt_password", Direct: []string{"qbt_password"}, File: []string{"qbt_password_file"}},
	{Key: "integrations.sonarr.api_key", Direct: []string{"integrations", "sonarr", "api_key"}, File: []string{"integrations", "sonarr", "api_key_file"}},
	{Key: "integrations.radarr.api_key", Direct: []string{"integrations", "radarr", "api_key"}, File: []string{"integrations", "radarr", "api_key_file"}},
}

var secretSpecByKey = func() map[string]secretSpec {
	specs := make(map[string]secretSpec, len(secretSpecList))
	for _, spec := range secretSpecList {
		specs[spec.Key] = spec
	}
	return specs
}()

// secretSettings infers the source of every supported secret from the raw
// document. It never returns a resolved secret value.
func secretSettings(tree map[string]any, environment map[string]string) []SecretSetting {
	result := make([]SecretSetting, 0, len(secretSpecList))
	for _, spec := range secretSpecList {
		result = append(result, secretMetadata(tree, spec, environment))
	}
	return result
}

func secretMetadata(tree map[string]any, spec secretSpec, environment map[string]string) SecretSetting {
	direct, directPresent := lookupPath(tree, spec.Direct)
	file, filePresent := lookupPath(tree, spec.File)
	directValue, _ := direct.(string)
	fileValue, _ := file.(string)
	result := SecretSetting{Key: spec.Key, Source: SecretUnset}
	switch {
	case filePresent && fileValue != "":
		result.Source = SecretPath
		result.Path = fileValue
		result.Configured = true
		if directPresent && directValue != "" {
			result.Error = "both a literal value and a secret file are set"
		}
	case directPresent && directValue != "":
		if name, ok := envReferenceName(directValue); ok {
			result.Source = SecretEnv
			result.EnvName = name
			result.Configured = true
			value, present := environment[name]
			switch {
			case !present:
				result.Error = "environment variable is not set"
			case value == "":
				result.Error = "environment variable is empty"
			}
		} else {
			result.Source = SecretValue
			result.Configured = true
		}
	}
	return result
}

// envReferenceName reports whether value is exactly one ${NAME} reference.
func envReferenceName(value string) (string, bool) {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	name := value[2 : len(value)-1]
	if !validEnvironmentName(name) {
		return "", false
	}
	return name, true
}

// applySecretPatch mutates the document for one secret. An omitted or "keep"
// patch leaves the secret untouched, so a form that does not render a secret
// can never erase it. Switching source removes the conflicting key atomically.
func applySecretPatch(editor documentEditor, spec secretSpec, patch SecretPatch) error {
	switch patch.Mode {
	case "", "keep":
		return nil
	case "clear":
		editor.remove(spec.Direct...)
		editor.remove(spec.File...)
		return nil
	case "replace":
		switch patch.Source {
		case "value":
			if patch.Value == nil {
				return errors.New("secret value is required")
			}
			editor.remove(spec.File...)
			editor.set(spec.Direct, escapeLiteral(*patch.Value))
		case "env":
			if patch.Env == nil || !validEnvironmentName(*patch.Env) {
				return errors.New("secret environment variable name is invalid")
			}
			editor.remove(spec.File...)
			editor.set(spec.Direct, "${"+*patch.Env+"}")
		case "path":
			if patch.Path == nil || *patch.Path == "" {
				return errors.New("secret file path is required")
			}
			editor.remove(spec.Direct...)
			editor.set(spec.File, *patch.Path)
		default:
			return errors.New("secret source must be value, env or path")
		}
		return nil
	default:
		return errors.New("secret mode must be keep, replace or clear")
	}
}

// escapeLiteral doubles every dollar so a literal value containing ${NAME}
// survives expansion as literal text instead of becoming a reference.
func escapeLiteral(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

// applyAuthMode switches the qBittorrent authentication mode by removing the
// credential keys the new mode cannot use. Secret patches applied afterwards
// set the credentials the new mode needs.
func applyAuthMode(editor documentEditor, mode string) error {
	switch mode {
	case "api_key":
		editor.remove("qbt_username")
		editor.remove("qbt_password")
		editor.remove("qbt_password_file")
	case "password":
		editor.remove("qbt_api_key")
		editor.remove("qbt_api_key_file")
	case "none":
		editor.remove("qbt_username")
		editor.remove("qbt_password")
		editor.remove("qbt_password_file")
		editor.remove("qbt_api_key")
		editor.remove("qbt_api_key_file")
	default:
		return errors.New("qbt_auth_mode must be api_key, password or none")
	}
	return nil
}
