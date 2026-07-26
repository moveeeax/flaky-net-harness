// Package analyze correlates what the client submitted with what the server
// actually holds, and classifies every difference.
//
// The classification exists to answer one question a vendor cannot argue with:
// did the app tell the operator the truth? An upload that fails and says so is
// a network problem. An upload that fails and reports success is a data-loss
// defect, and that is what this package is built to name.
package analyze

import (
	"fmt"
	"sort"
	"time"

	"github.com/moveeeax/flaky-net-harness/internal/profile"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

// Outcome is the fate of one logical object.
type Outcome string

const (
	// OutcomeDelivered: the server holds the object and the bytes match.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeSilentLoss: the object is not on the server and the operator was
	// never told. This is the finding the audit is sold on.
	OutcomeSilentLoss Outcome = "silent_loss"
	// OutcomeReportedLoss: the object is not on the server, but the app
	// surfaced an error. Correct behaviour under a broken network.
	OutcomeReportedLoss Outcome = "reported_loss"
	// OutcomeCorrupted: the server holds an object whose bytes differ from
	// what the client sent — a truncated body accepted as complete.
	OutcomeCorrupted Outcome = "corrupted"
	// OutcomeDuplicated: the same payload is stored twice under different
	// ids, i.e. a retry that is not idempotent.
	OutcomeDuplicated Outcome = "duplicated"
	// OutcomeUnattributed: the server holds an object the client trace never
	// mentions.
	OutcomeUnattributed Outcome = "unattributed"
)

// Severity ranks a finding for the report and the exit code.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityInfo     Severity = "info"
)

var severityRank = map[Severity]int{
	SeverityCritical: 0,
	SeverityHigh:     1,
	SeverityMedium:   2,
	SeverityInfo:     3,
}

// Finding is one object's story across the run.
type Finding struct {
	ObjectID string           `json:"object_id"`
	Kind     trace.ObjectKind `json:"kind"`
	Label    string           `json:"label"`
	Workflow string           `json:"workflow"`
	Bytes    int64            `json:"bytes"`
	Outcome  Outcome          `json:"outcome"`
	Severity Severity         `json:"severity"`
	// Evidence is the one-line justification, phrased so it can be quoted
	// straight into the report and into the email to the vendor.
	Evidence string `json:"evidence"`
	// Attempts is how many times the client sent this object.
	Attempts int `json:"attempts"`
	// ClaimedSuccess: at least one attempt returned HTTP 2xx.
	ClaimedSuccess bool `json:"claimed_success"`
	// OperatorWasTold: at least one attempt produced a user-visible error.
	OperatorWasTold bool `json:"operator_was_told"`
	// BestTransferPct is the furthest any attempt got through its body.
	BestTransferPct float64 `json:"best_transfer_pct"`
	// LastStatus is the HTTP status of the final attempt (0 = no response).
	LastStatus int `json:"last_status"`
}

// Silent reports whether operator work was destroyed or corrupted without the
// operator being told. A non-idempotent retry is a defect but not destruction,
// so it deliberately does not count here.
func (f Finding) Silent() bool {
	return !f.OperatorWasTold &&
		(f.Outcome == OutcomeSilentLoss || f.Outcome == OutcomeCorrupted)
}

// WorkflowSummary aggregates findings for one workflow.
type WorkflowSummary struct {
	Workflow     string  `json:"workflow"`
	Submitted    int     `json:"submitted"`
	Arrived      int     `json:"arrived"`
	SilentlyLost int     `json:"silently_lost"`
	ReportedLost int     `json:"reported_lost"`
	Corrupted    int     `json:"corrupted"`
	LostBytes    int64   `json:"lost_bytes"`
	LossPct      float64 `json:"loss_pct"`
}

// Report is the full analysis, and the input to every renderer.
type Report struct {
	GeneratedAt    time.Time         `json:"generated_at"`
	Run            trace.RunMeta     `json:"run"`
	Profile        profile.Profile   `json:"profile"`
	ProfileFprint  string            `json:"profile_fingerprint"`
	ServerSource   string            `json:"server_source"`
	Findings       []Finding         `json:"findings"`
	Workflows      []WorkflowSummary `json:"workflows"`
	Counts         map[Outcome]int   `json:"counts"`
	SeverityCounts map[Severity]int  `json:"severity_counts"`
	SubmittedBytes int64             `json:"submitted_bytes"`
	LostBytes      int64             `json:"lost_bytes"`
	SilentBytes    int64             `json:"silently_lost_bytes"`
	// Warnings flag inputs that weaken the report rather than findings in the
	// app under test.
	Warnings []string `json:"warnings,omitempty"`
	Verdict  string   `json:"verdict"`
}

// SilentLossCount is the headline number.
func (r Report) SilentLossCount() int {
	n := 0
	for _, f := range r.Findings {
		if f.Silent() {
			n++
		}
	}
	return n
}

// ExitCode is 2 when the run found silent data loss, 1 when it found loss the
// operator was told about, 0 when everything arrived. It is what makes the
// harness usable as a CI gate later without changing the analysis.
func (r Report) ExitCode() int {
	if r.SeverityCounts[SeverityCritical] > 0 {
		return 2
	}
	if r.SeverityCounts[SeverityHigh] > 0 || r.SeverityCounts[SeverityMedium] > 0 {
		return 1
	}
	return 0
}

// Analyze diffs a client trace against the server state under a known profile.
func Analyze(ct *trace.ClientTrace, ss *trace.ServerState, p profile.Profile) (*Report, error) {
	if ct == nil || ss == nil {
		return nil, fmt.Errorf("analyze: client trace and server state are both required")
	}
	r := &Report{
		GeneratedAt:    time.Now().UTC(),
		Run:            ct.Run,
		Profile:        p,
		ProfileFprint:  p.Fingerprint(),
		ServerSource:   ss.Source,
		Counts:         map[Outcome]int{},
		SeverityCounts: map[Severity]int{},
	}
	r.Warnings = consistencyWarnings(ct, p)

	byPayload := payloadIndex(ss)
	claimedIDs := map[string]bool{}

	for _, id := range ct.ObjectIDs() {
		obj, _ := ct.Object(id)
		attempts := ct.AttemptsFor(id)
		claimedIDs[id] = true

		f := Finding{
			ObjectID: id,
			Kind:     obj.Kind,
			Label:    obj.Label,
			Bytes:    obj.Bytes,
			Attempts: len(attempts),
		}
		if len(attempts) > 0 {
			f.Workflow = attempts[0].Workflow
			f.LastStatus = attempts[len(attempts)-1].StatusCode
		}
		for _, a := range attempts {
			if a.Succeeded() {
				f.ClaimedSuccess = true
			}
			if a.UserVisibleError {
				f.OperatorWasTold = true
			}
			if pct := a.TransferPct(); pct > f.BestTransferPct {
				f.BestTransferPct = pct
			}
		}

		srv, onServer := ss.Get(id)
		switch {
		case onServer && srv.SHA256 == obj.SHA256 && (srv.Bytes == 0 || srv.Bytes == obj.Bytes):
			f.Outcome = OutcomeDelivered
			f.Severity = SeverityInfo
			if f.Attempts > 1 {
				f.Evidence = fmt.Sprintf("arrived intact after %d attempts", f.Attempts)
			} else {
				f.Evidence = "arrived intact"
			}
			if dupes := byPayload[obj.SHA256]; len(dupes) > 1 {
				f.Outcome = OutcomeDuplicated
				f.Severity = SeverityMedium
				f.Evidence = fmt.Sprintf("stored %d times under ids %v: the retry path is not idempotent", len(dupes), dupes)
			}
		case onServer:
			f.Outcome = OutcomeCorrupted
			f.Severity = SeverityHigh
			f.Evidence = fmt.Sprintf("server holds %s under this id but the client sent %s: a partial body was accepted as complete",
				describeBytes(srv.Bytes, srv.SHA256), describeBytes(obj.Bytes, obj.SHA256))
			if f.ClaimedSuccess && !f.OperatorWasTold {
				f.Severity = SeverityCritical
				f.Evidence += "; the app reported success"
			}
		case f.ClaimedSuccess:
			f.Outcome = OutcomeSilentLoss
			f.Severity = SeverityCritical
			f.Evidence = fmt.Sprintf("the app received HTTP %d and showed no error, but the server has no such object", f.LastStatus)
		case !f.OperatorWasTold:
			f.Outcome = OutcomeSilentLoss
			f.Severity = SeverityCritical
			f.Evidence = fmt.Sprintf("no attempt completed (%.0f%% of the body sent, last status %s) and the app never told the operator",
				f.BestTransferPct, statusText(f.LastStatus))
		default:
			f.Outcome = OutcomeReportedLoss
			f.Severity = SeverityMedium
			f.Evidence = fmt.Sprintf("lost in transit (%.0f%% sent) and the app surfaced an error to the operator", f.BestTransferPct)
		}
		r.Findings = append(r.Findings, f)
	}

	for _, so := range ss.Objects {
		if claimedIDs[so.ID] {
			continue
		}
		r.Findings = append(r.Findings, Finding{
			ObjectID: so.ID,
			Kind:     so.Kind,
			Bytes:    so.Bytes,
			Outcome:  OutcomeUnattributed,
			Severity: SeverityInfo,
			Evidence: "present on the server but absent from the client trace; check the proxy captured the whole run",
		})
	}

	sort.SliceStable(r.Findings, func(i, j int) bool {
		if a, b := severityRank[r.Findings[i].Severity], severityRank[r.Findings[j].Severity]; a != b {
			return a < b
		}
		return r.Findings[i].ObjectID < r.Findings[j].ObjectID
	})

	summarise(r)
	return r, nil
}

func summarise(r *Report) {
	perWorkflow := map[string]*WorkflowSummary{}
	for _, f := range r.Findings {
		r.Counts[f.Outcome]++
		r.SeverityCounts[f.Severity]++
		if f.Outcome == OutcomeUnattributed {
			continue
		}
		r.SubmittedBytes += f.Bytes

		ws := perWorkflow[f.Workflow]
		if ws == nil {
			ws = &WorkflowSummary{Workflow: f.Workflow}
			perWorkflow[f.Workflow] = ws
		}
		ws.Submitted++
		switch f.Outcome {
		case OutcomeDelivered, OutcomeDuplicated:
			ws.Arrived++
		case OutcomeCorrupted:
			ws.Corrupted++
			ws.LostBytes += f.Bytes
			r.LostBytes += f.Bytes
			if f.Silent() {
				r.SilentBytes += f.Bytes
			}
		case OutcomeSilentLoss:
			ws.SilentlyLost++
			ws.LostBytes += f.Bytes
			r.LostBytes += f.Bytes
			r.SilentBytes += f.Bytes
		case OutcomeReportedLoss:
			ws.ReportedLost++
			ws.LostBytes += f.Bytes
			r.LostBytes += f.Bytes
		}
	}
	for _, ws := range perWorkflow {
		if ws.Submitted > 0 {
			lost := ws.Submitted - ws.Arrived
			ws.LossPct = float64(lost) / float64(ws.Submitted) * 100
		}
		r.Workflows = append(r.Workflows, *ws)
	}
	sort.Slice(r.Workflows, func(i, j int) bool { return r.Workflows[i].Workflow < r.Workflows[j].Workflow })

	switch {
	case r.SilentLossCount() > 0:
		r.Verdict = fmt.Sprintf("FAIL — %d of %d submitted items were destroyed or corrupted with no user-visible error",
			r.SilentLossCount(), len(r.Findings)-r.Counts[OutcomeUnattributed])
	case r.Counts[OutcomeReportedLoss] > 0 || r.Counts[OutcomeCorrupted] > 0:
		r.Verdict = "DEGRADED — items were lost, but the operator was told every time"
	default:
		r.Verdict = "PASS — everything the client submitted is on the server, intact"
	}
}

// consistencyWarnings flags inputs that make the report less defensible.
func consistencyWarnings(ct *trace.ClientTrace, p profile.Profile) []string {
	var w []string
	if ct.Run.Profile != "" && ct.Run.Profile != p.Name {
		w = append(w, fmt.Sprintf("trace was recorded under profile %q but is being analysed as %q",
			ct.Run.Profile, p.Name))
	}
	if ct.Run.ProfileVersion != 0 && ct.Run.ProfileVersion != p.Version {
		w = append(w, fmt.Sprintf("trace was recorded under %s v%d, analysed against v%d",
			ct.Run.Profile, ct.Run.ProfileVersion, p.Version))
	}
	if fp := p.Fingerprint(); ct.Run.ProfileFingerprint != "" && ct.Run.ProfileFingerprint != fp {
		w = append(w, fmt.Sprintf("profile fingerprint mismatch (trace %s, current %s): the conditions have changed since this run, so it is not comparable",
			ct.Run.ProfileFingerprint, fp))
	}
	if ct.Run.Recording == "" {
		w = append(w, "no screen recording referenced: user-visible-error claims in this report rest on the operator's notes alone")
	}
	return w
}

// payloadIndex maps sha256 to the server-side ids holding it, so a
// non-idempotent retry shows up as one payload under several ids.
func payloadIndex(ss *trace.ServerState) map[string][]string {
	idx := map[string][]string{}
	for _, o := range ss.Objects {
		if o.SHA256 == "" {
			continue
		}
		idx[o.SHA256] = append(idx[o.SHA256], o.ID)
	}
	for k := range idx {
		sort.Strings(idx[k])
	}
	return idx
}

func describeBytes(n int64, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%d bytes (%s)", n, short)
}

func statusText(code int) string {
	if code == 0 {
		return "no response"
	}
	return fmt.Sprintf("%d", code)
}
