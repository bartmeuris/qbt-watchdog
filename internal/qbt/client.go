package qbt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"

	"qbt-watchdog/internal/config"
)

type Torrent struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	Downloaded    int64   `json:"downloaded"`
	Size          int64   `json:"size"`
	TotalSize     int64   `json:"total_size"`
	Completed     int64   `json:"completed"`
	AmountLeft    int64   `json:"amount_left"`
	DownloadSpeed int64   `json:"dlspeed"`
	NumSeeds      int     `json:"num_seeds"`
	NumLeechers   int     `json:"num_leechs"`
	AddedOn       int64   `json:"added_on"`
	Category      string  `json:"category"`
	Tags          string  `json:"tags"`
}

var hashPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)

func ValidHash(hash string) bool { return hashPattern.MatchString(hash) }
func ShortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

type Client struct {
	http               *http.Client
	base               *url.URL
	username, password string
	apiKey             string
	loggedIn           bool
	log                *slog.Logger
}

func New(c config.Config, logger ...*slog.Logger) (*Client, error) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if len(logger) > 0 {
		log = logger[0]
	}
	var jar http.CookieJar
	if c.APIKey == "" {
		jar, _ = cookiejar.New(nil)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.TLSInsecure} // explicit development-only opt-in
	if c.TLSCAFile != "" {
		data := c.TLSCAPEM
		var err error
		if data == nil {
			data, err = os.ReadFile(c.TLSCAFile)
		}
		if err != nil {
			return nil, errors.New("cannot read custom CA bundle")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("custom CA bundle contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &Client{http: &http.Client{Timeout: c.HTTPTimeout, Jar: jar, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: c.URL, username: c.Username, password: c.Password, apiKey: c.APIKey, log: log}, nil
}

func (c *Client) endpoint(path string, query url.Values) *url.URL {
	u := c.base.ResolveReference(&url.URL{Path: "api/v2/" + path})
	u.RawQuery = query.Encode()
	return u
}
func (c *Client) raw(ctx context.Context, method, path string, query, form url.Values) ([]byte, int, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query).String(), body)
	if err != nil {
		return nil, 0, errors.New("cannot construct API request")
	}
	req.Header.Set("User-Agent", "qbt-watchdog/1")
	req.Header.Set("Referer", c.base.String())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, errors.New("qBittorrent request failed (network or TLS)")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("cannot read API response")
	}
	if len(data) > 16*1024*1024 {
		return nil, resp.StatusCode, errors.New("API response exceeds size limit")
	}
	return data, resp.StatusCode, nil
}

func (c *Client) invalidate()           { c.loggedIn = false; c.http.Jar, _ = cookiejar.New(nil) }
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }
func (c *Client) login(ctx context.Context) error {
	c.invalidate()
	data, status, err := c.raw(ctx, http.MethodPost, "auth/login", nil, url.Values{"username": {c.username}, "password": {c.password}})
	if err != nil {
		return err
	}
	if status != 200 || strings.TrimSpace(string(data)) != "Ok." {
		c.invalidate()
		c.log.Warn("qBittorrent authentication rejected", "event", "qbt_authentication_failed")
		return errors.New("qBittorrent authentication rejected")
	}
	for _, cookie := range c.http.Jar.Cookies(c.endpoint("torrents/info", nil)) {
		if cookie.Name == "SID" && cookie.Value != "" {
			c.loggedIn = true
			c.log.Info("qBittorrent authenticated", "event", "qbt_authenticated")
			return nil
		}
	}
	c.invalidate()
	return errors.New("qBittorrent authentication missing session cookie")
}

func (c *Client) request(ctx context.Context, method, path string, query, form url.Values) ([]byte, error) {
	if c.apiKey == "" && c.username != "" && !c.loggedIn {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		data, status, err := c.raw(ctx, method, path, query, form)
		if err != nil {
			return nil, err
		}
		if status == 200 {
			return data, nil
		}
		if status != 401 && status != 403 {
			return nil, fmt.Errorf("qBittorrent API returned HTTP %d", status)
		}
		if c.apiKey != "" {
			return nil, errors.New("qBittorrent API-key authentication failed; check key permissions, expiry and server support")
		}
		c.invalidate()
		if c.username == "" || attempt == 1 {
			return nil, errors.New("qBittorrent API authentication failed")
		}
		c.log.Info("qBittorrent session expired; retrying authentication once", "event", "qbt_reauthenticate")
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		if method != http.MethodGet && method != http.MethodHead {
			return nil, errors.New("qBittorrent session renewed; mutation not retried, fresh safety check required")
		}
	}
	return nil, errors.New("qBittorrent authentication retry exhausted")
}

func (c *Client) Versions(ctx context.Context) (string, string, error) {
	a, e := c.request(ctx, "GET", "app/version", nil, nil)
	if e != nil {
		return "", "", e
	}
	b, e := c.request(ctx, "GET", "app/webapiVersion", nil, nil)
	if e != nil {
		return "", "", e
	}
	if !versionPattern.Match(a) || !versionPattern.Match(b) {
		return "", "", errors.New("invalid qBittorrent version response")
	}
	return strings.TrimSpace(string(a)), strings.TrimSpace(string(b)), nil
}

var versionPattern = regexp.MustCompile(`^[vV]?[0-9][a-zA-Z0-9.\-+ \r\n]{0,63}$`)

func (c *Client) list(ctx context.Context, query url.Values) ([]Torrent, error) {
	data, err := c.request(ctx, "GET", "torrents/info", query, nil)
	if err != nil {
		return nil, err
	}
	var decoded []struct {
		Torrent
		Progress   *float64 `json:"progress"`
		Downloaded *int64   `json:"downloaded"`
		Size       *int64   `json:"size"`
		TotalSize  *int64   `json:"total_size"`
		AmountLeft *int64   `json:"amount_left"`
		NumSeeds   *int     `json:"num_seeds"`
	}
	if err = json.Unmarshal(data, &decoded); err != nil || decoded == nil {
		return nil, errors.New("invalid torrent list response")
	}
	torrents := make([]Torrent, 0, len(decoded))
	for _, entry := range decoded {
		if entry.Progress == nil || entry.Downloaded == nil || entry.Size == nil || entry.TotalSize == nil || entry.AmountLeft == nil || entry.NumSeeds == nil || *entry.NumSeeds < 0 {
			return nil, errors.New("torrent response lacks required safety fields")
		}
		entry.Torrent.Progress = *entry.Progress
		entry.Torrent.Downloaded = *entry.Downloaded
		entry.Torrent.Size = *entry.Size
		entry.Torrent.TotalSize = *entry.TotalSize
		entry.Torrent.AmountLeft = *entry.AmountLeft
		entry.Torrent.NumSeeds = *entry.NumSeeds
		torrents = append(torrents, entry.Torrent)
	}
	seen := map[string]bool{}
	for i := range torrents {
		t := &torrents[i]
		t.Hash = strings.ToLower(t.Hash)
		if !ValidHash(t.Hash) || seen[t.Hash] || t.State == "" || math.IsNaN(t.Progress) || math.IsInf(t.Progress, 0) || t.Progress < 0 || t.Progress > 1 || t.Downloaded < 0 || t.Size < 0 || t.TotalSize < 0 || t.Completed < 0 || t.AmountLeft < 0 || t.Size > t.TotalSize || t.AddedOn > 253402300799 {
			return nil, errors.New("invalid torrent data")
		}
		seen[t.Hash] = true
	}
	return torrents, nil
}
func (c *Client) List(ctx context.Context) ([]Torrent, error) { return c.list(ctx, nil) }
func (c *Client) Get(ctx context.Context, hash string) (*Torrent, error) {
	if !ValidHash(hash) {
		return nil, errors.New("invalid torrent identifier")
	}
	ts, e := c.list(ctx, url.Values{"hashes": {hash}})
	if e != nil {
		return nil, e
	}
	if len(ts) == 0 {
		return nil, nil
	}
	if len(ts) != 1 || ts[0].Hash != hash {
		return nil, errors.New("targeted torrent response mismatch")
	}
	return &ts[0], nil
}
func (c *Client) Delete(ctx context.Context, hash string, files bool) error {
	if !ValidHash(hash) {
		return errors.New("invalid torrent identifier")
	}
	_, err := c.request(ctx, "POST", "torrents/delete", nil, url.Values{"hashes": {hash}, "deleteFiles": {fmt.Sprint(files)}})
	return err
}

func (c *Client) AddTags(ctx context.Context, hashes []string, tag string) error {
	return c.updateTags(ctx, "torrents/addTags", hashes, tag)
}

func (c *Client) RemoveTags(ctx context.Context, hashes []string, tag string) error {
	return c.updateTags(ctx, "torrents/removeTags", hashes, tag)
}

func (c *Client) updateTags(ctx context.Context, path string, hashes []string, tag string) error {
	if len(hashes) == 0 {
		return nil
	}
	if strings.TrimSpace(tag) == "" || strings.TrimSpace(tag) != tag || strings.Contains(tag, ",") {
		return errors.New("invalid qBittorrent tag")
	}
	for _, hash := range hashes {
		if !ValidHash(hash) {
			return errors.New("invalid torrent identifier")
		}
	}
	_, err := c.request(ctx, "POST", path, nil, url.Values{"hashes": {strings.Join(hashes, "|")}, "tags": {tag}})
	return err
}
