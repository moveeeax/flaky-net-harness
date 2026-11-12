package compare

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/flaky-net-harness/internal/analyze"
	"github.com/moveeeax/flaky-net-harness/internal/profile"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

const fixtureDir = "../../testdata/runs"

// analyzeRun analyses the fixture in dir under the named profile.
func analyzeRun(t *testing.T, dir, profileName string) *analyze.Report {
	t.Helper()
	ct, err := trace.LoadClientTrace(filepath.Join(fixtureDir, dir, "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := trace.LoadServerState(filepath.Join(fixtureDir, dir, "server-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Builtin().Get(profileName)
	if err != nil {
		t.Fatal(err)
	}
	r, err := analyze.Analyze(ct, ss, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func before(t *testing.T) *analyze.Report { return analyzeRun(t, "nairobi-1700", "nairobi-1700") }
func after(t *testing.T) *analyze.Report  { return analyzeRun(t, "nairobi-1700-fix", "nairobi-1700") }

func mustCompare(t *testing.T, b, a *analyze.Report) *Comparison {
	t.Helper()
	c, err := Compare(b, a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func transitionFor(t *testing.T, c *Comparison, id string) Transition {
	t.Helper()
	for _, tr := range c.Transitions {
		if tr.ObjectID == id {
			return tr
		}
	}
	t.Fatalf("no transition for %q", id)
	return Transition{}
}

// setOutcome rewrites one finding, as a different run would have classified it.
func setOutcome(t *testing.T, r *analyze.Report, id string, o analyze.Outcome, sev analyze.Severity, told bool) {
	t.Helper()
	for i := range r.Findings {
		if r.Findings[i].ObjectID == id {
			r.Findings[i].Outcome = o
			r.Findings[i].Severity = sev
			r.Findings[i].OperatorWasTold = told
			return
		}
	}
	t.Fatalf("no finding for %q", id)
}

func TestVendorFixIsClassifiedPerObject(t *testing.T) {
	c := mustCompare(t, before(t), after(t))

	want := map[string]Change{
		"sig-0091":     ChangeStillSilent, // HTTP 200 and nothing on the server, both runs
		"ph-002":       ChangeFixed,       // corrupted before, retried to completion after
		"ph-003":       ChangeFixed,
		"ph-004":       ChangeFixed,
		"ph-006":       ChangeFixed,
		"ph-001":       ChangeImproved,   // stored twice before, once after
		"srv-att-77c1": ChangeOnlyBefore, // the duplicate's server-side id
		"jc-4471":      ChangeUnchanged,
		"ph-005":       ChangeUnchanged, // lost with an error shown, both runs
	}
	if len(c.Transitions) != len(want) {
		t.Fatalf("transitions = %d, want %d", len(c.Transitions), len(want))
	}
	for id, change := range want {
		if got := transitionFor(t, c, id).Change; got != change {
			t.Errorf("%s change = %s, want %s", id, got, change)
		}
	}
	ph3 := transitionFor(t, c, "ph-003")
	if ph3.Before != analyze.OutcomeSilentLoss || ph3.After != analyze.OutcomeDelivered {
		t.Errorf("ph-003 transition = %s -> %s, want silent_loss -> delivered", ph3.Before, ph3.After)
	}
}

func TestPartialFixVerdictAndExitCode(t *testing.T) {
	c := mustCompare(t, before(t), after(t))
	if c.Verdict != VerdictPartial {
		t.Fatalf("verdict = %s, want PARTIAL (%s)", c.Verdict, c.Summary)
	}
	if !strings.Contains(c.Summary, "4 of 5 silent losses fixed, 1 still lost silently") {
		t.Errorf("summary = %q", c.Summary)
	}
	if code := c.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1 while silent loss remains", code)
	}
	if !c.FingerprintsMatch || len(c.Warnings) != 0 {
		t.Errorf("same profile should compare cleanly: match=%v warnings=%v", c.FingerprintsMatch, c.Warnings)
	}
}

func TestTransitionsSortWorstFirst(t *testing.T) {
	c := mustCompare(t, before(t), after(t))
	if first := c.Transitions[0]; first.Change != ChangeStillSilent || first.ObjectID != "sig-0091" {
		t.Errorf("first transition = %s %s, want the remaining silent loss", first.Change, first.ObjectID)
	}
	if last := c.Transitions[len(c.Transitions)-1]; last.Change != ChangeUnchanged {
		t.Errorf("last transition = %s, want unchanged", last.Change)
	}
}

func TestEverySilentLossGoneIsFixed(t *testing.T) {
	a := after(t)
	setOutcome(t, a, "sig-0091", analyze.OutcomeDelivered, analyze.SeverityInfo, false)
	c := mustCompare(t, before(t), a)
	if c.Verdict != VerdictFixed {
		t.Fatalf("verdict = %s, want FIXED (%s)", c.Verdict, c.Summary)
	}
	if c.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", c.ExitCode())
	}
}

func TestNewSilentLossIsARegressionEvenWithFixes(t *testing.T) {
	a := after(t)
	// The job card used to arrive; after the fix it vanishes without a word.
	setOutcome(t, a, "jc-4471", analyze.OutcomeSilentLoss, analyze.SeverityCritical, false)
	c := mustCompare(t, before(t), a)
	if c.Verdict != VerdictRegressed {
		t.Fatalf("verdict = %s, want REGRESSED (%s)", c.Verdict, c.Summary)
	}
	if got := transitionFor(t, c, "jc-4471").Change; got != ChangeRegressed {
		t.Errorf("jc-4471 change = %s, want regressed", got)
	}
	if !strings.Contains(c.Summary, "despite 4 of 5 earlier silent losses fixed") {
		t.Errorf("summary should still credit the fixes: %q", c.Summary)
	}
	if c.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", c.ExitCode())
	}
}

func TestSilentLossOnANewObjectIsARegression(t *testing.T) {
	a := after(t)
	a.Findings = append(a.Findings, analyze.Finding{
		ObjectID: "ph-007", Workflow: "photo_batch_upload",
		Outcome: analyze.OutcomeSilentLoss, Severity: analyze.SeverityCritical,
	})
	c := mustCompare(t, before(t), a)
	if got := transitionFor(t, c, "ph-007").Change; got != ChangeRegressed {
		t.Errorf("ph-007 change = %s, want regressed", got)
	}
	if c.Verdict != VerdictRegressed {
		t.Errorf("verdict = %s, want REGRESSED", c.Verdict)
	}
}

func TestIdenticalRunsAreUnchanged(t *testing.T) {
	c := mustCompare(t, before(t), before(t))
	if c.Verdict != VerdictUnchanged {
		t.Fatalf("verdict = %s, want UNCHANGED", c.Verdict)
	}
	if !strings.Contains(c.Summary, "the same 5 item(s) are still destroyed or corrupted") {
		t.Errorf("summary = %q", c.Summary)
	}
	if c.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1 while silent loss remains", c.ExitCode())
	}

	clean := analyzeRun(t, "motorway-handover", "motorway-handover")
	c = mustCompare(t, clean, clean)
	if c.Verdict != VerdictUnchanged || c.ExitCode() != 0 {
		t.Errorf("clean vs clean: verdict = %s, exit = %d, want UNCHANGED and 0", c.Verdict, c.ExitCode())
	}
}

func TestSilentLossMissingFromTheAfterRunIsNotAFix(t *testing.T) {
	a := after(t)
	var kept []analyze.Finding
	for _, f := range a.Findings {
		if f.ObjectID != "sig-0091" {
			kept = append(kept, f)
		}
	}
	a.Findings = kept
	c := mustCompare(t, before(t), a)
	if got := transitionFor(t, c, "sig-0091").Change; got != ChangeOnlyBefore {
		t.Errorf("sig-0091 change = %s, want only_before", got)
	}
	if c.Verdict != VerdictPartial {
		t.Errorf("verdict = %s, want PARTIAL: the signature was never re-tested", c.Verdict)
	}
	if !strings.Contains(c.Summary, "1 earlier silent loss(es) were not exercised by the after run") {
		t.Errorf("summary = %q", c.Summary)
	}
	if c.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1", c.ExitCode())
	}
}

func TestSeverityMovementWithoutSilentLoss(t *testing.T) {
	a := after(t)
	setOutcome(t, a, "jc-4471", analyze.OutcomeDuplicated, analyze.SeverityMedium, false)
	c := mustCompare(t, before(t), a)
	if got := transitionFor(t, c, "jc-4471").Change; got != ChangeWorsened {
		t.Errorf("jc-4471 change = %s, want worsened", got)
	}
	if c.Verdict == VerdictRegressed {
		t.Error("a duplicate is a defect but not silent loss; it must not make the verdict REGRESSED")
	}
}

func TestDifferentFingerprintsAreRefused(t *testing.T) {
	b := before(t)
	a := analyzeRun(t, "motorway-handover", "motorway-handover")
	_, err := Compare(b, a, Options{})
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
	if !strings.Contains(err.Error(), "a9ca345d8411f9ef") || !strings.Contains(err.Error(), "d8025cca96dcbd0f") {
		t.Errorf("error should name both fingerprints: %v", err)
	}

	c, err := Compare(b, a, Options{AllowMismatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.FingerprintsMatch {
		t.Error("FingerprintsMatch should be false")
	}
	if len(c.Warnings) == 0 || !strings.HasPrefix(c.Warnings[0], "NOT COMPARABLE:") {
		t.Errorf("first warning should say the runs are not comparable: %v", c.Warnings)
	}
}

func TestRunWarningsAreCarriedOver(t *testing.T) {
	b := before(t)
	b.Warnings = []string{"no screen recording referenced"}
	c := mustCompare(t, b, after(t))
	if len(c.Warnings) != 1 || c.Warnings[0] != "before run: no screen recording referenced" {
		t.Errorf("warnings = %v", c.Warnings)
	}
}

func writeReport(t *testing.T, r *analyze.Report) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadReportRoundTrips(t *testing.T) {
	r, err := LoadReport(writeReport(t, before(t)))
	if err != nil {
		t.Fatal(err)
	}
	if r.SilentLossCount() != 5 || r.ProfileFprint != "a9ca345d8411f9ef" {
		t.Errorf("loaded report: silent = %d, fingerprint = %q", r.SilentLossCount(), r.ProfileFprint)
	}
}

func TestLoadReportRejectsEditedFingerprint(t *testing.T) {
	r := before(t)
	r.ProfileFprint = "d8025cca96dcbd0f"
	_, err := LoadReport(writeReport(t, r))
	if err == nil || !strings.Contains(err.Error(), "edited or is corrupt") {
		t.Fatalf("err = %v, want a fingerprint integrity error", err)
	}
}

func TestLoadReportRejectsOtherJSON(t *testing.T) {
	_, err := LoadReport(filepath.Join(fixtureDir, "nairobi-1700", "client-trace.json"))
	if err == nil || !strings.Contains(err.Error(), "analyze --format json") {
		t.Fatalf("err = %v, want a hint to use analyze --format json", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReport(empty); err == nil || !strings.Contains(err.Error(), "not an analyze report") {
		t.Fatalf("err = %v", err)
	}
}
