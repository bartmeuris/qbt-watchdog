package qbt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const otherHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// validTorrentJSON builds a complete, accepted torrent entry. Callers append or
// replace fields to exercise one rejection at a time.
func validTorrentJSON(hash string) string {
	return `{"hash":"` + hash + `","name":"n","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"dlspeed":0,"num_seeds":0,"num_leechs":0,"added_on":100,"category":"","tags":""}`
}

func responseError(t *testing.T, err error) *ResponseError {
	t.Helper()
	var rejected *ResponseError
	if !errors.As(err, &rejected) {
		t.Fatalf("error is not a *ResponseError: %v", err)
	}
	return rejected
}

func TestDecodeTorrentsNamesTheRejectedField(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		code  string
		field string
		index int
	}{
		{"invalid json", "not json", CodeInvalidJSON, "", -1},
		{"null response", "null", CodeNotAList, "", -1},
		{"object response", "{}", CodeNotAList, "", -1},
		{"non-object entry", "[1]", CodeWrongFieldType, "", 0},
		{"wrong field type", `[{"hash":"` + hash + `","state":"metaDL","progress":"x","downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeWrongFieldType, "progress", 0},
		{"missing field", `[{"hash":"` + hash + `","state":"metaDL","downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeMissingField, "progress", 0},
		{"null field", `[{"hash":"` + hash + `","state":"metaDL","progress":null,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeNullField, "progress", 0},
		{"negative seeds", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":-1}]`, CodeNegativeField, "num_seeds", 0},
		{"invalid hash", `[{"hash":"all","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeInvalidHash, "", 0},
		{"empty state", `[{"hash":"` + hash + `","state":"","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeEmptyState, "state", 0},
		{"non finite progress", `[{"hash":"` + hash + `","state":"metaDL","progress":1e999,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeNonFiniteProgress, "progress", 0},
		{"progress out of range", `[{"hash":"` + hash + `","state":"metaDL","progress":1.5,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeProgressOutOfRange, "progress", 0},
		{"negative downloaded", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":-1,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeNegativeField, "downloaded", 0},
		{"negative size", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":-2,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeNegativeField, "size", 0},
		{"negative total size", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":-2,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeNegativeField, "total_size", 0},
		{"negative completed", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":-2,"amount_left":0,"num_seeds":0}]`, CodeNegativeField, "completed", 0},
		{"negative amount left", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":-2,"num_seeds":0}]`, CodeNegativeField, "amount_left", 0},
		{"size exceeds total", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":2,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`, CodeSizeExceedsTotal, "size", 0},
		{"timestamp out of range", `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0,"added_on":253402300800}]`, CodeTimestampOutOfRange, "added_on", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeTorrents([]byte(tc.body), "torrents/info")
			rejected := responseError(t, err)
			if rejected.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", rejected.Code, tc.code, err)
			}
			if rejected.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", rejected.Field, tc.field, err)
			}
			if rejected.Index != tc.index {
				t.Fatalf("index = %d, want %d (%v)", rejected.Index, tc.index, err)
			}
		})
	}
}

func TestDecodeTorrentsExactMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"size exceeds total",
			`[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":2,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`,
			`qBittorrent torrents/info rejected: entry 0, torrent 0123456789ab: size=2 exceeds total_size=1`,
		},
		{
			"missing field",
			`[{"hash":"` + hash + `","state":"metaDL","downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`,
			`qBittorrent torrents/info rejected: entry 0, torrent 0123456789ab: missing required field "progress"`,
		},
		{
			"negative seeds",
			`[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":-1}]`,
			`qBittorrent torrents/info rejected: entry 0, torrent 0123456789ab: num_seeds=-1 must be >= 0`,
		},
		{
			"invalid hash omits torrent",
			`[{"hash":"all","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`,
			`qBittorrent torrents/info rejected: entry 0: hash is not a valid info-hash`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeTorrents([]byte(tc.body), "torrents/info")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("message = %q, want %q", err, tc.want)
			}
		})
	}
}

// TestDecodeTorrentsAcceptsUnknownSizeSentinel covers qBittorrent 5.2.3, which
// reports -1 for size-related fields whose value is not yet known (a magnet
// still fetching metadata). The sentinel must be accepted and preserved, not
// clamped, so downstream policy guards keep treating it as unknown.
func TestDecodeTorrentsAcceptsUnknownSizeSentinel(t *testing.T) {
	body := `[{"hash":"` + hash + `","name":"magnet","state":"metaDL","progress":0,"downloaded":0,"size":-1,"total_size":-1,"completed":-1,"amount_left":-1,"num_seeds":0}]`
	torrents, err := decodeTorrents([]byte(body), "torrents/info")
	if err != nil {
		t.Fatalf("unknown-size sentinel rejected: %v", err)
	}
	if len(torrents) != 1 {
		t.Fatalf("want one torrent, got %d", len(torrents))
	}
	got := torrents[0]
	if got.Size != -1 || got.TotalSize != -1 || got.Completed != -1 || got.AmountLeft != -1 {
		t.Fatalf("sentinel was normalized instead of preserved: %+v", got)
	}
}

func TestDecodeTorrentsRejectsBelowUnknownSizeSentinel(t *testing.T) {
	body := `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":-2,"completed":0,"amount_left":0,"num_seeds":0}]`
	_, err := decodeTorrents([]byte(body), "torrents/info")
	rejected := responseError(t, err)
	if rejected.Code != CodeNegativeField || rejected.Field != "total_size" {
		t.Fatalf("wrong rejection: %+v", rejected)
	}
	want := `qBittorrent torrents/info rejected: entry 0, torrent 0123456789ab: total_size=-2 must be >= 0, or -1 when unknown`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err, want)
	}
}

func TestDecodeTorrentsSkipsSizeExceedsTotalWhenTotalUnknown(t *testing.T) {
	body := `[{"hash":"` + hash + `","state":"metaDL","progress":0,"downloaded":0,"size":5,"total_size":-1,"completed":0,"amount_left":0,"num_seeds":0}]`
	torrents, err := decodeTorrents([]byte(body), "torrents/info")
	if err != nil {
		t.Fatalf("size/total consistency checked against an unknown total: %v", err)
	}
	if len(torrents) != 1 || torrents[0].Size != 5 || torrents[0].TotalSize != -1 {
		t.Fatalf("unexpected decode: %+v", torrents)
	}
}

func TestDecodeTorrentsAcceptsMixedKnownAndUnknownSizes(t *testing.T) {
	body := "[" + validTorrentJSON(hash) + `,{"hash":"` + otherHash + `","name":"magnet","state":"metaDL","progress":0,"downloaded":0,"size":-1,"total_size":-1,"completed":-1,"amount_left":-1,"num_seeds":0}]`
	torrents, err := decodeTorrents([]byte(body), "torrents/info")
	if err != nil {
		t.Fatalf("mixed list rejected: %v", err)
	}
	if len(torrents) != 2 {
		t.Fatalf("want the full list, got %d", len(torrents))
	}
	if torrents[0].TotalSize != 1 || torrents[1].TotalSize != -1 {
		t.Fatalf("values not preserved: %+v", torrents)
	}
}

// qbt523TorrentsInfo is a trimmed but field-accurate qBittorrent 5.2.3
// torrents/info response: a completed torrent with known sizes followed by a
// magnet whose metadata has not arrived, so every size-related field is -1.
const qbt523TorrentsInfo = `[
  {"hash":"0123456789abcdef0123456789abcdef01234567","name":"known","state":"uploading","progress":1,"downloaded":1024,"size":1024,"total_size":1024,"completed":1024,"amount_left":0,"dlspeed":0,"num_seeds":2,"num_leechs":0,"added_on":100,"category":"tv","tags":""},
  {"hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","name":"magnet","state":"metaDL","progress":0,"downloaded":0,"size":-1,"total_size":-1,"completed":-1,"amount_left":-1,"dlspeed":0,"num_seeds":0,"num_leechs":0,"added_on":200,"category":"","tags":""}
]`

func TestDecodeTorrentsAcceptsQBittorrent523MagnetFixture(t *testing.T) {
	torrents, err := decodeTorrents([]byte(qbt523TorrentsInfo), "torrents/info")
	if err != nil {
		t.Fatalf("qBittorrent 5.2.3 fixture rejected: %v", err)
	}
	if len(torrents) != 2 {
		t.Fatalf("want both torrents, got %d", len(torrents))
	}
	if torrents[1].State != "metaDL" || torrents[1].TotalSize != -1 || torrents[1].Size != -1 {
		t.Fatalf("magnet sentinel not preserved: %+v", torrents[1])
	}
}

func TestDecodeTorrentsRejectsDuplicateHashesRegardlessOfCase(t *testing.T) {
	body := "[" + validTorrentJSON(hash) + "," + validTorrentJSON(strings.ToUpper(hash)) + "]"
	_, err := decodeTorrents([]byte(body), "torrents/info")
	rejected := responseError(t, err)
	if rejected.Code != CodeDuplicateHash || rejected.Index != 1 || rejected.ShortHash != ShortHash(hash) {
		t.Fatalf("duplicate not named: %+v", rejected)
	}
}

func TestDecodeTorrentsReturnsNoPartialList(t *testing.T) {
	body := "[" + validTorrentJSON(hash) + `,{"hash":"` + otherHash + `","state":"metaDL","progress":1.5,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`
	torrents, err := decodeTorrents([]byte(body), "torrents/info")
	if err == nil {
		t.Fatal("invalid trailing entry accepted")
	}
	if torrents != nil {
		t.Fatalf("partial list returned: %v", torrents)
	}
	rejected := responseError(t, err)
	if rejected.Index != 1 || rejected.Code != CodeProgressOutOfRange {
		t.Fatalf("wrong rejection: %+v", rejected)
	}
}

func TestDecodeTorrentsAcceptsValidEmptyList(t *testing.T) {
	torrents, err := decodeTorrents([]byte("[]"), "torrents/info")
	if err != nil {
		t.Fatal(err)
	}
	if torrents == nil || len(torrents) != 0 {
		t.Fatalf("empty list not accepted as an empty slice: %#v", torrents)
	}
}

func TestDecodeTorrentsIgnoresUnknownFields(t *testing.T) {
	body := `[{"hash":"` + hash + `","name":"n","state":"metaDL","progress":0,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"dlspeed":0,"num_seeds":0,"num_leechs":0,"added_on":100,"category":"","tags":"","unknown":{"nested":true},"save_path":"SECRET_PATH"}]`
	torrents, err := decodeTorrents([]byte(body), "torrents/info")
	if err != nil || len(torrents) != 1 {
		t.Fatalf("unknown fields broke compatibility: %v %v", torrents, err)
	}
}

func TestDecodeTorrentsNeverEchoesSensitiveStrings(t *testing.T) {
	body := `[{"hash":"` + hash + `","name":"SECRET_NAME","state":"metaDL","progress":"SECRET_PROGRESS","downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0,"save_path":"SECRET_PATH","tags":"SECRET_TAG"}]`
	_, err := decodeTorrents([]byte(body), "torrents/info")
	if err == nil {
		t.Fatal("malformed entry accepted")
	}
	for _, secret := range []string{"SECRET_NAME", "SECRET_PROGRESS", "SECRET_PATH", "SECRET_TAG", hash} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("diagnostic echoed sensitive content %q: %v", secret, err)
		}
	}
}

func TestListAndTargetedReadShareDiagnostics(t *testing.T) {
	body := `[{"hash":"` + hash + `","state":"metaDL","progress":1.5,"downloaded":0,"size":0,"total_size":1,"completed":0,"amount_left":0,"num_seeds":0}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	c := client(t, server, false)

	_, listErr := c.List(context.Background())
	_, getErr := c.Get(context.Background(), hash)
	listRejected := responseError(t, listErr)
	getRejected := responseError(t, getErr)
	if *listRejected != *getRejected {
		t.Fatalf("list and targeted read disagree: %+v vs %+v", listRejected, getRejected)
	}
	if listRejected.Code != CodeProgressOutOfRange || listRejected.Field != "progress" {
		t.Fatalf("unexpected diagnostic: %+v", listRejected)
	}
}
