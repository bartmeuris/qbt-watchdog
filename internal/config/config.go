// Package config owns the whole configuration surface of qbt-watchdog.
//
// The command line is deliberately tiny (--config, --once, --version, --help,
// plus the version and healthcheck subcommands). QBTW_CONFIG selects the file;
// other environment values are used only through explicit ${NAME} string
// references in YAML or TOML, with an optional adjacent .env supplying fallback
// values.
//
// Parsing is strict: the file is decoded once into an immutable Config value
// with unknown keys rejected, and every field is validated at that boundary.
// Nothing downstream ever re-validates, and nothing downstream ever reads
// Viper: Viper is not safe for concurrent use, so it exists only inside Decode
// and never escapes it. Hot reload publishes new immutable snapshots through
// Manager (see manager.go).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// PolicyID names one of the five uniform cleanup policies. Every torrent that
// the watchdog considers falls into exactly one of them.
type PolicyID string

const (
	// Metadata covers torrents stuck in the metaDL state.
	Metadata PolicyID = "metadata"
	// StalledNoSeeders covers stalledDL torrents with zero progress whose
	// connected seeders have never been observed.
	StalledNoSeeders PolicyID = "stalled_no_seeders"
	// StalledSeedersSeen covers stalledDL torrents with zero progress that
	// did have connected seeders at some earlier observation.
	StalledSeedersSeen PolicyID = "stalled_seeders_seen"
	// StalledPartial covers stalledDL torrents that made some progress.
	StalledPartial PolicyID = "stalled_partial"
	// CompletedNoData covers completed-looking torrents that carry no payload.
	CompletedNoData PolicyID = "completed_no_data"
	// StoppedArrManaged covers operator-stopped Arr-managed torrents selected by
	// explicit operator tags.
	StoppedArrManaged PolicyID = "stopped_arr_managed"
)

func PolicyIDs() []PolicyID {
	return []PolicyID{Metadata, StalledNoSeeders, StalledSeedersSeen, StalledPartial, CompletedNoData, StoppedArrManaged}
}
func (id PolicyID) Valid() bool { return slices.Contains(PolicyIDs(), id) }

// Action is what a policy does once its threshold elapses.
type Action string

const (
	Warn       Action = "warn"
	Delete     Action = "delete"
	DeleteFile Action = "delete_file"
)

func Actions() []Action            { return []Action{Warn, Delete, DeleteFile} }
func (a Action) Valid() bool       { return slices.Contains(Actions(), a) }
func (a Action) Destructive() bool { return a == Delete || a == DeleteFile }

// Policy is the uniform shape shared by all cleanup policies.
type Policy struct {
	Action    Action        `json:"action"`
	Threshold time.Duration `json:"threshold"`
	ArrMode   ArrMode       `json:"arr_mode"`
	MatchTags []string      `json:"match_tags,omitempty"`
}

// TagSync owns the qBittorrent tag namespace used for optional write-back.
type TagSync struct {
	Enabled          bool   `json:"enabled"`
	Prefix           string `json:"prefix"`
	MaxWritesPerPoll int    `json:"max_writes_per_poll"`
}

// Config is an immutable, fully validated snapshot of the configuration file.
// Callers must treat it as read-only; use Clone before handing a copy to code
// that might mutate the maps, slices or URL.
//
// Reload semantics, also documented in config.example.yaml:
//
//   - Restart required: Listen (the socket is already bound) and StateFile
//     (the on-disk episode store is already open). Changing either is rejected
//     with a warning and the previous configuration is retained in full.
//   - Applied live: everything else, including the qBittorrent endpoint,
//     credentials and TLS settings (the HTTP client is rebuilt), policies,
//     intervals, exclusions, tag_sync, log level and format, the readiness/UI
//     settings, and the Sonarr/Radarr integrations (their clients are rebuilt
//     too).
type Config struct {
	// TLSCAPEM is the CA bundle read from TLSCAFile at parse time, so a
	// rotated bundle is picked up by the same reload that re-reads secrets.
	TLSCAPEM []byte
	// URL is the qBittorrent base URL, normalised to a trailing slash.
	URL *url.URL

	Username, Password string
	APIKey             string `json:"-"`

	PollInterval, MaxObservationGap, HTTPTimeout                  time.Duration
	DeleteConfirmationTimeout, UIRefreshInterval, ReadinessMaxAge time.Duration

	DryRun, TLSInsecure, Once bool
	TagSync                   TagSync

	// MaxDeletions caps destructive actions per poll; 0 disables them.
	MaxDeletions, HistoryLimit int

	IncludeCategories, ExcludeCategories, ExcludeTags []string

	StateFile, Listen, TLSCAFile, LogLevel, LogFormat, LogColor, ConfigFile string

	Policies map[PolicyID]Policy

	// Integrations holds the Sonarr and Radarr endpoints. Like everything
	// except Listen and StateFile they apply live: a changed endpoint,
	// credential, mode or timeout is adopted by the reload that sees it,
	// and the affected client is rebuilt rather than reconfigured.
	Integrations Integrations
}

// Clone returns a deep copy so that a caller mutating its snapshot can never
// reach the configuration another goroutine is reading.
func (c Config) Clone() Config {
	c.TLSCAPEM = slices.Clone(c.TLSCAPEM)
	if c.URL != nil {
		u := *c.URL
		c.URL = &u
	}
	c.IncludeCategories = slices.Clone(c.IncludeCategories)
	c.ExcludeCategories = slices.Clone(c.ExcludeCategories)
	c.ExcludeTags = slices.Clone(c.ExcludeTags)
	p := make(map[PolicyID]Policy, len(c.Policies))
	for id, policy := range c.Policies {
		policy.MatchTags = slices.Clone(policy.MatchTags)
		p[id] = policy
	}
	c.Policies = p
	c.Integrations = c.Integrations.Clone()
	return c
}

// EffectiveAction is the only action the engine may act on: global dry_run
// downgrades every configured action to Warn.
func (c Config) EffectiveAction(id PolicyID) Action {
	if c.DryRun {
		return Warn
	}
	policy, ok := c.Policies[id]
	if !ok {
		return Warn
	}
	return policy.Action
}

func (c Config) EffectiveArrMode(id PolicyID, svc ArrService) (ArrMode, bool) {
	policy, ok := c.Policies[id]
	if !ok {
		return "", false
	}
	mode := policy.ArrMode
	if mode == NoArrMode {
		return "", false
	}
	if mode == "" || mode == InheritArrMode {
		return svc.Mode, true
	}
	return mode, true
}

func (c Config) EndpointKey() string {
	// Endpoint identity is only the qBittorrent URL. The tag_sync prefix is
	// persisted separately so prefix remediation can survive endpoint-stable
	// reloads and restarts.
	h := sha256.Sum256([]byte(c.URL.String()))
	return hex.EncodeToString(h[:])
}

// SafetyKey hashes safety settings and credentials, never persisting their raw
// values, so changed credentials cannot inherit elapsed time across a restart.
func (c Config) SafetyKey() string {
	type safetyPolicy struct {
		Action    Action
		Threshold time.Duration
		MatchTags []string
	}
	policies := make(map[PolicyID]safetyPolicy, len(c.Policies))
	for id, policy := range c.Policies {
		policies[id] = safetyPolicy{Action: policy.Action, Threshold: policy.Threshold, MatchTags: policy.MatchTags}
	}
	v := struct {
		URL                           string
		Credentials                   [3]string
		Policies                      map[PolicyID]safetyPolicy
		DryRun                        bool
		Poll, Gap, HTTP, Confirmation time.Duration
		Cap                           int
		Include, Exclude, Tags        []string
	}{c.URL.String(), [3]string{c.Username, c.Password, c.APIKey}, policies, c.DryRun, c.PollInterval, c.MaxObservationGap, c.HTTPTimeout, c.DeleteConfirmationTimeout, c.MaxDeletions, c.IncludeCategories, c.ExcludeCategories, c.ExcludeTags}
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// WarningCode is the stable identifier of a warning. Presentation layers route
// and deduplicate on this value, never on the human Message text, so wording can
// change without moving a warning to a different surface.
type WarningCode string

const (
	// WarningDryRunDisabled reports that global dry run is off.
	WarningDryRunDisabled WarningCode = "dry_run_disabled"
	// WarningRemoveFiles reports that a policy may remove payload files.
	WarningRemoveFiles WarningCode = "remove_files"
	// WarningTLSInsecure reports that certificate verification is disabled.
	WarningTLSInsecure WarningCode = "tls_insecure"
	// WarningReservedPrefix reports a tag that case-insensitively resembles the
	// reserved watchdog prefix but is not rejected as one.
	WarningReservedPrefix WarningCode = "reserved_prefix_near_miss"
	// WarningStoppedPolicyArmed reports that the stopped-Arr policy can act.
	WarningStoppedPolicyArmed WarningCode = "stopped_policy_armed"
)

// WarningScope names what a warning is about, so the UI can place it on the
// matching surface: the header, a policy row, or a settings control.
type WarningScope string

const (
	// WarningGlobal is a process-wide notice.
	WarningGlobal WarningScope = "global"
	// WarningPolicy is scoped to a policy (Policy is set).
	WarningPolicy WarningScope = "policy"
	// WarningSetting is scoped to a configuration control (Target is the anchor).
	WarningSetting WarningScope = "setting"
)

// Warning is a loud, operator-visible safety notice. Warnings are derived
// purely from the snapshot so they can be re-emitted after every reload, and
// they never contain secrets.
//
// Code, Scope and Target are the stable identity the UI routes on; Message is
// the human sentence shown to the operator. Policy is the structured policy the
// warning belongs to (empty when it is not policy specific). Tags carries the
// operator tags a tag-related warning is about.
type Warning struct {
	Code    WarningCode
	Scope   WarningScope
	Target  string // settings anchor (without '#') the warning links to, if any
	Message string
	Policy  PolicyID // empty when the warning is not policy specific
	Tags    []string
}

// Warnings lists every unsafe aspect of the snapshot, in a stable order.
func (c Config) Warnings() []Warning {
	warnings := []Warning{}
	if !c.DryRun {
		warnings = append(warnings, Warning{Code: WarningDryRunDisabled, Scope: WarningSetting, Target: "dry-run", Message: "Dry run disabled"})
	}
	for _, id := range PolicyIDs() {
		if c.EffectiveAction(id) == DeleteFile {
			warnings = append(warnings, Warning{
				Code: WarningRemoveFiles, Scope: WarningPolicy, Target: "policy-" + string(id) + "-action",
				Message: "Remove torrent and files", Policy: id,
			})
		}
	}
	if c.TLSInsecure {
		warnings = append(warnings, Warning{Code: WarningTLSInsecure, Scope: WarningSetting, Target: "advanced", Message: "TLS certificate verification disabled"})
	}
	for _, tag := range c.ExcludeTags {
		if reservedTagNearMiss(tag, c.TagSync.Prefix) {
			warnings = append(warnings, Warning{Code: WarningReservedPrefix, Scope: WarningSetting, Target: "scope", Message: "Tag resembles the reserved watchdog prefix but remains an operator tag", Tags: []string{tag}})
		}
	}
	if policy, ok := c.Policies[StoppedArrManaged]; ok {
		for _, tag := range policy.MatchTags {
			if reservedTagNearMiss(tag, c.TagSync.Prefix) {
				warnings = append(warnings, Warning{Code: WarningReservedPrefix, Scope: WarningSetting, Target: "scope", Message: "Tag resembles the reserved watchdog prefix but remains an operator tag", Policy: StoppedArrManaged, Tags: []string{tag}})
			}
		}
	}
	if policy, ok := c.Policies[StoppedArrManaged]; ok && len(policy.MatchTags) > 0 && !c.DryRun && c.EffectiveAction(StoppedArrManaged) != Warn {
		warnings = append(warnings, Warning{
			Code: WarningStoppedPolicyArmed, Scope: WarningPolicy, Target: "policy-stopped_arr_managed",
			Message: "Stopping a torrent with these tags makes it eligible for this policy after its waiting period. Arr recovery depends on the configured recovery mode.",
			Policy:  StoppedArrManaged,
		})
	}
	return warnings
}

// RestartRequired names the settings a running process cannot adopt.
var RestartRequired = []string{"listen", "state_file"}

// RestartRequiredChanges lists the restart-only settings that differ.
func RestartRequiredChanges(current, next Config) []string {
	changed := []string{}
	if current.Listen != next.Listen {
		changed = append(changed, "listen")
	}
	if current.StateFile != next.StateFile {
		changed = append(changed, "state_file")
	}
	return changed
}

// RestartRequiredError rejects a candidate that touches restart-only settings.
// The whole candidate is rejected rather than partially applied, so the
// previous configuration stays internally consistent.
func RestartRequiredError(current, next Config) error {
	changed := RestartRequiredChanges(current, next)
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("%s cannot change while running; restart to apply", strings.Join(changed, " and "))
}

// Format is the on-disk encoding of the configuration file.
type Format string

const (
	YAML Format = "yaml"
	TOML Format = "toml"
)

// FormatFor derives the encoding from the file extension. YAML is preferred;
// TOML is accepted because Viper decodes it with the same strictness.
func FormatFor(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return YAML, nil
	case ".toml":
		return TOML, nil
	}
	return "", errors.New("configuration file must end in .yaml, .yml or .toml")
}

// ErrVersion signals that --version was requested. It is not a failure: the
// caller prints the build information and exits successfully.
var ErrVersion = errors.New("version requested")

const usage = `Usage: qbt-watchdog [--config FILE] [--once]
       qbt-watchdog version | --version
       qbt-watchdog healthcheck [--url URL]

All settings live in the configuration file; see config.example.yaml.
The file is re-read automatically when it or a referenced dependency changes.`

// Parse handles the minimal command line. Per-setting flags and per-setting
// environment variables intentionally do not exist: an unrecognised flag is an
// error rather than a silently ignored setting.
func Parse(args []string, lookupEnv func(string) (string, bool), output io.Writer) (Config, error) {
	fs := flag.NewFlagSet("qbt-watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "config.yaml", "configuration file, YAML or TOML (or QBTW_CONFIG)")
	once := fs.Bool("once", false, "run one poll and exit")
	version := fs.Bool("version", false, "print version information and exit")
	fs.Usage = func() {
		fmt.Fprintln(output, usage)
		fs.SetOutput(output)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, err
		}
		return Config{}, errors.New("invalid command-line flags (use --help)")
	}
	if *version {
		return Config{}, ErrVersion
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("unexpected positional arguments")
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	if !explicit {
		if env, ok := lookupEnv("QBTW_CONFIG"); ok {
			*path = env
		}
	}
	c, err := Load(*path)
	c.Once = *once
	return c, err
}

// readBounded refuses oversized files and never echoes their content, which is
// what keeps secret files out of error strings.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open configuration or secret file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("configuration or secret file unreadable or too large")
	}
	return b, nil
}

// Load reads, decodes and validates the file at path. It is the only entry
// point used by the reload watcher, so a reload validates exactly like startup.
func Load(path string) (Config, error) {
	return LoadWithEnvironment(path, environmentSnapshot(os.Environ()))
}

// LoadWithEnvironment uses an explicit process snapshot instead of reading the
// real environment. Its values (including empty ones) shadow the adjacent .env.
// The caller must not mutate process while this call is running.
func LoadWithEnvironment(path string, process map[string]string) (Config, error) {
	format, err := FormatFor(path)
	if err != nil {
		return Config{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Config{}, errors.New("cannot resolve configuration location")
	}
	data, err := readBounded(path, 1024*1024)
	if err != nil {
		return Config{}, err
	}
	environment, err := effectiveEnvironment(path, process)
	if err != nil {
		return Config{}, err
	}
	c, err := DecodeWithEnvironment(format, data, environment)
	if err != nil {
		return Config{}, err
	}
	c.ConfigFile = path
	return c, nil
}

func configDependencyPaths(path string, process map[string]string) ([]string, error) {
	format, err := FormatFor(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, errors.New("cannot resolve configuration location")
	}
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	data, err := readBounded(path, 1024*1024)
	if err != nil {
		return nil, err
	}
	environment, err := readDotEnv(dotenv)
	if err != nil {
		return nil, err
	}
	for name, value := range process {
		environment[name] = value
	}
	v := viper.New()
	v.SetConfigType(string(format))
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("invalid %s syntax", format)
	}
	f := defaults()
	settings, err := json.Marshal(v.AllSettings())
	if err != nil {
		return nil, errors.New("invalid configuration values")
	}
	settings, err = expandSettings(settings, environment)
	if err != nil {
		return nil, err
	}
	strict := json.NewDecoder(bytes.NewReader(settings))
	strict.DisallowUnknownFields()
	if err = strict.Decode(&f); err != nil {
		return nil, errors.New("configuration contains unknown fields or incorrect types")
	}
	paths := coreDependencyPaths(path)
	paths = append(paths, f.APIKeyFile, f.PasswordFile, f.TLSCAFile)
	if f.Integrations != nil {
		if f.Integrations.Sonarr != nil {
			paths = append(paths, f.Integrations.Sonarr.APIKeyFile)
		}
		if f.Integrations.Radarr != nil {
			paths = append(paths, f.Integrations.Radarr.APIKeyFile)
		}
	}
	return paths, nil
}

// fileConfig mirrors the file one-to-one. Durations stay strings here so that
// "30" is rejected instead of silently meaning 30 nanoseconds.
type filePolicy struct {
	Action    *Action           `json:"action"`
	Threshold *string           `json:"threshold"`
	ArrMode   *ArrMode          `json:"arr_mode"`
	MatchTags presentStringList `json:"match_tags"`
}

type presentStringList struct {
	Values  []string
	Present bool
}

func (l *presentStringList) UnmarshalJSON(data []byte) error {
	l.Present = true
	if string(data) == "null" {
		l.Values = nil
		return nil
	}
	return json.Unmarshal(data, &l.Values)
}

func markPolicyMatchTagPresence(format Format, data []byte, policies *map[PolicyID]filePolicy) error {
	if format != YAML {
		return nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("invalid %s syntax", format)
	}
	if len(document.Content) == 0 {
		return nil
	}
	policiesNode := mappingValue(document.Content[0], "policies")
	if policiesNode == nil || policiesNode.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(policiesNode.Content); i += 2 {
		id := PolicyID(strings.ToLower(policiesNode.Content[i].Value))
		policyNode := policiesNode.Content[i+1]
		if mappingValue(policyNode, "match_tags") == nil {
			continue
		}
		if *policies == nil {
			*policies = map[PolicyID]filePolicy{}
		}
		policy := (*policies)[id]
		policy.MatchTags.Present = true
		(*policies)[id] = policy
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if strings.EqualFold(node.Content[i].Value, key) {
			return node.Content[i+1]
		}
	}
	return nil
}

type fileTagSync struct {
	Enabled          bool   `json:"enabled"`
	Prefix           string `json:"prefix"`
	MaxWritesPerPoll int    `json:"max_writes_per_poll"`
}

type fileConfig struct {
	QBTURL                    string                  `json:"qbt_url"`
	Username                  string                  `json:"qbt_username"`
	Password                  string                  `json:"qbt_password"`
	PasswordFile              string                  `json:"qbt_password_file"`
	APIKey                    string                  `json:"qbt_api_key"`
	APIKeyFile                string                  `json:"qbt_api_key_file"`
	PollInterval              string                  `json:"poll_interval"`
	MaxObservationGap         *string                 `json:"max_observation_gap"`
	HTTPTimeout               string                  `json:"http_timeout"`
	DeleteConfirmationTimeout string                  `json:"delete_confirmation_timeout"`
	UIRefreshInterval         string                  `json:"ui_refresh_interval"`
	ReadinessMaxAge           string                  `json:"readiness_max_age"`
	DryRun                    bool                    `json:"dry_run"`
	TLSInsecure               bool                    `json:"tls_insecure_skip_verify"`
	MaxDeletions              int                     `json:"max_actions_per_poll"`
	HistoryLimit              int                     `json:"history_limit"`
	IncludeCategories         []string                `json:"include_categories"`
	ExcludeCategories         []string                `json:"exclude_categories"`
	ExcludeTags               []string                `json:"exclude_tags"`
	StateFile                 string                  `json:"state_file"`
	Listen                    string                  `json:"listen"`
	TLSCAFile                 string                  `json:"tls_ca_file"`
	LogLevel                  string                  `json:"log_level"`
	LogFormat                 string                  `json:"log_format"`
	LogColor                  string                  `json:"log_color"`
	Policies                  map[PolicyID]filePolicy `json:"policies"`
	TagSync                   fileTagSync             `json:"tag_sync"`
	Integrations              *fileIntegrations       `json:"integrations"`
}

// defaults are the safe values used for every key the file omits.
func defaults() fileConfig {
	return fileConfig{
		PollInterval: "30s", HTTPTimeout: "10s", DeleteConfirmationTimeout: "2m",
		UIRefreshInterval: "5s", ReadinessMaxAge: "2m",
		DryRun:       true,
		MaxDeletions: 10, HistoryLimit: 100,
		ExcludeTags: []string{"keep", "qbt-watchdog-ignore"},
		StateFile:   "/data/state.json", Listen: ":8080",
		LogLevel: "info", LogFormat: "json", LogColor: "auto",
		TagSync:      fileTagSync{Prefix: "qbtw-", MaxWritesPerPoll: 20},
		Integrations: defaultIntegrations(),
	}
}

// Decode turns file bytes into a validated snapshot.
//
// Each call owns its own Viper instance and discards it before returning, so
// no Viper state is ever shared with a poll or request path. Viper is used
// only as the YAML/TOML reader; the settings map is then re-decoded strictly
// with encoding/json so unknown keys and wrong types are hard errors instead
// of permissive Get* conversions. Errors never echo file content, which is
// what keeps inline secrets out of logs.
func Decode(format Format, data []byte) (Config, error) {
	return DecodeWithEnvironment(format, data, nil)
}

// removedWebAuthKeys are the built-in web authentication settings that no
// longer exist. They are still parsed by Viper (Viper keeps every key), so a
// file that carries them can be recognised and rejected with a migration
// instruction instead of a generic unknown-field error. A deployment that
// relied on built-in auth must not start silently unprotected.
var removedWebAuthKeys = []string{"web_username", "web_password", "web_password_file", "metrics_public"}

func removedWebAuthSettings(settings map[string]any) []string {
	removed := []string{}
	for _, key := range removedWebAuthKeys {
		if _, present := settings[key]; present {
			removed = append(removed, key)
		}
	}
	return removed
}

// DecodeWithEnvironment expands parsed string values using only environment.
// Neither decoder reads the process environment or .env; Decode rejects any
// unescaped reference. Secret-file settings still read their named files.
func DecodeWithEnvironment(format Format, data []byte, environment map[string]string) (Config, error) {
	if format != YAML && format != TOML {
		return Config{}, errors.New("unsupported configuration format")
	}
	v := viper.New()
	v.SetConfigType(string(format))
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return Config{}, fmt.Errorf("invalid %s syntax", format)
	}
	parsed := v.AllSettings()
	if removed := removedWebAuthSettings(parsed); len(removed) > 0 {
		return Config{}, fmt.Errorf("built-in web authentication was removed; delete %s and enforce access control with a reverse proxy", strings.Join(removed, ", "))
	}
	f := defaults()
	settings, err := json.Marshal(parsed)
	if err != nil {
		return Config{}, errors.New("invalid configuration values")
	}
	settings, err = expandSettings(settings, environment)
	if err != nil {
		return Config{}, err
	}
	strict := json.NewDecoder(bytes.NewReader(settings))
	strict.DisallowUnknownFields()
	if err = strict.Decode(&f); err != nil {
		return Config{}, errors.New("configuration contains unknown fields or incorrect types")
	}
	if err = markPolicyMatchTagPresence(format, data, &f.Policies); err != nil {
		return Config{}, err
	}
	return build(f)
}

// build converts an already-shaped file into the trusted snapshot, failing on
// the first problem it finds.
func build(f fileConfig) (Config, error) {
	endpoint, err := parseEndpoint(f.QBTURL)
	if err != nil {
		return Config{}, err
	}
	policies, err := parsePolicies(f.Policies)
	if err != nil {
		return Config{}, err
	}
	integrations, err := parseIntegrations(f.Integrations)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Integrations: integrations,
		URL:          endpoint, Username: f.Username,
		DryRun: f.DryRun, TLSInsecure: f.TLSInsecure,
		MaxDeletions: f.MaxDeletions, HistoryLimit: f.HistoryLimit,
		IncludeCategories: normalizeList(f.IncludeCategories),
		ExcludeCategories: normalizeList(f.ExcludeCategories),
		ExcludeTags:       normalizeList(f.ExcludeTags),
		TagSync:           TagSync{Enabled: f.TagSync.Enabled, Prefix: f.TagSync.Prefix, MaxWritesPerPoll: f.TagSync.MaxWritesPerPoll},
		StateFile:         f.StateFile, Listen: f.Listen, TLSCAFile: f.TLSCAFile,
		LogLevel: f.LogLevel, LogFormat: f.LogFormat, LogColor: f.LogColor, Policies: policies,
	}
	if err = parseDurations(f, &c); err != nil {
		return Config{}, err
	}
	if err = resolveSecrets(f, &c); err != nil {
		return Config{}, err
	}
	if err = validate(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func parseEndpoint(raw string) (*url.URL, error) { return parseBaseURL("qbt_url", raw) }

// parseBaseURL accepts only an HTTP(S) origin with an optional path, which is
// what lets a service sit behind a reverse proxy at https://host/sonarr/. The
// trailing slash is normalised on so that joining a relative API path can
// never eat the last path segment. Credentials, queries and fragments are
// refused: a base URL is not a place to hide a secret.
func parseBaseURL(field, raw string) (*url.URL, error) {
	invalid := fmt.Errorf("%s must be an HTTP(S) base URL without credentials, query or fragment", field)
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, invalid
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid %s port", field)
		}
	}
	escapedPath := strings.TrimRight(u.EscapedPath(), "/") + "/"
	u.Path, _ = url.PathUnescape(escapedPath)
	u.RawPath = escapedPath
	return u, nil
}

// visibleASCII reports whether every character may safely be placed in an HTTP
// header value, which is the only place an API key is ever used. Rejecting
// whitespace and control characters here is what makes header injection
// through a rotated secret file impossible.
func visibleASCII(s string) bool {
	for _, ch := range s {
		if ch <= ' ' || ch > '~' {
			return false
		}
	}
	return true
}

func parseDurations(f fileConfig, c *Config) error {
	for _, entry := range []struct {
		name, value string
		target      *time.Duration
	}{
		{"poll_interval", f.PollInterval, &c.PollInterval},
		{"http_timeout", f.HTTPTimeout, &c.HTTPTimeout},
		{"delete_confirmation_timeout", f.DeleteConfirmationTimeout, &c.DeleteConfirmationTimeout},
		{"ui_refresh_interval", f.UIRefreshInterval, &c.UIRefreshInterval},
		{"readiness_max_age", f.ReadinessMaxAge, &c.ReadinessMaxAge},
	} {
		duration, err := time.ParseDuration(entry.value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("%s must be a positive duration", entry.name)
		}
		*entry.target = duration
	}
	if c.PollInterval > time.Duration(1<<63-1)/3 {
		return errors.New("poll_interval too large")
	}
	// The default observation gap tolerates two missed polls before an
	// episode is considered interrupted.
	c.MaxObservationGap = 3 * c.PollInterval
	if f.MaxObservationGap == nil {
		return nil
	}
	gap, err := time.ParseDuration(*f.MaxObservationGap)
	if err != nil || gap <= 0 {
		return errors.New("max_observation_gap must be a positive duration")
	}
	c.MaxObservationGap = gap
	return nil
}

func parsePolicies(configured map[PolicyID]filePolicy) (map[PolicyID]Policy, error) {
	for id := range configured {
		if !id.Valid() {
			return nil, errors.New("unknown policy; expected metadata, stalled_no_seeders, stalled_seeders_seen, stalled_partial, completed_no_data or stopped_arr_managed")
		}
	}
	policies := make(map[PolicyID]Policy, len(PolicyIDs()))
	for _, id := range PolicyIDs() {
		action, threshold, arrMode, matchTags := defaultPolicy(id)
		p := configured[id]
		if p.Action != nil {
			action = *p.Action
		}
		if p.Threshold != nil {
			threshold = *p.Threshold
		}
		if p.ArrMode != nil {
			arrMode = *p.ArrMode
		}
		if id != StoppedArrManaged && p.MatchTags.Present {
			return nil, errors.New("match_tags is only valid for stopped_arr_managed")
		}
		if p.MatchTags.Present {
			matchTags = normalizeList(p.MatchTags.Values)
		}
		if id == StoppedArrManaged && len(matchTags) == 0 {
			return nil, errors.New("stopped_arr_managed requires nonempty match_tags")
		}
		if !action.Valid() {
			return nil, errors.New("policy action must be warn, delete or delete_file")
		}
		if !arrMode.ValidForPolicy() {
			return nil, errors.New("policy arr_mode must be inherit, none, blocklist_and_search, blocklist_only or search_only")
		}
		duration, err := time.ParseDuration(threshold)
		if err != nil || duration <= 0 {
			return nil, errors.New("policy threshold must be a positive duration")
		}
		policies[id] = Policy{Action: action, Threshold: duration, ArrMode: arrMode, MatchTags: matchTags}
	}
	return policies, nil
}

func defaultPolicy(id PolicyID) (Action, string, ArrMode, []string) {
	switch id {
	case CompletedNoData:
		return Warn, "2m", InheritArrMode, nil
	case StoppedArrManaged:
		return Warn, "10m", InheritArrMode, []string{"Sonarr", "Radarr"}
	default:
		return Warn, "30m", InheritArrMode, nil
	}
}

// resolveSecrets reads password files on every load, so rotating a mounted
// secret is picked up by the same reload that notices the file change. The
// values are never returned in errors.
func resolveSecrets(f fileConfig, c *Config) error {
	if f.APIKey != "" && f.APIKeyFile != "" {
		return errors.New("qbt_api_key and qbt_api_key_file are mutually exclusive")
	}
	if (f.APIKey != "" || f.APIKeyFile != "") && (f.Username != "" || f.Password != "" || f.PasswordFile != "") {
		return errors.New("qBittorrent API-key and username/password authentication are mutually exclusive")
	}
	read := func(direct, path string) (string, error) {
		if direct != "" && path != "" {
			return "", errors.New("password and password_file are mutually exclusive")
		}
		if path == "" {
			return direct, nil
		}
		b, err := readBounded(path, 65536)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	var err error
	if c.APIKey, err = read(f.APIKey, f.APIKeyFile); err != nil {
		return err
	}
	if f.APIKeyFile != "" && c.APIKey == "" {
		return errors.New("qbt_api_key_file must contain a nonempty API key")
	}
	if !visibleASCII(c.APIKey) {
		return errors.New("qbt_api_key must contain only visible ASCII characters without whitespace")
	}
	if c.Password, err = read(f.Password, f.PasswordFile); err != nil {
		return err
	}
	return nil
}

func validate(c *Config) error {
	if err := validateTagSync(c.TagSync); err != nil {
		return err
	}
	for _, id := range PolicyIDs() {
		if _, ok := c.Policies[id]; !ok {
			return fmt.Errorf("missing default policy %s", id)
		}
	}
	if err := validateReservedTags("exclude_tags", c.ExcludeTags, c.TagSync.Prefix); err != nil {
		return err
	}
	for id, policy := range c.Policies {
		if err := validateReservedTags("policies."+string(id)+".match_tags", policy.MatchTags, c.TagSync.Prefix); err != nil {
			return err
		}
	}
	if c.MaxDeletions < 0 || c.MaxDeletions > 10000 {
		return errors.New("max_actions_per_poll must be between 0 and 10000")
	}
	if c.HistoryLimit < 1 || c.HistoryLimit > 10000 {
		return errors.New("history_limit must be between 1 and 10000")
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("credentials require both username and password")
	}
	if c.StateFile == "" {
		return errors.New("state_file cannot be empty")
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("invalid listen port")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		return errors.New("log_level must be debug, info, warn or error")
	}
	if !slices.Contains([]string{"json", "text", "console"}, c.LogFormat) {
		return errors.New("log_format must be json, text or console")
	}
	if !slices.Contains([]string{"auto", "always", "never"}, c.LogColor) {
		return errors.New("log_color must be auto, always or never")
	}
	if c.TLSCAFile == "" {
		return nil
	}
	c.TLSCAPEM, err = readBounded(c.TLSCAFile, 1024*1024)
	return err
}

func validateTagSync(t TagSync) error {
	if strings.TrimSpace(t.Prefix) == "" {
		return errors.New("tag_sync.prefix cannot be empty")
	}
	if t.Prefix != strings.TrimSpace(t.Prefix) {
		return errors.New("tag_sync.prefix must not have surrounding whitespace")
	}
	if strings.Contains(t.Prefix, ",") {
		return errors.New("tag_sync.prefix cannot contain commas")
	}
	if len(t.Prefix) < 4 {
		return errors.New("tag_sync.prefix must be at least four characters")
	}
	if t.MaxWritesPerPoll <= 0 || t.MaxWritesPerPoll > 10000 {
		return errors.New("tag_sync.max_writes_per_poll must be between 1 and 10000")
	}
	return nil
}

func validateReservedTags(field string, tags []string, prefix string) error {
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			return fmt.Errorf("%s cannot use reserved watchdog tag prefix", field)
		}
	}
	return nil
}

func reservedTagNearMiss(tag, prefix string) bool {
	return !strings.HasPrefix(tag, prefix) && strings.HasPrefix(strings.ToLower(tag), strings.ToLower(prefix))
}

// normalizeList trims surrounding whitespace and drops empties. Comparison
// stays case sensitive because qBittorrent category and tag names are.
func normalizeList(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Split parses a qBittorrent comma-separated tag list.
func Split(s string) []string { return normalizeList(strings.Split(s, ",")) }
