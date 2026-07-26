// Package profile defines the named, versioned network profiles the harness
// replays. A profile is a pinned set of tc/netem parameters plus a timeline of
// adversarial events, so that two runs of the same profile version are
// comparable and a finding can be reproduced by a third party.
package profile

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

//go:embed profiles.json
var builtinFS embed.FS

// EventKind is an adversarial event applied part-way through a run.
type EventKind string

const (
	// EventStall blackholes the link (100% loss) without tearing the
	// interface down: sockets stay open and time out slowly.
	EventStall EventKind = "stall"
	// EventDisconnect drops the link hard, as a radio handover does.
	EventDisconnect EventKind = "disconnect"
	// EventRestore returns the link to the profile's steady-state shape.
	EventRestore EventKind = "restore"
	// EventKillApp SIGKILLs the target application process.
	EventKillApp EventKind = "kill_app"
)

// Valid reports whether k is a known event kind.
func (k EventKind) Valid() bool {
	switch k {
	case EventStall, EventDisconnect, EventRestore, EventKillApp:
		return true
	}
	return false
}

// Event is a scheduled disruption. Exactly one of AtSeconds or AtTransferPct
// is used: AtTransferPct fires the event once the observed upload has moved
// that share of its declared Content-Length, which is how "drop the network at
// 60% of the transfer" is expressed.
type Event struct {
	Kind          EventKind `json:"kind"`
	AtSeconds     int       `json:"at_seconds,omitempty"`
	AtTransferPct int       `json:"at_transfer_pct,omitempty"`
	ForSeconds    int       `json:"for_seconds,omitempty"`
	Note          string    `json:"note"`
}

// Shape is the steady-state link shape applied with tc/netem.
type Shape struct {
	DownlinkKbit int     `json:"downlink_kbit"`
	UplinkKbit   int     `json:"uplink_kbit"`
	DelayMS      int     `json:"delay_ms"`
	JitterMS     int     `json:"jitter_ms"`
	LossPct      float64 `json:"loss_pct"`
	// LossCorrelationPct models bursty loss: netem's correlation term.
	LossCorrelationPct float64 `json:"loss_correlation_pct"`
	ReorderPct         float64 `json:"reorder_pct"`
	DuplicatePct       float64 `json:"duplicate_pct"`
	// QueueLimitPackets is netem's backlog; a shallow queue on a slow uplink
	// is what turns congestion into tail drops rather than growing latency.
	QueueLimitPackets int `json:"queue_limit_packets"`
}

// Profile is a named, versioned network condition.
type Profile struct {
	Name          string  `json:"name"`
	Version       int     `json:"version"`
	Summary       string  `json:"summary"`
	Justification string  `json:"justification"`
	Shape         Shape   `json:"shape"`
	Events        []Event `json:"events"`
	// RunSeconds is the wall-clock budget for one workflow under this profile.
	RunSeconds int `json:"run_seconds"`
}

// Duration returns the run budget as a time.Duration.
func (p Profile) Duration() time.Duration {
	return time.Duration(p.RunSeconds) * time.Second
}

// Fingerprint is a stable SHA-256 over the profile's observable parameters.
// It is printed in every loss report: if the fingerprint differs, the two runs
// are not comparable, whatever the profile name says.
func (p Profile) Fingerprint() string {
	canon := struct {
		Name       string  `json:"name"`
		Version    int     `json:"version"`
		Shape      Shape   `json:"shape"`
		Events     []Event `json:"events"`
		RunSeconds int     `json:"run_seconds"`
	}{p.Name, p.Version, p.Shape, p.Events, p.RunSeconds}
	// Notes are documentation, not parameters; strip them so a typo fix does
	// not invalidate historical comparisons.
	canon.Events = append([]Event(nil), canon.Events...)
	for i := range canon.Events {
		canon.Events[i].Note = ""
	}
	b, err := json.Marshal(canon)
	if err != nil { // unreachable: the struct is plain data
		panic(fmt.Sprintf("profile: marshalling %q: %v", p.Name, err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Validate checks a profile is internally consistent and physically sane.
func (p Profile) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("profile: name is empty")
	}
	if p.Version < 1 {
		return fmt.Errorf("profile %q: version must be >= 1, got %d", p.Name, p.Version)
	}
	if p.Shape.DownlinkKbit <= 0 || p.Shape.UplinkKbit <= 0 {
		return fmt.Errorf("profile %q: downlink and uplink must be > 0 kbit", p.Name)
	}
	if p.Shape.DelayMS < 0 || p.Shape.JitterMS < 0 {
		return fmt.Errorf("profile %q: delay and jitter must be >= 0", p.Name)
	}
	if p.Shape.JitterMS > p.Shape.DelayMS {
		return fmt.Errorf("profile %q: jitter %dms exceeds delay %dms, which makes netem reorder packets by accident",
			p.Name, p.Shape.JitterMS, p.Shape.DelayMS)
	}
	if err := pct("loss_pct", p.Name, p.Shape.LossPct); err != nil {
		return err
	}
	if err := pct("loss_correlation_pct", p.Name, p.Shape.LossCorrelationPct); err != nil {
		return err
	}
	if err := pct("reorder_pct", p.Name, p.Shape.ReorderPct); err != nil {
		return err
	}
	if err := pct("duplicate_pct", p.Name, p.Shape.DuplicatePct); err != nil {
		return err
	}
	if p.Shape.QueueLimitPackets <= 0 {
		return fmt.Errorf("profile %q: queue_limit_packets must be > 0", p.Name)
	}
	if p.RunSeconds <= 0 {
		return fmt.Errorf("profile %q: run_seconds must be > 0", p.Name)
	}
	outageOpen := false
	for i, e := range p.Events {
		if !e.Kind.Valid() {
			return fmt.Errorf("profile %q: event %d: unknown kind %q", p.Name, i, e.Kind)
		}
		if e.AtSeconds == 0 && e.AtTransferPct == 0 {
			return fmt.Errorf("profile %q: event %d (%s): needs at_seconds or at_transfer_pct", p.Name, i, e.Kind)
		}
		if e.AtSeconds != 0 && e.AtTransferPct != 0 {
			return fmt.Errorf("profile %q: event %d (%s): at_seconds and at_transfer_pct are mutually exclusive", p.Name, i, e.Kind)
		}
		if e.AtSeconds > p.RunSeconds {
			return fmt.Errorf("profile %q: event %d (%s) fires at %ds, after the %ds run budget", p.Name, i, e.Kind, e.AtSeconds, p.RunSeconds)
		}
		if e.AtTransferPct < 0 || e.AtTransferPct > 100 {
			return fmt.Errorf("profile %q: event %d (%s): at_transfer_pct must be 0..100", p.Name, i, e.Kind)
		}
		switch e.Kind {
		case EventStall, EventDisconnect:
			if outageOpen {
				return fmt.Errorf("profile %q: event %d (%s): link is already down; restore it first", p.Name, i, e.Kind)
			}
			if e.ForSeconds < 0 {
				return fmt.Errorf("profile %q: event %d (%s): for_seconds must be >= 0", p.Name, i, e.Kind)
			}
			// A zero duration means "open ended": the outage stays until an
			// explicit restore event closes it.
			outageOpen = e.ForSeconds == 0
		case EventRestore:
			if !outageOpen {
				return fmt.Errorf("profile %q: event %d (restore): link is not down", p.Name, i)
			}
			outageOpen = false
		}
	}
	if outageOpen {
		return fmt.Errorf("profile %q: link is left down at the end of the run; add a restore event", p.Name)
	}
	return nil
}

func pct(field, name string, v float64) error {
	if v < 0 || v > 100 {
		return fmt.Errorf("profile %q: %s must be 0..100, got %g", name, field, v)
	}
	return nil
}

// Set is a collection of profiles keyed by name.
type Set struct {
	byName map[string]Profile
}

// Load parses a profile set from JSON and validates every profile in it.
func Load(data []byte) (*Set, error) {
	var list []Profile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("profile: decoding set: %w", err)
	}
	s := &Set{byName: make(map[string]Profile, len(list))}
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := s.byName[p.Name]; dup {
			return nil, fmt.Errorf("profile: duplicate profile name %q", p.Name)
		}
		s.byName[p.Name] = p
	}
	if len(s.byName) == 0 {
		return nil, fmt.Errorf("profile: set is empty")
	}
	return s, nil
}

// Builtin returns the three profiles shipped with the harness.
func Builtin() *Set {
	data, err := builtinFS.ReadFile("profiles.json")
	if err != nil { // unreachable: the file is embedded at build time
		panic("profile: embedded profiles.json missing: " + err.Error())
	}
	s, err := Load(data)
	if err != nil { // unreachable in a build whose tests pass
		panic("profile: embedded profiles.json invalid: " + err.Error())
	}
	return s
}

// Get returns the named profile.
func (s *Set) Get(name string) (Profile, error) {
	p, ok := s.byName[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q (known: %v)", name, s.Names())
	}
	return p, nil
}

// Names returns the profile names in stable order.
func (s *Set) Names() []string {
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// All returns every profile in stable name order.
func (s *Set) All() []Profile {
	out := make([]Profile, 0, len(s.byName))
	for _, n := range s.Names() {
		out = append(out, s.byName[n])
	}
	return out
}
