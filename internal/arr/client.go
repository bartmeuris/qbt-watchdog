// Package arr talks to the stable Sonarr v4 and Radarr v3 APIs, both of which
// are served under /api/v3.
//
// The package does exactly two things: it turns a media manager's queue into
// trusted identity facts, and it performs the individual calls that recover a
// stuck download. It owns no policy. There are no retries here, no backoff, no
// scheduling and no persistence: every call happens once and reports precisely
// what is known about its effect (see Outcome), leaving the caller free to
// decide what is worth repeating and what must be written down first.
//
// Two safety rules shape the whole package. A torrent is matched to a queue
// row only through the download id, never through a release name, because a
// mismatch here blocklists somebody else's download. And nothing that touches
// the API key ever reaches a log, an error or a metric: the key is written
// into one header and is otherwise untouched, and error text is drawn from a
// fixed vocabulary rather than from server responses.
package arr

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"qbt-watchdog/internal/config"
)

const (
	// apiPath is shared: Sonarr v4 and Radarr v3 both serve /api/v3.
	apiPath = "api/v3/"
	// userAgent identifies the watchdog in an instance's access log, so an
	// operator can tell which client removed a queue item.
	userAgent = "qbt-watchdog/1"
	// maxResponse bounds a single response body. A queue page of 200 rows
	// is a few hundred kilobytes; anything near this ceiling is a fault.
	maxResponse = 8 << 20
	// queuePageSize and maxQueuePages bound a full queue walk. Their
	// product is the largest queue that can be read; a larger one fails
	// loudly, because a silently truncated queue is indistinguishable from
	// "this torrent is not managed here".
	queuePageSize = 200
	maxQueuePages = 25
	// historyPageSize bounds the read-only history lookup, which is only
	// ever used to confirm what recently happened to one download id.
	historyPageSize = 50
)

// Command status values as reported by CommandResource.
const (
	StatusQueued    = "queued"
	StatusStarted   = "started"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusAborted   = "aborted"
)

// Command is a media manager background command, reduced to the identity and
// state a caller needs to follow it across restarts.
type Command struct {
	ID     int64
	Name   string
	Status string
}

// Finished reports whether the command has reached a terminal state. An
// unrecognised status is treated as still running, so a caller waits rather
// than concluding.
func (c Command) Finished() bool {
	return c.Status == StatusCompleted || c.Status == StatusFailed || c.Status == StatusAborted
}

// Succeeded reports whether the command finished and did so successfully.
func (c Command) Succeeded() bool { return c.Status == StatusCompleted }

func normalizeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "", StatusQueued, StatusStarted, StatusCompleted, StatusFailed, StatusAborted:
		return status
	default:
		return "unknown"
	}
}

// HistoryRecord is one read-only history entry for a download id.
type HistoryRecord struct {
	ID         int64
	DownloadID string
	EventType  string
	MediaID    *int64
	Date       time.Time
}

// Client is one media manager instance. It is immutable after construction:
// changing an endpoint, a credential or a timeout means building a new client,
// never mutating this one, so an in-flight poll can never observe a half
// applied reload.
type Client struct {
	http    *http.Client
	base    *url.URL
	apiKey  string
	kind    config.ArrKind
	mode    config.ArrMode
	service string
	log     *slog.Logger
}

// New builds a client for one configured service.
//
// The configuration layer has already guaranteed these invariants for every
// enabled service; they are re-asserted here because New is also reachable
// from code that builds an ArrService by hand, and a client without a
// credential would otherwise fail much later and much more confusingly.
func New(s config.ArrService, logger ...*slog.Logger) (*Client, error) {
	var err error
	if s, err = s.Validated(); err != nil {
		return nil, err
	}
	if !s.Enabled {
		return nil, errors.New("media manager is disabled")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if len(logger) > 0 && logger[0] != nil {
		log = logger[0]
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	base := *s.URL
	return &Client{
		// Redirects are refused rather than followed: a redirect would
		// replay the API key at whatever host the response names.
		http: &http.Client{
			Timeout:       s.Timeout,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base: &base, apiKey: s.APIKey, kind: s.Kind, mode: s.Mode,
		service: string(s.Kind), log: log,
	}, nil
}

func (c *Client) Kind() config.ArrKind { return c.kind }
func (c Client) String() string        { return "ArrClient{credentials redacted}" }
func (c Client) GoString() string      { return c.String() }
func (c *Client) EndpointKey() string {
	return (config.ArrService{Kind: c.kind, URL: c.base}).EndpointKey()
}

// Map uses a previously fetched queue snapshot without performing I/O.
func (c *Client) Map(items []QueueItem, hash string) (Job, bool) { return Match(items, hash) }

// RemoveJob issues at most one blocklist call for a tracked download.
// A NotFound result requires read-only reconciliation, not another row DELETE.
func (c *Client) RemoveJob(ctx context.Context, job Job) error {
	id, ok := job.RemovalID()
	if !ok {
		return (&call{op: "remove"}).reject(c.service, "job identity is incomplete or exceeds the limit")
	}
	return c.Remove(ctx, id)
}
func (c *Client) Mode() config.ArrMode  { return c.mode }
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }
func (c *Client) endpoint(path string, query url.Values) string {
	u := c.base.ResolveReference(&url.URL{Path: apiPath + path})
	u.RawQuery = query.Encode()
	return u.String()
}

// call is one request, described completely. mutating is the single input to
// every ambiguity decision, so it is stated at the call site rather than
// inferred from the HTTP method.
type call struct {
	op     string
	method string
	path   string
	query  url.Values
	body   []byte
	// mutating drives every ambiguity decision, so it is stated rather
	// than inferred from the HTTP method.
	mutating bool
	// readsBody marks a call whose answer is in the payload. For a call
	// without it the status line is the entire answer, and a body that
	// cannot be read afterwards changes nothing about what happened.
	readsBody bool
}

// request performs one call and returns either its body or a classified
// *Error. Neither the URL, the key, nor the response body ever reaches the
// error: a caller that logs the error logs nothing an operator must redact.
func (c *Client) request(ctx context.Context, spec call) ([]byte, error) {
	var body io.Reader
	if spec.body != nil {
		body = bytes.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, c.endpoint(spec.path, spec.query), body)
	if err != nil {
		return nil, spec.reject(c.service, "cannot construct API request")
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if spec.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, spec.classifyTransport(c.service, err)
	}
	defer resp.Body.Close()
	c.log.Debug("media manager request completed", "event", "arr_request", "service", c.service, "op", spec.op, "status", resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, spec.classifyStatus(c.service, resp.StatusCode)
	}
	// The instance has answered, so the request was applied. Downgrading a
	// mutation to a failure now would invite a retry that blocklists the
	// replacement release instead of the original.
	if !spec.readsBody {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		if spec.mutating {
			return nil, spec.ambiguous(c.service, "request was accepted but its response could not be read")
		}
		return nil, spec.classifyTransport(c.service, err)
	}
	if len(data) > maxResponse {
		if spec.mutating {
			return nil, spec.ambiguous(c.service, "request was accepted but its response exceeds the size limit")
		}
		return nil, spec.reject(c.service, "response exceeds the size limit")
	}
	return data, nil
}

// Queue reads the whole queue, one bounded page at a time.
//
// The result is the raw row set rather than grouped jobs, because a caller
// polling many torrents reads the queue once and matches repeatedly against
// it; see Match and Group.
func (c *Client) Queue(ctx context.Context) ([]QueueItem, error) {
	unknownRows := "includeUnknownMovieItems"
	if c.kind == config.Sonarr {
		unknownRows = "includeUnknownSeriesItems"
	}
	items := []QueueItem{}
	total, received := -1, 0
	seen := map[int64]int{}
	identities := map[int64]string{}
	for page := 1; page <= maxQueuePages; page++ {
		spec := call{op: "queue", method: http.MethodGet, path: "queue", readsBody: true, query: url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(queuePageSize)},
			"sortKey":  {"id"}, "sortDirection": {"ascending"},
			unknownRows: {"true"},
		}}
		data, err := c.request(ctx, spec)
		if err != nil {
			return nil, err
		}
		var decoded pagedResponse[queueRecord]
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, spec.reject(c.service, "queue response is not valid JSON")
		}
		if decoded.Records == nil || len(decoded.Records) > queuePageSize ||
			(decoded.Page != nil && *decoded.Page != page) ||
			(decoded.PageSize != nil && *decoded.PageSize != queuePageSize) {
			return nil, spec.reject(c.service, "queue pagination is invalid")
		}
		if decoded.TotalRecords != nil {
			if *decoded.TotalRecords < 0 || (page > 1 && total != *decoded.TotalRecords) {
				return nil, spec.reject(c.service, "queue changed during pagination")
			}
			total = *decoded.TotalRecords
		} else if total >= 0 {
			return nil, spec.reject(c.service, "queue pagination lost its total")
		}
		// A queue larger than the walk can cover must fail loudly: a
		// truncated queue looks exactly like an unmanaged torrent, and
		// would let the cleanup delete something recoverable.
		if decoded.TotalRecords != nil && *decoded.TotalRecords > maxQueuePages*queuePageSize {
			return nil, spec.reject(c.service, "queue is larger than the supported page limit")
		}
		for _, record := range decoded.Records {
			if record.ID == nil || *record.ID <= 0 || strings.TrimSpace(string(record.DownloadID)) == "" {
				return nil, spec.reject(c.service, "queue row lacks identity")
			}
			if previous, exists := seen[*record.ID]; exists && previous != page {
				return nil, spec.reject(c.service, "queue pages overlap")
			}
			identity := strings.ToLower(string(record.DownloadID))
			if previous, exists := identities[*record.ID]; exists && previous != identity {
				return nil, spec.reject(c.service, "queue row has conflicting identities")
			}
			identities[*record.ID] = identity
			seen[*record.ID] = page
			if item, usable := record.item(c.kind); usable {
				items = append(items, item)
			}
		}
		received += len(decoded.Records)
		if total >= 0 && (received > total || (len(decoded.Records) < queuePageSize && received != total)) {
			return nil, spec.reject(c.service, "queue pagination is incomplete or inconsistent")
		}
		if len(decoded.Records) < queuePageSize || received == total {
			return items, nil
		}
	}
	return nil, (&call{op: "queue"}).reject(c.service, "queue is larger than the supported page limit")
}

// Remove blocklists a release and drops its queue row without touching the
// torrent in qBittorrent.
//
// The parameters are the contract with the rest of the watchdog:
// removeFromClient=false because the watchdog, not the media manager, owns the
// torrent's lifetime and deletes it under its own safety rules;
// blocklist=true so the same release is not grabbed again; skipRedownload=true
// so the media manager does not start its own search in parallel with the one
// this package issues explicitly; changeCategory=false leaves category routing
// untouched.
//
// A vanished item is reported as NotFound, not as a failure: the media
// manager's import loop removes queue rows on its own schedule, so losing the
// race is normal. It is not proof that the release was blocklisted: callers
// must reconcile read-only rather than trying every remaining pack row.
func (c *Client) Remove(ctx context.Context, itemID int64) error {
	spec := call{
		op: "remove", method: http.MethodDelete, path: "queue/" + strconv.FormatInt(itemID, 10),
		query: url.Values{"removeFromClient": {"false"}, "blocklist": {"true"}, "skipRedownload": {"true"}, "changeCategory": {"false"}},
		// A DELETE that times out may already have blocklisted the
		// release, so it must never be classified as "nothing happened".
		mutating: true,
	}
	if itemID <= 0 {
		return spec.reject(c.service, "queue item id is not valid")
	}
	_, err := c.request(ctx, spec)
	return err
}

// Search asks for a replacement for the given episodes (Sonarr) or movies
// (Radarr) with a single command, and returns the command's identity so the
// caller can follow it later with Status.
//
// An empty or oversized id list is refused here rather than sent: a search
// command with no ids is a request to search for nothing, and an unbounded one
// is a request the instance may never finish.
func (c *Client) Search(ctx context.Context, mediaIDs []int64) (Command, error) {
	spec := call{op: "search", method: http.MethodPost, path: "command", mutating: true, readsBody: true}
	if len(mediaIDs) == 0 {
		return Command{}, spec.reject(c.service, "search requires at least one episode or movie id")
	}
	if len(mediaIDs) > MaxIDsPerJob {
		return Command{}, spec.reject(c.service, "search exceeds the supported id limit")
	}
	for _, id := range mediaIDs {
		if id <= 0 {
			return Command{}, spec.reject(c.service, "search id is not valid")
		}
	}
	mediaIDs, _ = bound(slices.Clone(mediaIDs))
	request := commandRequest{Name: "MoviesSearch", MovieIDs: mediaIDs}
	if c.kind == config.Sonarr {
		request = commandRequest{Name: "EpisodeSearch", EpisodeIDs: mediaIDs}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Command{}, spec.reject(c.service, "cannot encode search command")
	}
	spec.body = body
	data, err := c.request(ctx, spec)
	if err != nil {
		return Command{}, err
	}
	// The command was accepted; only its identity is in doubt. That is
	// ambiguous rather than failed, because the search is now running and
	// re-issuing it would duplicate work.
	var decoded commandResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Command{}, spec.ambiguous(c.service, "search was accepted but its response could not be read")
	}
	command, identified := decoded.command()
	if !identified {
		return Command{}, spec.ambiguous(c.service, "search was accepted without a usable command id")
	}
	return command, nil
}

// Status reads a command's current state. It is read-only, so a lost answer is
// never ambiguous: nothing was changed by asking.
func (c *Client) Status(ctx context.Context, commandID int64) (Command, error) {
	spec := call{op: "command", method: http.MethodGet, path: "command/" + strconv.FormatInt(commandID, 10), readsBody: true}
	if commandID <= 0 {
		return Command{}, spec.reject(c.service, "command id is not valid")
	}
	data, err := c.request(ctx, spec)
	if err != nil {
		return Command{}, err
	}
	var decoded commandResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Command{}, spec.reject(c.service, "command response is not valid JSON")
	}
	command, identified := decoded.command()
	if !identified || command.ID != commandID {
		return Command{}, spec.reject(c.service, "command response has no usable command id")
	}
	return command, nil
}

// History returns the most recent history entries for one download id.
//
// The filter is sent to the instance and applied again locally. The
// server-side parameter is the fast path; the local pass is what makes the
// result correct even if an instance ignores the parameter, which would
// otherwise hand the caller another download's history.
func (c *Client) History(ctx context.Context, downloadID string) ([]HistoryRecord, error) {
	wanted := NormalizeDownloadID(downloadID)
	spec := call{op: "history", method: http.MethodGet, path: "history", readsBody: true, query: url.Values{
		"page":          {"1"},
		"pageSize":      {strconv.Itoa(historyPageSize)},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
		"downloadId":    {wanted},
	}}
	if wanted == "" {
		return nil, spec.reject(c.service, "download id is not valid")
	}
	data, err := c.request(ctx, spec)
	if err != nil {
		return nil, err
	}
	var decoded pagedResponse[historyRecord]
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, spec.reject(c.service, "history response is not valid JSON")
	}
	if decoded.Records == nil || len(decoded.Records) > historyPageSize ||
		(decoded.Page != nil && *decoded.Page != 1) ||
		(decoded.PageSize != nil && *decoded.PageSize != historyPageSize) ||
		(decoded.TotalRecords != nil && (*decoded.TotalRecords < 0 || *decoded.TotalRecords > historyPageSize || *decoded.TotalRecords != len(decoded.Records))) {
		return nil, spec.reject(c.service, "history pagination is invalid")
	}
	records := []HistoryRecord{}
	for _, record := range decoded.Records {
		entry, usable := record.record(c.kind)
		if !usable && NormalizeDownloadID(string(record.DownloadID)) == wanted {
			return nil, spec.reject(c.service, "history record lacks identity")
		}
		if usable && entry.DownloadID == wanted {
			records = append(records, entry)
		}
	}
	return records, nil
}
