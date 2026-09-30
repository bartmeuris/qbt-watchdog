package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// ErrConflict reports that the file changed on disk between Read and Save.
var ErrConflict = errors.New("config file changed on disk; reload and retry")

// Patch is the set of settings the UI may edit. Every field is a pointer so
// the UI can send only the fields it changed. Use the same JSON tags as the
// config file keys (snake_case).
type Patch struct {
	DryRun                    *bool     `json:"dry_run,omitempty"`
	LogLevel                  *string   `json:"log_level,omitempty"`
	LogFormat                 *string   `json:"log_format,omitempty"`
	LogColor                  *string   `json:"log_color,omitempty"`
	MaxActionsPerPoll         *int      `json:"max_actions_per_poll,omitempty"`
	MaxObservationGap         *string   `json:"max_observation_gap,omitempty"` // duration string
	DeleteConfirmationTimeout *string   `json:"delete_confirmation_timeout,omitempty"`
	HistoryLimit              *int      `json:"history_limit,omitempty"`
	UIRefreshInterval         *string   `json:"ui_refresh_interval,omitempty"`
	TagSyncEnabled            *bool     `json:"tag_sync_enabled,omitempty"`
	TagSyncMaxWritesPerPoll   *int      `json:"tag_sync_max_writes_per_poll,omitempty"`
	IncludeCategories         *[]string `json:"include_categories,omitempty"`
	ExcludeCategories         *[]string `json:"exclude_categories,omitempty"`
	ExcludeTags               *[]string `json:"exclude_tags,omitempty"`
	// Policies maps a policy id (e.g. "stalled_no_seeders") to its editable fields.
	Policies map[string]PolicyPatch `json:"policies,omitempty"`
	// Integrations maps an integration name ("sonarr"/"radarr") to its editable fields.
	Integrations map[string]IntegrationPatch `json:"integrations,omitempty"`
}

// PolicyPatch is the editable subset of one cleanup policy.
type PolicyPatch struct {
	Action           *string   `json:"action,omitempty"` // warn|delete|delete_file
	ThresholdSeconds *int      `json:"threshold_seconds,omitempty"`
	MatchTags        *[]string `json:"match_tags,omitempty"`
}

// IntegrationPatch is the editable subset of one media-manager integration.
type IntegrationPatch struct {
	Mode *string `json:"mode,omitempty"` // none|blocklist_only|search_only|blocklist_and_search
}

// Editor reads and writes the raw config file at path. It edits the raw YAML
// document rather than the resolved Config, so comments, key order and
// ${ENV}/secret references survive a save untouched.
type Editor struct {
	path string
}

// NewEditor returns an Editor bound to path.
func NewEditor(path string) *Editor { return &Editor{path: path} }

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

// Save applies patch to the raw document, re-encodes it preserving comments and
// key order, and atomically writes it. It returns the new stamp. If the file
// changed since Read (stamp mismatch) it returns ErrConflict without writing.
// After a successful write it validates the result through the same loader the
// watcher uses and returns that error, so a caller can report "saved but
// rejected". ${ENV} and secret references are never resolved here.
func (e *Editor) Save(raw []byte, stamp string, patch Patch) (newStamp string, err error) {
	_, currentStamp, err := e.Read()
	if err != nil {
		return "", err
	}
	if currentStamp != stamp {
		return "", ErrConflict
	}
	out, err := applyPatch(raw, patch)
	if err != nil {
		return "", err
	}
	if err := e.writeAtomic(out); err != nil {
		return "", err
	}
	info, err := os.Stat(e.path)
	if err != nil {
		return "", err
	}
	newStamp = fileStamp(out, info.ModTime())
	if _, err := Load(e.path); err != nil {
		return newStamp, err
	}
	return newStamp, nil
}

// SaveRaw validates raw and atomically writes it, returning the new stamp.
// It does not apply a patch; the caller supplies the full edited document.
func (e *Editor) SaveRaw(raw []byte, stamp string) (newStamp string, err error) {
	_, currentStamp, err := e.Read()
	if err != nil {
		return "", err
	}
	if currentStamp != stamp {
		return "", ErrConflict
	}
	if err := e.writeAtomic(raw); err != nil {
		return "", err
	}
	info, err := os.Stat(e.path)
	if err != nil {
		return "", err
	}
	newStamp = fileStamp(raw, info.ModTime())
	if _, err := Load(e.path); err != nil {
		return newStamp, err
	}
	return newStamp, nil
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

// applyPatch edits a raw YAML document in place, preserving every key, comment
// and scalar it does not touch. It never serializes a resolved Config.
func applyPatch(raw []byte, patch Patch) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid configuration YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("configuration root must be a mapping")
	}

	setString := func(key string, v *string) {
		if v != nil {
			setMappingKey(root, key, scalarNode("!!str", *v))
		}
	}
	setBool := func(key string, v *bool) {
		if v != nil {
			setMappingKey(root, key, scalarNode("!!bool", strconv.FormatBool(*v)))
		}
	}
	setInt := func(key string, v *int) {
		if v != nil {
			setMappingKey(root, key, scalarNode("!!int", strconv.Itoa(*v)))
		}
	}
	setStringList := func(key string, v *[]string) {
		if v != nil {
			setMappingKey(root, key, stringListNode(*v))
		}
	}

	setBool("dry_run", patch.DryRun)
	setString("log_level", patch.LogLevel)
	setString("log_format", patch.LogFormat)
	setString("log_color", patch.LogColor)
	setInt("max_actions_per_poll", patch.MaxActionsPerPoll)
	setString("max_observation_gap", patch.MaxObservationGap)
	setString("delete_confirmation_timeout", patch.DeleteConfirmationTimeout)
	setInt("history_limit", patch.HistoryLimit)
	setString("ui_refresh_interval", patch.UIRefreshInterval)
	setStringList("include_categories", patch.IncludeCategories)
	setStringList("exclude_categories", patch.ExcludeCategories)
	setStringList("exclude_tags", patch.ExcludeTags)

	if patch.TagSyncEnabled != nil || patch.TagSyncMaxWritesPerPoll != nil {
		tagSync := ensureMapping(root, "tag_sync")
		if patch.TagSyncEnabled != nil {
			setMappingKey(tagSync, "enabled", scalarNode("!!bool", strconv.FormatBool(*patch.TagSyncEnabled)))
		}
		if patch.TagSyncMaxWritesPerPoll != nil {
			setMappingKey(tagSync, "max_writes_per_poll", scalarNode("!!int", strconv.Itoa(*patch.TagSyncMaxWritesPerPoll)))
		}
	}

	if len(patch.Policies) > 0 {
		policies := ensureMapping(root, "policies")
		for id, pp := range patch.Policies {
			policy := ensureMapping(policies, id)
			if pp.Action != nil {
				setMappingKey(policy, "action", scalarNode("!!str", *pp.Action))
			}
			if pp.ThresholdSeconds != nil {
				setMappingKey(policy, "threshold", scalarNode("!!str", strconv.Itoa(*pp.ThresholdSeconds)+"s"))
			}
			if pp.MatchTags != nil {
				setMappingKey(policy, "match_tags", stringListNode(*pp.MatchTags))
			}
		}
	}

	if len(patch.Integrations) > 0 {
		integrations := ensureMapping(root, "integrations")
		for name, ip := range patch.Integrations {
			integration := ensureMapping(integrations, name)
			if ip.Mode != nil {
				setMappingKey(integration, "mode", scalarNode("!!str", *ip.Mode))
			}
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("cannot encode configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("cannot encode configuration: %w", err)
	}
	return buf.Bytes(), nil
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
