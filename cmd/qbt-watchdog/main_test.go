package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--help"}} {
		var out, err bytes.Buffer
		if code := run(args, &out, &err); code != 0 || out.Len() == 0 {
			t.Fatal(code, out.String(), err.String())
		}
	}
}

func TestOnceCycleAndConfigurationFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/app/version":
			_, _ = w.Write([]byte("5.0.1"))
		case "/api/v2/app/webapiVersion":
			_, _ = w.Write([]byte("2.11.2"))
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte(`[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"test","state":"metaDL","progress":0,"downloaded":0,"num_seeds":0}]`))
		default:
			t.Error("unexpected API", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("qbt_url=%q\nstate_file=%q", server.URL, filepath.Join(dir, "state.json"))), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--once", "--config=" + path}
	if code := run(args, &out, &stderr); code != 0 || !strings.Contains(out.String(), `"dry_run":true`) {
		t.Fatal(code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := run([]string{"--qbt-url=ftp://host"}, &out, &stderr); code != 2 {
		t.Fatal(code)
	}
	server.Close()
	out.Reset()
	stderr.Reset()
	if code := run(args, &out, &stderr); code != 1 || strings.Contains(stderr.String(), server.URL) {
		t.Fatal(code, stderr.String())
	}
}
func TestHealthcheck(t *testing.T) {
	for _, status := range []int{200, 503} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		var out bytes.Buffer
		code := healthcheck([]string{"--url", s.URL}, &out)
		s.Close()
		if (code == 0) != (status == 200) {
			t.Fatal(code, status)
		}
	}
	var out bytes.Buffer
	if healthcheck([]string{"--url", "http://user:SECRET@host"}, &out) == 0 || strings.Contains(out.String(), "SECRET") {
		t.Fatal("unsafe URL")
	}
}
