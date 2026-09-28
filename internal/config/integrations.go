package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ArrKind names one of the two supported media managers. The value is also the
// configuration key and the label used in logs and metrics, so it never needs
// translating between layers.
type ArrKind string

const (
	Sonarr ArrKind = "sonarr"
	Radarr ArrKind = "radarr"
)

// ArrMode decides how far the recovery workflow goes once a torrent has been
// identified in the media manager's queue.
type ArrMode string

const (
	// BlocklistAndSearch removes the queue item, blocklists the release so
	// the same one is not grabbed again, and then asks for a replacement.
	BlocklistAndSearch ArrMode = "blocklist_and_search"
	// SearchOnly asks for a replacement without blocklisting anything,
	// which suits operators who curate their own blocklists.
	SearchOnly ArrMode = "search_only"
)

func ArrModes() []ArrMode { return []ArrMode{BlocklistAndSearch, SearchOnly} }

func (m ArrMode) Valid() bool { return slices.Contains(ArrModes(), m) }

// Blocklists reports whether the release must be blocklisted before a
// replacement is requested.
func (m ArrMode) Blocklists() bool { return m == BlocklistAndSearch }

// ArrService is an immutable, fully validated media-manager endpoint. Like
// Config it is parsed once at the boundary: when Enabled is true the URL is a
// usable base URL and APIKey is a nonempty visible-ASCII credential, so no
// caller ever re-checks either.
//
// APIKey is json:"-" for the same reason Config.APIKey is: the snapshot is
// marshalled into the status payload and must never carry a credential.
type ArrService struct {
	// URL is the base URL, normalised to a trailing slash so a reverse
	// proxy base path such as https://host/sonarr/ survives path joining.
	URL    *url.URL
	APIKey string `json:"-" yaml:"-" toml:"-"`

	// Categories restricts this service to the listed qBittorrent
	// categories. Empty means every category. Comparison is case
	// sensitive, because qBittorrent category names are.
	Categories []string

	Kind    ArrKind
	Mode    ArrMode
	Timeout time.Duration
	Enabled bool
}

func (s ArrService) Clone() ArrService {
	if s.URL != nil {
		u := *s.URL
		s.URL = &u
	}
	s.Categories = slices.Clone(s.Categories)
	return s
}

// Equal includes credentials for in-memory reload decisions, never persistence.
func (s ArrService) Equal(other ArrService) bool {
	return s.Kind == other.Kind && s.Enabled == other.Enabled && s.Mode == other.Mode &&
		s.Timeout == other.Timeout && s.APIKey == other.APIKey &&
		slices.Equal(s.Categories, other.Categories) && s.endpoint() == other.endpoint()
}

func (s ArrService) endpoint() string {
	if s.URL == nil {
		return ""
	}
	return s.URL.String()
}

// EndpointKey identifies the instance for durable jobs, independent of key rotation.
func (s ArrService) EndpointKey() string {
	h := sha256.Sum256([]byte(string(s.Kind) + "\x00" + s.endpoint()))
	return hex.EncodeToString(h[:])
}

func (s ArrService) String() string   { return "ArrService{credentials redacted}" }
func (s ArrService) GoString() string { return s.String() }

// Validated re-parses a programmatically constructed service without reading files.
func (s ArrService) Validated() (ArrService, error) {
	if s.Kind != Sonarr && s.Kind != Radarr {
		return ArrService{}, fmt.Errorf("media manager kind must be sonarr or radarr")
	}
	return parseArrService(s.Kind, &fileArr{
		Enabled: s.Enabled, URL: s.endpoint(), APIKey: s.APIKey,
		Categories: s.Categories, Mode: s.Mode, Timeout: s.Timeout.String(),
	})
}

// Handles reports whether this service is responsible for a torrent in the
// given qBittorrent category.
func (s ArrService) Handles(category string) bool {
	return len(s.Categories) == 0 || slices.Contains(s.Categories, category)
}

// Integrations groups every media manager the watchdog can talk to. Both
// services always exist; a service that the file never mentions is simply
// disabled, so callers branch on Enabled rather than on presence.
type Integrations struct {
	Sonarr, Radarr ArrService
}

func (i Integrations) Clone() Integrations {
	return Integrations{Sonarr: i.Sonarr.Clone(), Radarr: i.Radarr.Clone()}
}

func (i Integrations) Equal(other Integrations) bool {
	return i.Sonarr.Equal(other.Sonarr) && i.Radarr.Equal(other.Radarr)
}

// Services lists both services in a stable order, enabled or not.
func (i Integrations) Services() []ArrService { return []ArrService{i.Sonarr, i.Radarr} }

// Active lists only the services the operator has switched on, in a stable
// order, so a caller iterating them produces deterministic logs.
func (i Integrations) Active() []ArrService {
	active := []ArrService{}
	for _, service := range i.Services() {
		if service.Enabled {
			active = append(active, service)
		}
	}
	return active
}

// Any reports whether any media manager is configured at all, which is the
// single check that decides whether the recovery workflow exists at runtime.
func (i Integrations) Any() bool { return len(i.Active()) > 0 }

// Key is a digest of non-secret integration settings. For a durable job tied
// to one instance, use ArrService.EndpointKey instead. Use Equal for reloads.
//
// It exists so durable per-torrent recovery markers can be invalidated when
// the operator repoints an integration: a marker saying "blocklisted in
// Sonarr" is meaningless once Sonarr is a different instance. It is
// deliberately NOT part of SafetyKey, because integration settings can only
// ever add a recovery step and can never shorten a cleanup clock; changing
// them must not reset the policy timers.
func (i Integrations) Key() string {
	type fingerprint struct {
		URL        string
		Kind       ArrKind
		Mode       ArrMode
		Categories []string
		Timeout    time.Duration
		Enabled    bool
	}
	prints := make([]fingerprint, 0, 2)
	for _, service := range i.Services() {
		endpoint := ""
		if service.URL != nil {
			endpoint = service.URL.String()
		}
		prints = append(prints, fingerprint{endpoint, service.Kind, service.Mode, service.Categories, service.Timeout, service.Enabled})
	}
	b, _ := json.Marshal(prints)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// IntegrationsKey mirrors SafetyKey and EndpointKey so every persisted
// identity digest is reached the same way.
func (c Config) IntegrationsKey() string { return c.Integrations.Key() }

// fileIntegrations mirrors the `integrations` block one-to-one. Both services
// are pre-populated with their defaults in defaults(), so a file that sets
// only `enabled` still gets the default mode and timeout.
type fileIntegrations struct {
	Sonarr *fileArr `json:"sonarr"`
	Radarr *fileArr `json:"radarr"`
}

type fileArr struct {
	Enabled    bool     `json:"enabled"`
	URL        string   `json:"url"`
	APIKey     string   `json:"api_key"`
	APIKeyFile string   `json:"api_key_file"`
	Categories []string `json:"categories"`
	Mode       ArrMode  `json:"mode"`
	Timeout    string   `json:"timeout"`
}

func defaultArr() *fileArr { return &fileArr{Mode: BlocklistAndSearch, Timeout: "10s"} }

func defaultIntegrations() *fileIntegrations {
	return &fileIntegrations{Sonarr: defaultArr(), Radarr: defaultArr()}
}

func parseIntegrations(f *fileIntegrations) (Integrations, error) {
	if f == nil {
		f = &fileIntegrations{}
	}
	sonarr, err := parseArrService(Sonarr, f.Sonarr)
	if err != nil {
		return Integrations{}, err
	}
	radarr, err := parseArrService(Radarr, f.Radarr)
	if err != nil {
		return Integrations{}, err
	}
	return Integrations{Sonarr: sonarr, Radarr: radarr}, nil
}

// parseArrService turns one service block into a trusted value.
//
// Structural settings — the mode, the timeout, the URL syntax and the mutual
// exclusion of the two credential sources — are validated even while the
// service is disabled, so a typo in a block that is switched off is still
// reported instead of waiting until the day it is switched on. Only the
// requirement that a URL and a credential be *present* is conditional on
// Enabled, so a disabled stub may be left blank.
func parseArrService(kind ArrKind, f *fileArr) (ArrService, error) {
	service := ArrService{Kind: kind, Mode: BlocklistAndSearch, Timeout: 10 * time.Second}
	if f == nil {
		return service, nil
	}
	field := func(name string) string { return "integrations." + string(kind) + "." + name }
	if !f.Mode.Valid() {
		return ArrService{}, fmt.Errorf("%s must be blocklist_and_search or search_only", field("mode"))
	}
	timeout, err := time.ParseDuration(f.Timeout)
	if err != nil || timeout <= 0 || timeout > 5*time.Minute {
		return ArrService{}, fmt.Errorf("%s must be a positive duration of at most 5m", field("timeout"))
	}
	key, err := resolveArrKey(field, f.APIKey, f.APIKeyFile)
	if err != nil {
		return ArrService{}, err
	}
	if f.URL != "" {
		if service.URL, err = parseBaseURL(field("url"), f.URL); err != nil {
			return ArrService{}, err
		}
	}
	service.Enabled, service.Mode, service.Timeout, service.APIKey = f.Enabled, f.Mode, timeout, key
	service.Categories = normalizeList(f.Categories)
	if !service.Enabled {
		return service, nil
	}
	if service.URL == nil {
		return ArrService{}, fmt.Errorf("%s is required while the integration is enabled", field("url"))
	}
	if service.APIKey == "" {
		return ArrService{}, fmt.Errorf("%s or %s is required while the integration is enabled", field("api_key"), field("api_key_file"))
	}
	return service, nil
}

// resolveArrKey reads the credential on every load so a rotated mounted secret
// is picked up by the same reload that notices the file change. Neither the
// key nor the file contents ever reach an error string.
func resolveArrKey(field func(string) string, direct, path string) (string, error) {
	if direct != "" && path != "" {
		return "", fmt.Errorf("%s and %s are mutually exclusive", field("api_key"), field("api_key_file"))
	}
	key := direct
	if path != "" {
		b, err := readBounded(path, 65536)
		if err != nil {
			return "", err
		}
		if key = strings.TrimRight(string(b), "\r\n"); key == "" {
			return "", fmt.Errorf("%s must contain a nonempty API key", field("api_key_file"))
		}
	}
	if !visibleASCII(key) {
		return "", fmt.Errorf("%s must contain only visible ASCII characters without whitespace", field("api_key"))
	}
	return key, nil
}
