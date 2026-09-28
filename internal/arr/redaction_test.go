package arr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qbt-watchdog/internal/config"
)

// Every error this package can produce is walked here, because an API key that
// reaches a log is a credential an operator must rotate. The bodies below
// deliberately echo the key back, which is exactly what a hostile or a
// misconfigured instance would do.
func TestAPIKeyNeverReachesAnErrorPath(t *testing.T) {
	failures := map[string]http.HandlerFunc{
		"unauthorised": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "invalid key "+apiKey, http.StatusUnauthorized)
		},
		"server error": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom "+apiKey, http.StatusInternalServerError)
		},
		"not found": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, apiKey, http.StatusNotFound)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://"+apiKey+".invalid/", http.StatusFound)
		},
		"garbage": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<html>"+apiKey+"</html>")
		},
		"empty": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "50")
		},
	}
	for name, handler := range failures {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, config.Sonarr, handler)
			queueErr, errs := callEverything(c)
			if queueErr == nil {
				t.Fatal("a failing instance produced a usable queue")
			}
			assertRedacted(t, errs)
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		endpoint := server.URL
		server.Close()
		c, err := New(service(t, config.Sonarr, endpoint))
		if err != nil {
			t.Fatal(err)
		}
		_, errs := callEverything(c)
		assertRedacted(t, errs)
	})
	t.Run("timed out", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
		defer server.Close()
		defer close(release)
		s := service(t, config.Sonarr, server.URL)
		s.Timeout = 30 * time.Millisecond
		c, err := New(s)
		if err != nil {
			t.Fatal(err)
		}
		_, errs := callEverything(c)
		assertRedacted(t, errs)
	})
}

// callEverything exercises every operation once and returns the queue error
// separately, because the queue is the one call that must fail whenever the
// instance is unusable.
func callEverything(c *Client) (error, []error) {
	ctx := context.Background()
	_, queueErr := c.Queue(ctx)
	_, searchErr := c.Search(ctx, []int64{1})
	_, statusErr := c.Status(ctx, 1)
	_, historyErr := c.History(ctx, hash)
	return queueErr, []error{queueErr, c.Remove(ctx, 1), searchErr, statusErr, historyErr}
}

// A removal is answered by its status line alone. An instance that returns a
// success with an unreadable body has still blocklisted the release, and
// reporting a failure here would invite a retry that blocklists the
// replacement instead.
func TestSuccessfulRemovalIgnoresAnUnreadableBody(t *testing.T) {
	c := newClient(t, config.Sonarr, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "truncated")
	})
	if err := c.Remove(context.Background(), 1); err != nil {
		t.Fatal("an applied removal was reported as failed", err)
	}
}

// assertRedacted requires every reported error to name the service and the
// operation an operator must look at, and to contain neither the credential
// nor anything the server said.
func assertRedacted(t *testing.T, errs []error) {
	t.Helper()
	reported := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		reported++
		message := err.Error()
		if strings.Contains(message, apiKey) {
			t.Fatal("error leaked the API key:", message)
		}
		if strings.Contains(strings.ToLower(message), "html") || strings.Contains(message, "boom") {
			t.Fatal("error echoed the response body:", message)
		}
		if !strings.HasPrefix(message, "sonarr ") {
			t.Fatal("error does not name the service:", message)
		}
		if OutcomeOf(err) == "" {
			t.Fatal("error carries no outcome:", message)
		}
	}
	if reported == 0 {
		t.Fatal("no error path was exercised")
	}
}

func TestOutcomeOfClassifiesForeignAndAbsentErrors(t *testing.T) {
	if OutcomeOf(nil) != Accepted {
		t.Fatal("a successful call is not accepted")
	}
	// An unrecognised failure cannot be proven not to have applied, so it
	// must never be classified as "nothing happened".
	if OutcomeOf(io.EOF) != Ambiguous {
		t.Fatal("a foreign error was optimistically classified")
	}
	for outcome, settled := range map[Outcome]bool{
		Accepted: true, NotFound: true, Rejected: true,
		Unreachable: false, Ambiguous: false,
	} {
		if outcome.Settled() != settled {
			t.Fatal("unexpected settlement for", outcome)
		}
	}
}

// Stage markers are persisted, so their spellings are part of the on-disk
// contract and may not drift.
func TestOutcomeSpellingsAreStable(t *testing.T) {
	for outcome, want := range map[Outcome]string{
		Accepted: "accepted", NotFound: "not_found", Rejected: "rejected",
		Unreachable: "transport_failure", Ambiguous: "ambiguous_timeout",
	} {
		if string(outcome) != want {
			t.Fatal("a persisted outcome name changed", outcome)
		}
	}
}
