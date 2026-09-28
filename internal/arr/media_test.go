package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

func TestMediaFileStatusRequiresExactExplicitIdentity(t *testing.T) {
	for _, kind := range []config.ArrKind{config.Sonarr, config.Radarr} {
		for _, body := range []string{`{"id":11,"hasFile":true}`, `{"id":11,"hasFile":false}`, `{"id":12,"hasFile":false}`, `{"id":11}`, `{"id":11,"hasFile":null}`, `{"id":11,"hasFile":"false"}`} {
			t.Run(string(kind)+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					path := "/api/v3/movie/11"
					if kind == config.Sonarr {
						path = "/api/v3/episode/11"
					}
					if r.Method != "GET" || r.URL.Path != path {
						t.Error("unexpected media request")
					}
					_, _ = fmt.Fprint(w, body)
				}))
				defer server.Close()
				u, _ := url.Parse(server.URL + "/")
				client, err := New(config.ArrService{Kind: kind, URL: u, APIKey: "test", Enabled: true, Mode: config.SearchOnly, Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseIdleConnections()
				hasFile, err := client.HasFile(context.Background(), 11)
				valid := body == `{"id":11,"hasFile":true}` || body == `{"id":11,"hasFile":false}`
				if (err == nil) != valid || (err == nil && hasFile != (body == `{"id":11,"hasFile":true}`)) {
					t.Fatal("unsafe file status", hasFile, err)
				}
			})
		}
	}
}
