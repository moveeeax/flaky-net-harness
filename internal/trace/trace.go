// Package trace defines the two inputs the loss report is built from: what the
// client believed it submitted (captured by the harness proxy) and what the
// vendor's server actually holds afterwards.
//
// Keeping these two artefacts as plain JSON is deliberate. During an audit the
// server side is often collected by the vendor themselves — an export, a
// database dump, a listing of an object store — and it must be trivial for
// their engineer to produce and to check.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ObjectKind labels the unit of work a field engineer would recognise.
type ObjectKind string

// Attempt is one HTTP request as observed on the wire by the harness proxy.
type Attempt struct {
	ID       string `json:"id"`
	Workflow string `json:"workflow"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	// StartedAtMS is milliseconds since the start of the run.
	StartedAtMS int `json:"started_at_ms"`
	EndedAtMS   int `json:"ended_at_ms"`
	// DeclaredBytes is the request Content-Length the client announced.
	DeclaredBytes int64 `json:"declared_bytes"`
	// SentBytes is what actually left the client before the connection ended.
	SentBytes int64 `json:"sent_bytes"`
	// StatusCode is 0 when no response line was ever received.
	StatusCode int `json:"status_code"`
	// TransportError is the proxy-observed failure, if any.
	TransportError string `json:"transport_error,omitempty"`
	// UserVisibleError records whether the app surfaced anything to the
	// operator — read off the screen recording during the run. This is the
	// difference between a bug and a silent data-loss bug.
	UserVisibleError bool `json:"user_visible_error"`
	// Objects are the logical items the client packed into this request.
	Objects []Object `json:"objects"`
}

// Complete reports whether the whole declared body left the client.
func (a Attempt) Complete() bool {
	return a.DeclaredBytes > 0 && a.SentBytes >= a.DeclaredBytes
}

// TransferPct is the share of the declared body that was sent, 0..100.
func (a Attempt) TransferPct() float64 {
	if a.DeclaredBytes <= 0 {
		return 0
	}
	pct := float64(a.SentBytes) / float64(a.DeclaredBytes) * 100
	if pct > 100 {
		return 100
	}
	return pct
}

// Succeeded reports whether the client was told, at the HTTP level, that the
// submission worked.
func (a Attempt) Succeeded() bool {
	return a.StatusCode >= 200 && a.StatusCode < 300
}

// Object is a logical item of work: a job card, a photo, a signature.
type Object struct {
	ID     string     `json:"id"`
	Kind   ObjectKind `json:"kind"`
	Label  string     `json:"label"`
	Bytes  int64      `json:"bytes"`
	SHA256 string     `json:"sha256"`
}

// RunMeta identifies the run the trace came from.
type RunMeta struct {
	Target             string `json:"target"`
	TargetKind         string `json:"target_kind"`
	Profile            string `json:"profile"`
	ProfileVersion     int    `json:"profile_version"`
	ProfileFingerprint string `json:"profile_fingerprint"`
	StartedAt          string `json:"started_at"`
	Operator           string `json:"operator,omitempty"`
	Recording          string `json:"recording,omitempty"`
}

// ClientTrace is the proxy-side artefact.
type ClientTrace struct {
	Run      RunMeta   `json:"run"`
	Attempts []Attempt `json:"attempts"`
}

// ServerObject is one item the server actually holds after the run.
type ServerObject struct {
	ID           string     `json:"id"`
	Kind         ObjectKind `json:"kind"`
	Bytes        int64      `json:"bytes"`
	SHA256       string     `json:"sha256"`
	ReceivedAtMS int        `json:"received_at_ms"`
}

// ServerState is the vendor-side artefact: everything the backend has that
// belongs to this run.
type ServerState struct {
	Source  string         `json:"source"`
	Objects []ServerObject `json:"objects"`
}

// LoadClientTrace reads and validates a client trace from disk.
func LoadClientTrace(path string) (*ClientTrace, error) {
	var ct ClientTrace
	if err := readJSON(path, &ct); err != nil {
		return nil, err
	}
	if err := ct.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ct, nil
}

// LoadServerState reads and validates a server state export from disk.
func LoadServerState(path string) (*ServerState, error) {
	var ss ServerState
	if err := readJSON(path, &ss); err != nil {
		return nil, err
	}
	if err := ss.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ss, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("trace: reading %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("trace: parsing %s: %w", path, err)
	}
	return nil
}

// Validate rejects traces that would produce a misleading report.
func (ct *ClientTrace) Validate() error {
	if ct.Run.Profile == "" {
		return fmt.Errorf("client trace: run.profile is required")
	}
	if len(ct.Attempts) == 0 {
		return fmt.Errorf("client trace: no attempts captured")
	}
	seenAttempt := map[string]bool{}
	seenObject := map[string]string{}
	for i, a := range ct.Attempts {
		if a.ID == "" {
			return fmt.Errorf("client trace: attempt %d: id is required", i)
		}
		if seenAttempt[a.ID] {
			return fmt.Errorf("client trace: duplicate attempt id %q", a.ID)
		}
		seenAttempt[a.ID] = true
		if a.Workflow == "" {
			return fmt.Errorf("client trace: attempt %q: workflow is required", a.ID)
		}
		if a.SentBytes < 0 || a.DeclaredBytes < 0 {
			return fmt.Errorf("client trace: attempt %q: byte counts must be >= 0", a.ID)
		}
		if a.StatusCode == 0 && a.TransportError == "" {
			return fmt.Errorf("client trace: attempt %q: no status code and no transport error; the proxy recorded nothing usable", a.ID)
		}
		if len(a.Objects) == 0 {
			return fmt.Errorf("client trace: attempt %q: carries no objects, so nothing can be tracked", a.ID)
		}
		for _, o := range a.Objects {
			if o.ID == "" {
				return fmt.Errorf("client trace: attempt %q: object id is required", a.ID)
			}
			if o.SHA256 == "" {
				return fmt.Errorf("client trace: attempt %q: object %q has no sha256; without it a truncated object cannot be told from a complete one", a.ID, o.ID)
			}
			// The same object may legitimately appear in several attempts
			// (a retry), but it must be the same bytes each time.
			if prev, ok := seenObject[o.ID]; ok && prev != o.SHA256 {
				return fmt.Errorf("client trace: object %q appears with two different sha256 values", o.ID)
			}
			seenObject[o.ID] = o.SHA256
		}
	}
	return nil
}

// Validate rejects server exports that would produce a misleading report.
func (ss *ServerState) Validate() error {
	seen := map[string]bool{}
	for i, o := range ss.Objects {
		if o.ID == "" {
			return fmt.Errorf("server state: object %d: id is required", i)
		}
		if seen[o.ID] {
			return fmt.Errorf("server state: duplicate object id %q", o.ID)
		}
		seen[o.ID] = true
	}
	return nil
}

// Workflows returns the distinct workflow names in stable order.
func (ct *ClientTrace) Workflows() []string {
	set := map[string]bool{}
	for _, a := range ct.Attempts {
		set[a.Workflow] = true
	}
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// AttemptsFor returns the attempts that carried the given object, in the order
// they started.
func (ct *ClientTrace) AttemptsFor(objectID string) []Attempt {
	var out []Attempt
	for _, a := range ct.Attempts {
		for _, o := range a.Objects {
			if o.ID == objectID {
				out = append(out, a)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAtMS < out[j].StartedAtMS })
	return out
}

// Object returns the client-side record of an object.
func (ct *ClientTrace) Object(id string) (Object, bool) {
	for _, a := range ct.Attempts {
		for _, o := range a.Objects {
			if o.ID == id {
				return o, true
			}
		}
	}
	return Object{}, false
}

// ObjectIDs returns every distinct object the client tried to submit, in first
// appearance order.
func (ct *ClientTrace) ObjectIDs() []string {
	seen := map[string]bool{}
	var out []string
	attempts := append([]Attempt(nil), ct.Attempts...)
	sort.SliceStable(attempts, func(i, j int) bool { return attempts[i].StartedAtMS < attempts[j].StartedAtMS })
	for _, a := range attempts {
		for _, o := range a.Objects {
			if !seen[o.ID] {
				seen[o.ID] = true
				out = append(out, o.ID)
			}
		}
	}
	return out
}

// Get returns the server-side record of an object.
func (ss *ServerState) Get(id string) (ServerObject, bool) {
	for _, o := range ss.Objects {
		if o.ID == id {
			return o, true
		}
	}
	return ServerObject{}, false
}

// Humanise turns a workflow or kind identifier into report prose.
func Humanise(s string) string {
	if s == "" {
		return ""
	}
	r := strings.NewReplacer("_", " ", "-", " ")
	return r.Replace(s)
}
