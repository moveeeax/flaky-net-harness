// Package compare puts two loss reports side by side: the run that found the
// problem and the run against the vendor's fix. It answers the follow-up
// question after a FAIL: which silent losses are gone, which remain, and did
// the fix destroy anything that used to arrive?
//
// Two runs are only comparable when they were recorded under the same profile
// fingerprint. Compare refuses otherwise unless the caller explicitly accepts
// the mismatch, in which case the comparison carries the warning.
package compare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/moveeeax/flaky-net-harness/internal/analyze"
)

// Change is what happened to one object between the two runs.
type Change string

const (
	// ChangeRegressed: not silently lost before (or not in the before run at
	// all), silently lost after. This is what makes a fix unacceptable.
	ChangeRegressed Change = "regressed"
	// ChangeStillSilent: silently lost in both runs. The fix did not reach it.
	ChangeStillSilent Change = "still_silent"
	// ChangeFixed: silently lost before, not silently lost after.
	ChangeFixed Change = "fixed"
	// ChangeWorsened: severity went up, without becoming silent loss.
	ChangeWorsened Change = "worsened"
	// ChangeImproved: severity went down, without silent loss on either side.
	ChangeImproved Change = "improved"
	// ChangeUnchanged: same outcome class in both runs, not silent.
	ChangeUnchanged Change = "unchanged"
	// ChangeOnlyAfter: the object appears only in the after run, not silent.
	ChangeOnlyAfter Change = "only_after"
	// ChangeOnlyBefore: the object appears only in the before run.
	ChangeOnlyBefore Change = "only_before"
)

var changeRank = map[Change]int{
	ChangeRegressed:   0,
	ChangeStillSilent: 1,
	ChangeFixed:       2,
	ChangeWorsened:    3,
	ChangeImproved:    4,
	ChangeOnlyAfter:   5,
	ChangeOnlyBefore:  6,
	ChangeUnchanged:   7,
}

var severityRank = map[analyze.Severity]int{
	analyze.SeverityCritical: 0,
	analyze.SeverityHigh:     1,
	analyze.SeverityMedium:   2,
	analyze.SeverityInfo:     3,
}

// Verdict is the one-word answer to "did the fix work?".
type Verdict string

const (
	// VerdictFixed: the before run had silent loss, the after run has none.
	VerdictFixed Verdict = "FIXED"
	// VerdictPartial: some silent losses are gone, some remain, none are new.
	VerdictPartial Verdict = "PARTIAL"
	// VerdictRegressed: at least one object is silently lost after the fix
	// that was not silently lost before. Takes precedence over every fix.
	VerdictRegressed Verdict = "REGRESSED"
	// VerdictUnchanged: no silent loss was fixed and none is new.
	VerdictUnchanged Verdict = "UNCHANGED"
)

// Side is the headline of one of the two runs.
type Side struct {
	Target       string `json:"target"`
	StartedAt    string `json:"started_at"`
	ServerSource string `json:"server_source"`
	Fingerprint  string `json:"profile_fingerprint"`
	Verdict      string `json:"verdict"`
	Findings     int    `json:"findings"`
	SilentLosses int    `json:"silent_losses"`
	SilentBytes  int64  `json:"silently_lost_bytes"`
	LostBytes    int64  `json:"lost_bytes"`
	ReportedLoss int    `json:"reported_losses"`
	Duplicated   int    `json:"duplicated"`
	Delivered    int    `json:"delivered"`
}

// Transition is one object's outcome in the before run and in the after run.
// An empty outcome means the object does not appear in that run.
type Transition struct {
	ObjectID       string           `json:"object_id"`
	Label          string           `json:"label,omitempty"`
	Workflow       string           `json:"workflow,omitempty"`
	Before         analyze.Outcome  `json:"before,omitempty"`
	After          analyze.Outcome  `json:"after,omitempty"`
	BeforeSeverity analyze.Severity `json:"before_severity,omitempty"`
	AfterSeverity  analyze.Severity `json:"after_severity,omitempty"`
	BeforeSilent   bool             `json:"before_silent"`
	AfterSilent    bool             `json:"after_silent"`
	Change         Change           `json:"change"`
	// Evidence is the after run's evidence, or the before run's when the
	// object is absent from the after run.
	Evidence string `json:"evidence"`
}

// Comparison is the full before/after view, and the input to every renderer.
type Comparison struct {
	GeneratedAt time.Time `json:"generated_at"`
	Profile     string    `json:"profile"`
	// ProfileVersion is the before run's profile version.
	ProfileVersion    int            `json:"profile_version"`
	FingerprintsMatch bool           `json:"fingerprints_match"`
	Before            Side           `json:"before"`
	After             Side           `json:"after"`
	Transitions       []Transition   `json:"transitions"`
	ChangeCounts      map[Change]int `json:"change_counts"`
	Warnings          []string       `json:"warnings,omitempty"`
	Verdict           Verdict        `json:"verdict"`
	Summary           string         `json:"summary"`
}

// Options control how strict Compare is.
type Options struct {
	// AllowMismatch compares runs recorded under different profile
	// fingerprints instead of refusing. The comparison carries a warning.
	AllowMismatch bool
}

// ErrFingerprintMismatch is returned when the two reports were produced under
// different network conditions and Options.AllowMismatch is not set.
var ErrFingerprintMismatch = errors.New("compare: profile fingerprints differ")

// LoadReport reads a report written by `analyze --format json`. Unknown
// fields are rejected, and the stored fingerprint must match the profile the
// report embeds, so a hand-edited report cannot pass for a comparable one.
func LoadReport(path string) (*analyze.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compare: reading %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r analyze.Report
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("compare: parsing %s (want the output of analyze --format json): %w", path, err)
	}
	if r.Verdict == "" || r.Profile.Name == "" {
		return nil, fmt.Errorf("compare: %s is not an analyze report (no verdict or profile); write one with analyze --format json", path)
	}
	if fp := r.Profile.Fingerprint(); r.ProfileFprint != fp {
		return nil, fmt.Errorf("compare: %s claims profile fingerprint %q but its embedded profile hashes to %q; the report was edited or is corrupt",
			path, r.ProfileFprint, fp)
	}
	return &r, nil
}

// Compare diffs two reports object by object.
func Compare(before, after *analyze.Report, opts Options) (*Comparison, error) {
	if before == nil || after == nil {
		return nil, fmt.Errorf("compare: both reports are required")
	}
	c := &Comparison{
		GeneratedAt:       time.Now().UTC(),
		Profile:           before.Profile.Name,
		ProfileVersion:    before.Profile.Version,
		FingerprintsMatch: before.ProfileFprint == after.ProfileFprint,
		Before:            side(before),
		After:             side(after),
		ChangeCounts:      map[Change]int{},
	}
	if !c.FingerprintsMatch {
		msg := fmt.Sprintf("before was recorded under %s v%d (fingerprint %s), after under %s v%d (fingerprint %s): the network conditions differ, so a change in outcome may be the network and not the fix",
			before.Profile.Name, before.Profile.Version, before.ProfileFprint,
			after.Profile.Name, after.Profile.Version, after.ProfileFprint)
		if !opts.AllowMismatch {
			return nil, fmt.Errorf("%w: %s", ErrFingerprintMismatch, msg)
		}
		c.Warnings = append(c.Warnings, "NOT COMPARABLE: "+msg)
	}
	for _, w := range before.Warnings {
		c.Warnings = append(c.Warnings, "before run: "+w)
	}
	for _, w := range after.Warnings {
		c.Warnings = append(c.Warnings, "after run: "+w)
	}

	beforeByID := index(before)
	afterByID := index(after)
	ids := make([]string, 0, len(beforeByID)+len(afterByID))
	for id := range beforeByID {
		ids = append(ids, id)
	}
	for id := range afterByID {
		if _, ok := beforeByID[id]; !ok {
			ids = append(ids, id)
		}
	}

	for _, id := range ids {
		b, inBefore := beforeByID[id]
		a, inAfter := afterByID[id]
		t := Transition{ObjectID: id}
		if inBefore {
			t.Label, t.Workflow = b.Label, b.Workflow
			t.Before, t.BeforeSeverity, t.BeforeSilent = b.Outcome, b.Severity, b.Silent()
			t.Evidence = b.Evidence
		}
		if inAfter {
			if a.Label != "" {
				t.Label = a.Label
			}
			if a.Workflow != "" {
				t.Workflow = a.Workflow
			}
			t.After, t.AfterSeverity, t.AfterSilent = a.Outcome, a.Severity, a.Silent()
			t.Evidence = a.Evidence
		}
		t.Change = classify(t, inBefore, inAfter)
		c.ChangeCounts[t.Change]++
		c.Transitions = append(c.Transitions, t)
	}

	sort.SliceStable(c.Transitions, func(i, j int) bool {
		if a, b := changeRank[c.Transitions[i].Change], changeRank[c.Transitions[j].Change]; a != b {
			return a < b
		}
		return c.Transitions[i].ObjectID < c.Transitions[j].ObjectID
	})

	c.Verdict, c.Summary = verdict(c)
	return c, nil
}

func classify(t Transition, inBefore, inAfter bool) Change {
	switch {
	case !inAfter:
		return ChangeOnlyBefore
	case t.AfterSilent && t.BeforeSilent:
		return ChangeStillSilent
	case t.AfterSilent:
		return ChangeRegressed
	case !inBefore:
		return ChangeOnlyAfter
	case t.BeforeSilent:
		return ChangeFixed
	}
	switch b, a := severityRank[t.BeforeSeverity], severityRank[t.AfterSeverity]; {
	case a < b:
		return ChangeWorsened
	case a > b:
		return ChangeImproved
	}
	return ChangeUnchanged
}

func verdict(c *Comparison) (Verdict, string) {
	regressed := c.ChangeCounts[ChangeRegressed]
	remaining := c.ChangeCounts[ChangeStillSilent]
	fixed := c.ChangeCounts[ChangeFixed]
	// Objects that were silently lost before and are missing from the after
	// run were not shown to be fixed: the after run did not exercise them.
	untested := c.untested()
	silentBefore := c.Before.SilentLosses

	var v Verdict
	var s string
	switch {
	case regressed > 0:
		v = VerdictRegressed
		s = fmt.Sprintf("%d item(s) newly destroyed or corrupted with no user-visible error", regressed)
		if fixed > 0 {
			s += fmt.Sprintf(", despite %d of %d earlier silent losses fixed", fixed, silentBefore)
		}
	case fixed > 0 && remaining == 0 && untested == 0:
		v = VerdictFixed
		s = fmt.Sprintf("all %d silent losses from the before run are gone and none are new", fixed)
	case fixed > 0:
		v = VerdictPartial
		s = fmt.Sprintf("%d of %d silent losses fixed, %d still lost silently, none new", fixed, silentBefore, remaining)
	case remaining > 0:
		v = VerdictUnchanged
		s = fmt.Sprintf("the same %d item(s) are still destroyed or corrupted with no user-visible error", remaining)
	case untested > 0:
		v = VerdictUnchanged
		s = "no silent loss was shown to be fixed"
	default:
		v = VerdictUnchanged
		s = "no silent loss in either run"
	}
	if untested > 0 {
		s += fmt.Sprintf("; %d earlier silent loss(es) were not exercised by the after run", untested)
	}
	return v, fmt.Sprintf("%s — %s", v, s)
}

// ExitCode is 2 when the fix introduced new silent loss, 1 when silent loss
// remains (or was not re-tested), 0 when the after run has none.
func (c *Comparison) ExitCode() int {
	switch c.Verdict {
	case VerdictRegressed:
		return 2
	case VerdictFixed:
		return 0
	}
	if c.After.SilentLosses > 0 || c.untested() > 0 {
		return 1
	}
	return 0
}

func (c *Comparison) untested() int {
	n := 0
	for _, t := range c.Transitions {
		if t.Change == ChangeOnlyBefore && t.BeforeSilent {
			n++
		}
	}
	return n
}

func side(r *analyze.Report) Side {
	return Side{
		Target:       r.Run.Target,
		StartedAt:    r.Run.StartedAt,
		ServerSource: r.ServerSource,
		Fingerprint:  r.ProfileFprint,
		Verdict:      r.Verdict,
		Findings:     len(r.Findings),
		SilentLosses: r.SilentLossCount(),
		SilentBytes:  r.SilentBytes,
		LostBytes:    r.LostBytes,
		ReportedLoss: r.Counts[analyze.OutcomeReportedLoss],
		Duplicated:   r.Counts[analyze.OutcomeDuplicated],
		Delivered:    r.Counts[analyze.OutcomeDelivered],
	}
}

func index(r *analyze.Report) map[string]analyze.Finding {
	m := make(map[string]analyze.Finding, len(r.Findings))
	for _, f := range r.Findings {
		m[f.ObjectID] = f
	}
	return m
}
