package qbt

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// Stable diagnostic codes for a rejected qBittorrent response. They are a closed
// vocabulary: downstream code classifies by these codes through errors.As, never
// by the human-readable message.
const (
	CodeInvalidJSON         = "invalid_json"
	CodeNotAList            = "not_a_list"
	CodeWrongFieldType      = "wrong_field_type"
	CodeMissingField        = "missing_field"
	CodeNullField           = "null_field"
	CodeNegativeField       = "negative_field"
	CodeInvalidHash         = "invalid_hash"
	CodeDuplicateHash       = "duplicate_hash"
	CodeEmptyState          = "empty_state"
	CodeNonFiniteProgress   = "non_finite_progress"
	CodeProgressOutOfRange  = "progress_out_of_range"
	CodeSizeExceedsTotal    = "size_exceeds_total"
	CodeTimestampOutOfRange = "timestamp_out_of_range"
	CodeTargetMismatch      = "target_mismatch"
)

// ResponseError is a typed, sanitized description of a qBittorrent response that
// failed validation. It never carries raw response bodies, full hashes, torrent
// names/paths/tags, credentials, cookies or headers. Numeric values, static
// field names, response indices and validated short hashes are safe to include.
type ResponseError struct {
	Code       string
	Operation  string
	Index      int
	ShortHash  string
	Field      string
	Value      string
	Constraint string
	Related    string
}

// Error renders a precise, human-readable sentence. The wording is derived only
// from the sanitized fields above, so a hostile response can never inject text.
func (e *ResponseError) Error() string {
	prefix := "qBittorrent " + e.Operation + " rejected"
	location := ""
	if e.Index >= 0 {
		location = "entry " + strconv.Itoa(e.Index)
	}
	if e.ShortHash != "" {
		if location != "" {
			location += ", "
		}
		location += "torrent " + e.ShortHash
	}
	if location != "" {
		prefix += ": " + location
	}
	switch e.Code {
	case CodeInvalidJSON:
		return prefix + ": response is not valid JSON"
	case CodeNotAList:
		return prefix + ": response is not a torrent list"
	case CodeWrongFieldType:
		return prefix + ": field " + quoteField(e.Field) + " has the wrong type (" + e.Constraint + ")"
	case CodeMissingField:
		return prefix + ": missing required field " + quoteField(e.Field)
	case CodeNullField:
		return prefix + ": field " + quoteField(e.Field) + " is null"
	case CodeNegativeField:
		return prefix + ": " + e.Field + "=" + e.Value + " must be " + e.Constraint
	case CodeInvalidHash:
		return prefix + ": hash is not a valid info-hash"
	case CodeDuplicateHash:
		return prefix + ": duplicate hash"
	case CodeEmptyState:
		return prefix + ": state is empty"
	case CodeNonFiniteProgress:
		return prefix + ": progress is not finite"
	case CodeProgressOutOfRange:
		return prefix + ": progress=" + e.Value + " must be " + e.Constraint
	case CodeSizeExceedsTotal:
		return prefix + ": size=" + e.Value + " exceeds total_size=" + e.Related
	case CodeTimestampOutOfRange:
		return prefix + ": added_on=" + e.Value + " exceeds " + e.Constraint
	case CodeTargetMismatch:
		return prefix + ": targeted read returned a different torrent"
	default:
		return prefix + ": response failed validation"
	}
}

func quoteField(field string) string {
	if field == "" {
		return "unknown"
	}
	return `"` + field + `"`
}

// rawTorrent decodes the required numeric safety fields as pointers so a missing
// or null value is distinguishable from a legitimate zero. The embedded Torrent
// supplies every other field; the outer pointers shadow its value fields.
type rawTorrent struct {
	Torrent
	Progress   *float64 `json:"progress"`
	Downloaded *int64   `json:"downloaded"`
	Size       *int64   `json:"size"`
	TotalSize  *int64   `json:"total_size"`
	AmountLeft *int64   `json:"amount_left"`
	NumSeeds   *int     `json:"num_seeds"`
}

// requiredNumericFields are the safety fields that must be present and non-null.
var requiredNumericFields = []string{"progress", "downloaded", "size", "total_size", "amount_left", "num_seeds"}

// knownFields bounds the field names a diagnostic may echo. A type error on an
// unknown key is reported without the key, so an arbitrary malformed string can
// never reach a log or the UI.
var knownFields = map[string]bool{
	"hash": true, "name": true, "state": true, "progress": true,
	"downloaded": true, "size": true, "total_size": true, "completed": true,
	"amount_left": true, "dlspeed": true, "num_seeds": true, "num_leechs": true,
	"added_on": true, "category": true, "tags": true,
}

// decodeTorrents parses and validates a torrents/info response. It returns no
// partial list: the first rejected entry aborts the whole decode. Unknown fields
// stay compatible because only known fields are inspected.
func decodeTorrents(data []byte, operation string) ([]Torrent, error) {
	if !json.Valid(data) {
		return nil, &ResponseError{Code: CodeInvalidJSON, Operation: operation, Index: -1}
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil || entries == nil {
		return nil, &ResponseError{Code: CodeNotAList, Operation: operation, Index: -1}
	}
	torrents := make([]Torrent, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for i, raw := range entries {
		t, rerr := validateTorrent(i, raw, operation)
		if rerr != nil {
			return nil, rerr
		}
		if seen[t.Hash] {
			return nil, &ResponseError{Code: CodeDuplicateHash, Operation: operation, Index: i, ShortHash: ShortHash(t.Hash)}
		}
		seen[t.Hash] = true
		torrents = append(torrents, t)
	}
	return torrents, nil
}

// validateTorrent walks every acceptance rule with an explicit early return, so
// the first failure names exactly which field or relationship was rejected.
func validateTorrent(index int, raw json.RawMessage, operation string) (Torrent, *ResponseError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Torrent{}, &ResponseError{Code: CodeWrongFieldType, Operation: operation, Index: index, Constraint: "expected object"}
	}

	// The hash is read from the raw field map so it is available even when a
	// later field fails to decode. It is only ever echoed when it is valid.
	var hash string
	if rawHash, ok := fields["hash"]; ok {
		_ = json.Unmarshal(rawHash, &hash)
	}
	hash = strings.ToLower(hash)
	if !ValidHash(hash) {
		return Torrent{}, &ResponseError{Code: CodeInvalidHash, Operation: operation, Index: index}
	}
	short := ShortHash(hash)

	var entry rawTorrent
	if err := json.Unmarshal(raw, &entry); err != nil {
		// An out-of-range JSON number decodes to a non-finite float and also
		// reports an error; name it precisely rather than as a generic type
		// error. The pointer is populated even when the decode errors.
		if entry.Progress != nil && (math.IsNaN(*entry.Progress) || math.IsInf(*entry.Progress, 0)) {
			return Torrent{}, &ResponseError{Code: CodeNonFiniteProgress, Operation: operation, Index: index, ShortHash: short, Field: "progress"}
		}
		field := ""
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && knownFields[typeErr.Field] {
			field = typeErr.Field
		}
		return Torrent{}, &ResponseError{Code: CodeWrongFieldType, Operation: operation, Index: index, ShortHash: short, Field: field, Constraint: "expected numeric"}
	}

	for _, name := range requiredNumericFields {
		value, present := fields[name]
		if !present {
			return Torrent{}, &ResponseError{Code: CodeMissingField, Operation: operation, Index: index, ShortHash: short, Field: name}
		}
		if isJSONNull(value) {
			return Torrent{}, &ResponseError{Code: CodeNullField, Operation: operation, Index: index, ShortHash: short, Field: name}
		}
	}
	if *entry.NumSeeds < 0 {
		return Torrent{}, negativeField(index, short, operation, "num_seeds", int64(*entry.NumSeeds))
	}

	entry.Torrent.Hash = hash
	entry.Torrent.Progress = *entry.Progress
	entry.Torrent.Downloaded = *entry.Downloaded
	entry.Torrent.Size = *entry.Size
	entry.Torrent.TotalSize = *entry.TotalSize
	entry.Torrent.AmountLeft = *entry.AmountLeft
	entry.Torrent.NumSeeds = *entry.NumSeeds
	t := entry.Torrent

	if t.State == "" {
		return Torrent{}, &ResponseError{Code: CodeEmptyState, Operation: operation, Index: index, ShortHash: short, Field: "state"}
	}
	if math.IsNaN(t.Progress) || math.IsInf(t.Progress, 0) {
		return Torrent{}, &ResponseError{Code: CodeNonFiniteProgress, Operation: operation, Index: index, ShortHash: short, Field: "progress"}
	}
	if t.Progress < 0 || t.Progress > 1 {
		return Torrent{}, &ResponseError{Code: CodeProgressOutOfRange, Operation: operation, Index: index, ShortHash: short, Field: "progress", Value: formatFloat(t.Progress), Constraint: "between 0 and 1"}
	}
	if t.Downloaded < 0 {
		return Torrent{}, negativeField(index, short, operation, "downloaded", t.Downloaded)
	}
	// qBittorrent reports -1 for a size-related field whose value is not yet
	// known (for example a magnet still fetching metadata). That sentinel is
	// accepted and preserved as "unknown"; only a value below it is invalid.
	if t.Size < unknownSizeSentinel {
		return Torrent{}, unknownSizeField(index, short, operation, "size", t.Size)
	}
	if t.TotalSize < unknownSizeSentinel {
		return Torrent{}, unknownSizeField(index, short, operation, "total_size", t.TotalSize)
	}
	if t.Completed < unknownSizeSentinel {
		return Torrent{}, unknownSizeField(index, short, operation, "completed", t.Completed)
	}
	if t.AmountLeft < unknownSizeSentinel {
		return Torrent{}, unknownSizeField(index, short, operation, "amount_left", t.AmountLeft)
	}
	// A size can only be compared against a known total; when the total is
	// unknown (-1) there is nothing to compare against.
	if t.TotalSize >= 0 && t.Size > t.TotalSize {
		return Torrent{}, &ResponseError{Code: CodeSizeExceedsTotal, Operation: operation, Index: index, ShortHash: short, Field: "size", Value: strconv.FormatInt(t.Size, 10), Related: strconv.FormatInt(t.TotalSize, 10)}
	}
	if t.AddedOn > 253402300799 {
		return Torrent{}, &ResponseError{Code: CodeTimestampOutOfRange, Operation: operation, Index: index, ShortHash: short, Field: "added_on", Value: strconv.FormatInt(t.AddedOn, 10), Constraint: "253402300799"}
	}
	return t, nil
}

func negativeField(index int, short, operation, field string, value int64) *ResponseError {
	return &ResponseError{Code: CodeNegativeField, Operation: operation, Index: index, ShortHash: short, Field: field, Value: strconv.FormatInt(value, 10), Constraint: ">= 0"}
}

// unknownSizeSentinel is qBittorrent's sentinel for a size-related field whose
// value is not yet known. It is preserved, never clamped to zero, so downstream
// policy guards that test for a positive total keep treating it as unknown.
const unknownSizeSentinel = -1

// unknownSizeConstraint is the acceptance rule for a size-related field: any
// non-negative value, or the -1 unknown sentinel.
const unknownSizeConstraint = ">= 0, or -1 when unknown"

// unknownSizeField reports a size-related value below the unknown sentinel. It
// keeps the negative_field code so downstream classification is unchanged.
func unknownSizeField(index int, short, operation, field string, value int64) *ResponseError {
	return &ResponseError{Code: CodeNegativeField, Operation: operation, Index: index, ShortHash: short, Field: field, Value: strconv.FormatInt(value, 10), Constraint: unknownSizeConstraint}
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
