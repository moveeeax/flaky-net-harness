package analyze

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/flaky-net-harness/internal/profile"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

const fixtureDir = "../../testdata/runs"

func loadRun(t *testing.T, name string) *Report {
	t.Helper()
	ct, err := trace.LoadClientTrace(filepath.Join(fixtureDir, name, "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := trace.LoadServerState(filepath.Join(fixtureDir, name, "server-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Builtin().Get(name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(ct, ss, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func findingFor(t *testing.T, r *Report, id string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.ObjectID == id {
			return f
		}
	}
	t.Fatalf("no finding for %q", id)
	return Finding{}
}

func TestFailingRunClassifiesEveryObject(t *testing.T) {
	r := loadRun(t, "nairobi-1700")

	want := map[string]Outcome{
		"jc-4471":      OutcomeDelivered,    // complete request, 201, bytes match
		"ph-001":       OutcomeDuplicated,   // arrived, but stored twice by the retry
		"ph-002":       OutcomeCorrupted,    // partial body kept as if complete
		"ph-003":       OutcomeSilentLoss,   // in the killed request, never mentioned again
		"ph-004":       OutcomeSilentLoss,   // same
		"ph-005":       OutcomeReportedLoss, // lost, but the app said so
		"ph-006":       OutcomeSilentLoss,   // client timeout, no error surfaced
		"sig-0091":     OutcomeSilentLoss,   // HTTP 200 and nothing on the server
		"srv-att-77c1": OutcomeUnattributed, // server-side id the client never knew
	}
	if len(r.Findings) != len(want) {
		t.Fatalf("findings = %d, want %d", len(r.Findings), len(want))
	}
	for id, outcome := range want {
		if got := findingFor(t, r, id).Outcome; got != outcome {
			t.Errorf("%s outcome = %s, want %s", id, got, outcome)
		}
	}
}

func TestHTTP200WithNothingOnTheServerIsCritical(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	f := findingFor(t, r, "sig-0091")
	if f.Severity != SeverityCritical {
		t.Errorf("severity = %s, want critical", f.Severity)
	}
	if !f.ClaimedSuccess || f.OperatorWasTold {
		t.Errorf("finding = %+v: the app claimed success and told the operator nothing", f)
	}
	if !strings.Contains(f.Evidence, "HTTP 200") {
		t.Errorf("evidence should quote the status the app was given, got %q", f.Evidence)
	}
	if !f.Silent() {
		t.Error("this is the headline case: it must count as silent")
	}
}

func TestReportedLossIsNotSilent(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	f := findingFor(t, r, "ph-005")
	if f.Silent() {
		t.Error("the operator saw an error, so this is not silent data loss")
	}
	if f.Severity != SeverityMedium {
		t.Errorf("severity = %s, want medium", f.Severity)
	}
}

func TestCorruptedObjectQuotesBothSizes(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	f := findingFor(t, r, "ph-002")
	if f.Severity != SeverityHigh {
		t.Errorf("severity = %s, want high", f.Severity)
	}
	for _, want := range []string{"1204224", "5100991"} {
		if !strings.Contains(f.Evidence, want) {
			t.Errorf("evidence should quote %s, got %q", want, f.Evidence)
		}
	}
}

func TestDuplicateDeliveryNamesTheServerIDs(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	f := findingFor(t, r, "ph-001")
	if f.Severity != SeverityMedium {
		t.Errorf("severity = %s, want medium", f.Severity)
	}
	if !strings.Contains(f.Evidence, "srv-att-77c1") {
		t.Errorf("evidence should name the duplicate server id, got %q", f.Evidence)
	}
	if f.Silent() {
		t.Error("a duplicate is a defect but nothing was destroyed, so it is not silent loss")
	}
	if f.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", f.Attempts)
	}
}

func TestFailingRunTotals(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	if got, want := r.SilentLossCount(), 5; got != want {
		t.Errorf("silent findings = %d, want %d (4 destroyed + 1 corrupted)", got, want)
	}
	if got, want := r.ExitCode(), 2; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.HasPrefix(r.Verdict, "FAIL") {
		t.Errorf("verdict = %q", r.Verdict)
	}
	if r.SeverityCounts[SeverityCritical] != 4 {
		t.Errorf("critical findings = %d, want 4", r.SeverityCounts[SeverityCritical])
	}
	// The bytes are what makes the finding land with a non-engineer.
	if r.SubmittedBytes <= r.LostBytes {
		t.Errorf("submitted %d should exceed lost %d", r.SubmittedBytes, r.LostBytes)
	}
	if r.SilentBytes == 0 || r.SilentBytes > r.LostBytes {
		t.Errorf("silent bytes = %d, lost = %d", r.SilentBytes, r.LostBytes)
	}
	if r.ProfileFprint != "a9ca345d8411f9ef" {
		t.Errorf("report should carry the profile fingerprint, got %q", r.ProfileFprint)
	}
}

func TestFindingsAreOrderedBySeverity(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	last := -1
	for _, f := range r.Findings {
		rank := severityRank[f.Severity]
		if rank < last {
			t.Fatalf("findings are out of severity order at %s (%s)", f.ObjectID, f.Severity)
		}
		last = rank
	}
	if r.Findings[0].Severity != SeverityCritical {
		t.Errorf("the report must open on a critical finding, got %s", r.Findings[0].Severity)
	}
}

func TestPerWorkflowSummary(t *testing.T) {
	r := loadRun(t, "nairobi-1700")
	byName := map[string]WorkflowSummary{}
	for _, w := range r.Workflows {
		byName[w.Workflow] = w
	}
	photos, ok := byName["photo_batch_upload"]
	if !ok {
		t.Fatalf("workflows = %+v", r.Workflows)
	}
	if photos.Submitted != 6 || photos.Arrived != 1 {
		t.Errorf("photo batch = %+v, want 6 submitted / 1 arrived", photos)
	}
	if photos.SilentlyLost != 3 || photos.ReportedLost != 1 || photos.Corrupted != 1 {
		t.Errorf("photo batch = %+v", photos)
	}
	if got := photos.LossPct; got < 83 || got > 84 {
		t.Errorf("photo batch loss = %.2f%%, want ~83.3%%", got)
	}
	if sig := byName["signature_capture"]; sig.LossPct != 100 {
		t.Errorf("signature capture loss = %.2f%%, want 100%%", sig.LossPct)
	}
	// The unattributed server object belongs to no client workflow and must
	// not invent an empty row.
	if _, bad := byName[""]; bad {
		t.Error("an empty workflow row leaked into the summary")
	}
}

func TestCleanRunPasses(t *testing.T) {
	r := loadRun(t, "motorway-handover")
	if got := r.SilentLossCount(); got != 0 {
		t.Errorf("silent findings = %d, want 0", got)
	}
	if got := r.ExitCode(); got != 0 {
		t.Errorf("exit code = %d, want 0", got)
	}
	if !strings.HasPrefix(r.Verdict, "PASS") {
		t.Errorf("verdict = %q, want a PASS", r.Verdict)
	}
	jc := findingFor(t, r, "jc-9902")
	if jc.Outcome != OutcomeDelivered || jc.Attempts != 2 {
		t.Errorf("job card = %+v, want delivered after 2 attempts", jc)
	}
	if !strings.Contains(jc.Evidence, "after 2 attempts") {
		t.Errorf("evidence should mention the retry, got %q", jc.Evidence)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("clean run should raise no caveats, got %v", r.Warnings)
	}
}

func TestWarningsFlagIncomparableRuns(t *testing.T) {
	ct, err := trace.LoadClientTrace(filepath.Join(fixtureDir, "nairobi-1700", "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := trace.LoadServerState(filepath.Join(fixtureDir, "nairobi-1700", "server-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.Builtin().Get("depot-basement")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(ct, ss, p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Warnings, "\n")
	for _, want := range []string{"recorded under", "fingerprint mismatch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings should mention %q, got:\n%s", want, joined)
		}
	}
}

func TestMissingRecordingIsFlagged(t *testing.T) {
	ct := &trace.ClientTrace{
		Run: trace.RunMeta{Profile: "nairobi-1700", ProfileVersion: 1, ProfileFingerprint: "a9ca345d8411f9ef"},
		Attempts: []trace.Attempt{{
			ID: "a", Workflow: "w", StatusCode: 200, DeclaredBytes: 10, SentBytes: 10,
			Objects: []trace.Object{{ID: "o", SHA256: "abc", Bytes: 10}},
		}},
	}
	p, err := profile.Builtin().Get("nairobi-1700")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(ct, &trace.ServerState{}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "no screen recording") {
		t.Errorf("warnings = %v, want the missing-recording caveat", r.Warnings)
	}
	// No recording still means a finding: the object is simply not there.
	if f := findingFor(t, r, "o"); f.Outcome != OutcomeSilentLoss {
		t.Errorf("outcome = %s, want silent_loss", f.Outcome)
	}
}

func TestExitCodeLadder(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want int
	}{
		{"clean", Report{SeverityCounts: map[Severity]int{SeverityInfo: 3}}, 0},
		{"medium only", Report{SeverityCounts: map[Severity]int{SeverityMedium: 1}}, 1},
		{"high", Report{SeverityCounts: map[Severity]int{SeverityHigh: 1}}, 1},
		{"critical", Report{SeverityCounts: map[Severity]int{SeverityCritical: 1, SeverityMedium: 4}}, 2},
	}
	for _, tc := range cases {
		if got := tc.r.ExitCode(); got != tc.want {
			t.Errorf("%s: exit code = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestAnalyzeRejectsMissingInputs(t *testing.T) {
	p, err := profile.Builtin().Get("nairobi-1700")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(nil, &trace.ServerState{}, p); err == nil {
		t.Fatal("expected an error without a client trace")
	}
	if _, err := Analyze(&trace.ClientTrace{}, nil, p); err == nil {
		t.Fatal("expected an error without a server state")
	}
}
