package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"qbt-watchdog/internal/config"
)

// HasFile requires an explicit hasFile boolean and the requested identity.
// Missing fields are unknown, never evidence that searching is safe.
func (c *Client) HasFile(ctx context.Context, id int64) (bool, error) {
	resource := "movie/"
	if c.kind == config.Sonarr {
		resource = "episode/"
	}
	spec := call{op: "media", method: http.MethodGet, path: resource + strconv.FormatInt(id, 10), readsBody: true}
	if id <= 0 {
		return false, spec.reject(c.service, "media id is not valid")
	}
	data, err := c.request(ctx, spec)
	if err != nil {
		return false, err
	}
	var record struct {
		ID      int64 `json:"id"`
		HasFile *bool `json:"hasFile"`
	}
	if json.Unmarshal(data, &record) != nil || record.ID != id || record.HasFile == nil {
		return false, spec.reject(c.service, "media status lacks identity or file status")
	}
	return *record.HasFile, nil
}
