package qbt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

const hash = "0123456789abcdef0123456789abcdef01234567"

func client(t *testing.T, server *httptest.Server, auth bool) *Client {
	t.Helper()
	u, _ := url.Parse(server.URL + "/")
	c := config.Config{URL: u, HTTPTimeout: time.Second}
	if auth {
		c.Username = "user"
		c.Password = "secret"
	}
	cl, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	return cl
}
func TestAuthenticationCookieReuseAndReauthentication(t *testing.T) {
	logins, lists := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Referer") == "" {
			t.Error("missing headers")
		}
		if r.URL.Path == "/api/v2/auth/login" {
			logins++
			if r.Method != "POST" {
				t.Error("login method")
			}
			_ = r.ParseForm()
			if r.Form.Get("username") != "user" || r.Form.Get("password") != "secret" {
				t.Error("credentials")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
			io.WriteString(w, "Ok.")
			return
		}
		lists++
		if _, err := r.Cookie("SID"); err != nil {
			t.Error("missing SID")
		}
		if lists == 2 {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, "[]")
	}))
	defer server.Close()
	c := client(t, server, true)
	for range 3 {
		if _, e := c.List(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if logins != 2 || lists != 4 {
		t.Fatal(logins, lists)
	}
}
func TestFailedAuthenticationBodyAndCookie(t *testing.T) {
	for _, tc := range []struct {
		body   string
		cookie bool
		status int
	}{{"Fails.", true, 200}, {"Ok.", false, 200}, {"Ok.", true, 403}, {"<html>login</html>", true, 200}} {
		t.Run(tc.body+string(rune(tc.status)), func(t *testing.T) {
			logins := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logins++
				if tc.cookie {
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			c := client(t, s, true)
			if _, e := c.List(context.Background()); e == nil {
				t.Fatal("accepted bad authentication")
			}
			if logins != 1 || c.loggedIn {
				t.Fatal("unbounded login or stale auth")
			}
		})
	}
}
func TestReauthenticationBoundedAndBypass(t *testing.T) {
	for _, auth := range []bool{false, true} {
		logins, reads := 0, 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "login") {
				logins++
				http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s", Path: "/"})
				io.WriteString(w, "Ok.")
				return
			}
			reads++
			w.WriteHeader(403)
		}))
		c := client(t, s, auth)
		_, e := c.List(context.Background())
		s.Close()
		if e == nil {
			t.Fatal("expected auth failure")
		}
		expected := 0
		if auth {
			expected = 2
		}
		if logins != expected || reads > 2 {
			t.Fatal(logins, reads)
		}
	}
}
func TestDecodeUnknownFieldsTargetAndDelete(t *testing.T) {
	deletes := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			if r.URL.RawQuery != "" && r.URL.Query().Get("hashes") != hash {
				t.Error("wrong target")
			}
			io.WriteString(w, `[{"hash":"`+hash+`","name":"test","state":"metaDL","progress":0,"downloaded":0,"dlspeed":1,"num_seeds":2,"num_leechs":3,"added_on":100,"category":"cat","tags":"keep","unknown":{"nested":true},"save_path":"SECRET_PATH"}]`)
		case "/api/v2/torrents/delete":
			deletes++
			if r.Method != "POST" {
				t.Error("delete method")
			}
			_ = r.ParseForm()
			if r.Form.Get("hashes") != hash || r.Form.Get("deleteFiles") != "false" {
				t.Error("unsafe delete form", r.Form)
			}
		default:
			t.Error("unexpected endpoint")
		}
	}))
	defer s.Close()
	c := client(t, s, false)
	ts, e := c.List(context.Background())
	if e != nil || len(ts) != 1 || ts[0].NumLeechers != 3 {
		t.Fatal(ts, e)
	}
	data, _ := json.Marshal(ts)
	if strings.Contains(string(data), "SECRET_PATH") {
		t.Fatal("retained sensitive field")
	}
	if _, e = c.Get(context.Background(), hash); e != nil {
		t.Fatal(e)
	}
	if e = c.Delete(context.Background(), hash, false); e != nil {
		t.Fatal(e)
	}
	if e = c.Delete(context.Background(), "all", false); e == nil {
		t.Fatal("accepted all")
	}
	if deletes != 1 {
		t.Fatal(deletes)
	}
}
func TestRedirectDoesNotLeakSecrets(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/SECRET_PATH", 307) }))
	defer s.Close()
	c := client(t, s, true)
	_, e := c.List(context.Background())
	if e == nil || leaked || strings.Contains(e.Error(), target.URL) || strings.Contains(e.Error(), "SECRET") {
		t.Fatal("unsafe redirect", e)
	}
}
func TestInvalidResponsesAndSanitizedErrors(t *testing.T) {
	for _, body := range []string{"null", "{}", "not json", `[{"hash":"all","state":"metaDL"}]`, `[{"hash":"` + hash + `","state":"metaDL","progress":-1}]`, `[{"hash":"` + hash + `","state":"metaDL","downloaded":-1}]`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := client(t, s, false)
		_, e := c.List(context.Background())
		s.Close()
		if e == nil || strings.Contains(e.Error(), hash) {
			t.Fatal("unsafe response", body, e)
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); io.WriteString(w, "SECRET_BODY") }))
	c := client(t, s, false)
	_, e := c.List(context.Background())
	if e == nil || strings.Contains(e.Error(), "SECRET") {
		t.Fatal(e)
	}
	s.Close()
	_, e = c.List(context.Background())
	if e == nil || strings.Contains(e.Error(), s.URL) {
		t.Fatal("URL leaked", e)
	}
}
func TestPrefixRefererAndVersions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/qbt/api/v2/") {
			t.Error(r.URL.Path)
		}
		if !strings.HasSuffix(r.Header.Get("Referer"), "/qbt/") {
			t.Error("wrong referer")
		}
		io.WriteString(w, "5.0.1")
	}))
	defer s.Close()
	c := client(t, s, false)
	c.base.Path = "/qbt/"
	a, b, e := c.Versions(context.Background())
	if e != nil || a != "5.0.1" || b != "5.0.1" {
		t.Fatal(a, b, e)
	}
}
func TestTargetMismatchAndCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"hash":"ffffffffffffffffffffffffffffffffffffffff","state":"metaDL"}]`)
	}))
	defer s.Close()
	c := client(t, s, false)
	if _, e := c.Get(context.Background(), hash); e == nil {
		t.Fatal("target mismatch accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.List(ctx); e == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestMissingSafetyFieldsFailClosed(t *testing.T) {
	for _, fields := range []string{``, `,"progress":0`, `,"downloaded":0`, `,"progress":null,"downloaded":0`, `,"progress":0,"downloaded":null`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[{"hash":"`+hash+`","state":"metaDL"`+fields+`}]`)
		}))
		c := client(t, s, false)
		_, err := c.List(context.Background())
		s.Close()
		if err == nil {
			t.Fatal("missing/null safety field treated as zero", fields)
		}
	}
}
