package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// ErrConflict reports that the file changed on disk between Read and Save.
var ErrConflict = errors.New("config file changed on disk; reload and retry")

// Patch is the set of settings the UI may edit. Every field is a pointer so
// the UI can send only the fields it changed. A nil pointer means "unchanged";
// a non-nil pointer to the zero value means "set to zero/false/empty". Use the
// same JSON tags as the config file keys (snake_case).
type Patch struct {
	// General.
	DryRun            *bool   `json:"dry_run,omitempty"`
	MaxActionsPerPoll *int    `json:"max_actions_per_poll,omitempty"`
	PollInterval      *string `json:"poll_interval,omitempty"`
	UIRefreshInterval *string `json:"ui_refresh_interval,omitempty"`
	HistoryLimit      *int    `json:"history_limit,omitempty"`
	// Advanced.
	MaxObservationGap         *string `json:"max_observation_gap,omitempty"` // duration string
	DeleteConfirmationTimeout *string `json:"delete_confirmation_timeout,omitempty"`
	HTTPTimeout               *string `json:"http_timeout,omitempty"`
	ReadinessMaxAge           *string `json:"readiness_max_age,omitempty"`
	TagSyncEnabled            *bool   `json:"tag_sync_enabled,omitempty"`
	TagSyncPrefix             *string `json:"tag_sync_prefix,omitempty"`
	TagSyncMaxWritesPerPoll   *int    `json:"tag_sync_max_writes_per_poll,omitempty"`
	TLSInsecure               *bool   `json:"tls_insecure_skip_verify,omitempty"`
	TLSCAFile                 *string `json:"tls_ca_file,omitempty"`
	LogLevel                  *string `json:"log_level,omitempty"`
	LogFormat                 *string `json:"log_format,omitempty"`
	LogColor                  *string `json:"log_color,omitempty"`
	StateFile                 *string `json:"state_file,omitempty"`
	Listen                    *string `json:"listen,omitempty"`
	// Scope.
	IncludeCategories *[]string `json:"include_categories,omitempty"`
	ExcludeCategories *[]string `json:"exclude_categories,omitempty"`
	ExcludeTags       *[]string `json:"exclude_tags,omitempty"`
	// Connections.
	QBTURL      *string `json:"qbt_url,omitempty"`
	QBTUsername *string `json:"qbt_username,omitempty"`
	QBTAuthMode *string `json:"qbt_auth_mode,omitempty"` // api_key|password|none
	// Secrets maps a secret key (e.g. "qbt_api_key") to its patch.
	Secrets map[string]SecretPatch `json:"secrets,omitempty"`
	// Policies maps a policy id (e.g. "stalled_no_seeders") to its editable fields.
	Policies map[string]PolicyPatch `json:"policies,omitempty"`
	// Integrations maps an integration name ("sonarr"/"radarr") to its editable fields.
	Integrations map[string]IntegrationPatch `json:"integrations,omitempty"`
}

// PolicyPatch is the editable subset of one cleanup policy.
type PolicyPatch struct {
	Action           *string   `json:"action,omitempty"` // warn|delete|delete_file
	ThresholdSeconds *int      `json:"threshold_seconds,omitempty"`
	ArrMode          *string   `json:"arr_mode,omitempty"` // inherit|none|blocklist_and_search|blocklist_only|search_only
	MatchTags        *[]string `json:"match_tags,omitempty"`
}

// IntegrationPatch is the editable subset of one media-manager integration.
type IntegrationPatch struct {
	Enabled    *bool     `json:"enabled,omitempty"`
	URL        *string   `json:"url,omitempty"`
	Mode       *string   `json:"mode,omitempty"` // blocklist_and_search|blocklist_only|search_only
	Timeout    *string   `json:"timeout,omitempty"`
	Categories *[]string `json:"categories,omitempty"`
}

// SecretPatch changes one secret's source. Mode is keep (default), replace or
// clear. For replace, Source selects value, env or path and the matching field
// carries the new source. A keep patch never touches the document.
type SecretPatch struct {
	Mode   string  `json:"mode,omitempty"`
	Source string  `json:"source,omitempty"`
	Value  *string `json:"value,omitempty"`
	Env    *string `json:"env,omitempty"`
	Path   *string `json:"path,omitempty"`
}

// Editor reads and writes the raw config file at path. It edits the raw
// document rather than the resolved Config, so comments, key order and
// ${ENV}/secret references survive a save untouched.
type Editor struct {
	path    string
	current func() Config
}

// NewEditor returns an Editor bound to path.
func NewEditor(path string) *Editor { return &Editor{path: path} }

// SetCurrent supplies the running configuration so a save can reject a
// restart-only change before it is written. It is optional; without it the
// manager still rejects the change when the candidate is applied.
func (e *Editor) SetCurrent(current func() Config) { e.current = current }

// Read returns the raw file bytes and a stamp identifying this exact version.
// The stamp folds the modification time and a content hash, so a same-size
// rewrite within the filesystem's timestamp resolution is still detected.
func (e *Editor) Read() (raw []byte, stamp string, err error) {
	f, err := os.Open(e.path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	raw, err = io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	return raw, fileStamp(raw, info.ModTime()), nil
}

// Save applies patch to the raw document, validates the candidate, and only
// then atomically writes it. It returns the new stamp. If the file changed
// since Read (stamp mismatch) it returns ErrConflict without writing. An
// invalid candidate is rejected before the file is touched, so a bad edit can
// never overwrite a good configuration. ${ENV} and secret references are never
// resolved into the document.
func (e *Editor) Save(raw []byte, stamp string, patch Patch) (newStamp string, err error) {
	format, err := FormatFor(e.path)
	if err != nil {
		return "", err
	}
	_, currentStamp, err := e.Read()
	if err != nil {
		return "", err
	}
	if currentStamp != stamp {
		return "", ErrConflict
	}
	candidate, err := applyPatch(format, raw, patch)
	if err != nil {
		return "", err
	}
	if err := e.validate(candidate); err != nil {
		return "", err
	}
	if err := e.writeAtomic(candidate); err != nil {
		return "", err
	}
	info, err := os.Stat(e.path)
	if err != nil {
		return "", err
	}
	return fileStamp(candidate, info.ModTime()), nil
}

// SaveRaw validates raw and only then atomically writes it, returning the new
// stamp. It does not apply a patch; the caller supplies the full edited
// document. An invalid document is rejected before the file is touched.
func (e *Editor) SaveRaw(raw []byte, stamp string) (newStamp string, err error) {
	_, currentStamp, err := e.Read()
	if err != nil {
		return "", err
	}
	if currentStamp != stamp {
		return "", ErrConflict
	}
	if err := e.validate(raw); err != nil {
		return "", err
	}
	if err := e.writeAtomic(raw); err != nil {
		return "", err
	}
	info, err := os.Stat(e.path)
	if err != nil {
		return "", err
	}
	return fileStamp(raw, info.ModTime()), nil
}

// validate decodes a candidate with the same environment and path context the
// loader uses, then rejects a restart-only change against the running config.
func (e *Editor) validate(data []byte) error {
	format, err := FormatFor(e.path)
	if err != nil {
		return err
	}
	environment, err := effectiveEnvironment(e.path, environmentSnapshot(os.Environ()))
	if err != nil {
		return err
	}
	next, err := DecodeWithEnvironment(format, data, environment)
	if err != nil {
		return err
	}
	if e.current != nil {
		return RestartRequiredError(e.current(), next)
	}
	return nil
}

// fileStamp identifies one exact on-disk version of the file.
func fileStamp(raw []byte, modtime time.Time) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%d:%x", modtime.UnixNano(), sum)
}

// writeAtomic writes data to a temp file in the same directory, fsyncs it,
// restores the original mode, and renames it over the target. The temp file is
// removed on any failure.
func (e *Editor) writeAtomic(data []byte) error {
	dir := filepath.Dir(e.path)
	mode := os.FileMode(0644)
	if info, err := os.Stat(e.path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".qbt-watchdog-edit-*")
	if err != nil {
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	name := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	if err := os.Rename(name, e.path); err != nil {
		return fmt.Errorf("cannot write configuration file: %w", err)
	}
	keep = true
	return nil
}

// applyPatch edits a raw document in place, preserving every key, comment and
// scalar it does not touch. It never serializes a resolved Config. The same
// patch applies to YAML and TOML through the documentEditor abstraction.
func applyPatch(format Format, raw []byte, patch Patch) ([]byte, error) {
	editor, err := newDocumentEditor(format, raw)
	if err != nil {
		return nil, err
	}
	setString := func(path []string, v *string) {
		if v != nil {
			editor.set(path, *v)
		}
	}
	setBool := func(path []string, v *bool) {
		if v != nil {
			editor.set(path, *v)
		}
	}
	setInt := func(path []string, v *int) {
		if v != nil {
			editor.set(path, *v)
		}
	}
	setStringList := func(path []string, v *[]string) {
		if v != nil {
			editor.set(path, *v)
		}
	}

	// Authentication mode first, so a secret patch in the same request wins.
	if patch.QBTAuthMode != nil {
		if err := applyAuthMode(editor, *patch.QBTAuthMode); err != nil {
			return nil, err
		}
	}

	setBool([]string{"dry_run"}, patch.DryRun)
	setInt([]string{"max_actions_per_poll"}, patch.MaxActionsPerPoll)
	setString([]string{"poll_interval"}, patch.PollInterval)
	setString([]string{"ui_refresh_interval"}, patch.UIRefreshInterval)
	setInt([]string{"history_limit"}, patch.HistoryLimit)
	setString([]string{"max_observation_gap"}, patch.MaxObservationGap)
	setString([]string{"delete_confirmation_timeout"}, patch.DeleteConfirmationTimeout)
	setString([]string{"http_timeout"}, patch.HTTPTimeout)
	setString([]string{"readiness_max_age"}, patch.ReadinessMaxAge)
	setBool([]string{"tag_sync", "enabled"}, patch.TagSyncEnabled)
	setString([]string{"tag_sync", "prefix"}, patch.TagSyncPrefix)
	setInt([]string{"tag_sync", "max_writes_per_poll"}, patch.TagSyncMaxWritesPerPoll)
	setBool([]string{"tls_insecure_skip_verify"}, patch.TLSInsecure)
	setString([]string{"tls_ca_file"}, patch.TLSCAFile)
	setString([]string{"log_level"}, patch.LogLevel)
	setString([]string{"log_format"}, patch.LogFormat)
	setString([]string{"log_color"}, patch.LogColor)
	setString([]string{"state_file"}, patch.StateFile)
	setString([]string{"listen"}, patch.Listen)
	setStringList([]string{"include_categories"}, patch.IncludeCategories)
	setStringList([]string{"exclude_categories"}, patch.ExcludeCategories)
	setStringList([]string{"exclude_tags"}, patch.ExcludeTags)
	setString([]string{"qbt_url"}, patch.QBTURL)
	setString([]string{"qbt_username"}, patch.QBTUsername)

	for _, id := range sortedKeys(patch.Policies) {
		pp := patch.Policies[id]
		path := []string{"policies", id}
		setString(append(path, "action"), pp.Action)
		if pp.ThresholdSeconds != nil {
			editor.set(append(path, "threshold"), strconv.Itoa(*pp.ThresholdSeconds)+"s")
		}
		setString(append(path, "arr_mode"), pp.ArrMode)
		setStringList(append(path, "match_tags"), pp.MatchTags)
	}

	for _, name := range sortedKeys(patch.Integrations) {
		ip := patch.Integrations[name]
		path := []string{"integrations", name}
		setBool(append(path, "enabled"), ip.Enabled)
		setString(append(path, "url"), ip.URL)
		setString(append(path, "mode"), ip.Mode)
		setString(append(path, "timeout"), ip.Timeout)
		setStringList(append(path, "categories"), ip.Categories)
	}

	for _, key := range sortedKeys(patch.Secrets) {
		spec, ok := secretSpecByKey[key]
		if !ok {
			return nil, fmt.Errorf("unknown secret %q", key)
		}
		if err := applySecretPatch(editor, spec, patch.Secrets[key]); err != nil {
			return nil, err
		}
	}

	return editor.encode()
}

// sortedKeys returns the map keys in a stable order so a patch produces the
// same document regardless of Go's map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func scalarNode(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func stringListNode(values []string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range values {
		node.Content = append(node.Content, scalarNode("!!str", v))
	}
	return node
}

// findMappingKey returns the value node for key in a mapping, or nil. Keys are
// matched exactly: YAML keys are case sensitive and the file uses snake_case.
func findMappingKey(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setMappingKey sets key to value in a mapping, creating the key if absent.
func setMappingKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// ensureMapping returns the mapping value for key, creating it if absent or
// replacing a non-mapping value.
func ensureMapping(parent *yaml.Node, key string) *yaml.Node {
	if child := findMappingKey(parent, key); child != nil && child.Kind == yaml.MappingNode {
		return child
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setMappingKey(parent, key, child)
	return child
}
