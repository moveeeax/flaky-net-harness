package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../testdata/runs"

func TestLoadFixtureRun(t *testing.T) {
	ct, err := LoadClientTrace(filepath.Join(fixtureDir, "nairobi-1700", "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ct.Run.Profile != "nairobi-1700" || ct.Run.ProfileVersion != 1 {
		t.Errorf("run meta = %+v", ct.Run)
	}
	if got, want := len(ct.Attempts), 6; got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
	if got, want := len(ct.ObjectIDs()), 8; got != want {
		t.Errorf("distinct objects = %d, want %d", got, want)
	}
	wantWorkflows := "job_card_submit,photo_batch_upload,signature_capture"
	if got := strings.Join(ct.Workflows(), ","); got != wantWorkflows {
		t.Errorf("workflows = %s, want %s", got, wantWorkflows)
	}

	ss, err := LoadServerState(filepath.Join(fixtureDir, "nairobi-1700", "server-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ss.Get("jc-4471"); !ok {
		t.Error("server state should hold the job card")
	}
	if _, ok := ss.Get("ph-003"); ok {
		t.Error("server state must not hold the photo the app destroyed")
	}
}

func TestAttemptsForFollowsRetriesInOrder(t *testing.T) {
	ct, err := LoadClientTrace(filepath.Join(fixtureDir, "nairobi-1700", "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := ct.AttemptsFor("ph-001")
	if len(got) != 2 {
		t.Fatalf("ph-001 attempts = %d, want 2", len(got))
	}
	if got[0].ID != "a2" || got[1].ID != "a3" {
		t.Errorf("attempt order = %s,%s, want a2,a3", got[0].ID, got[1].ID)
	}
	if got[0].Complete() {
		t.Error("the killed attempt should not be complete")
	}
	if !got[1].Complete() || !got[1].Succeeded() {
		t.Error("the retry should be complete and successful")
	}
}

func TestTransferPct(t *testing.T) {
	cases := []struct {
		a    Attempt
		want float64
	}{
		{Attempt{DeclaredBytes: 1000, SentBytes: 250}, 25},
		{Attempt{DeclaredBytes: 1000, SentBytes: 0}, 0},
		{Attempt{DeclaredBytes: 0, SentBytes: 500}, 0},
		{Attempt{DeclaredBytes: 1000, SentBytes: 1200}, 100},
	}
	for _, tc := range cases {
		if got := tc.a.TransferPct(); got != tc.want {
			t.Errorf("TransferPct(%d/%d) = %g, want %g", tc.a.SentBytes, tc.a.DeclaredBytes, got, tc.want)
		}
	}
}

func TestSucceededOnlyFor2xx(t *testing.T) {
	for code, want := range map[int]bool{0: false, 199: false, 200: true, 201: true, 299: true, 302: false, 500: false} {
		if got := (Attempt{StatusCode: code}).Succeeded(); got != want {
			t.Errorf("Succeeded(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestClientTraceValidationRejectsUnusableInput(t *testing.T) {
	cases := map[string]string{
		"no profile":  `{"run":{},"attempts":[]}`,
		"no attempts": `{"run":{"profile":"nairobi-1700"},"attempts":[]}`,
		"duplicate attempt id": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","status_code":200,"objects":[{"id":"o","sha256":"x"}]},
			{"id":"a","workflow":"w","status_code":200,"objects":[{"id":"o2","sha256":"y"}]}]}`,
		"no workflow": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","status_code":200,"objects":[{"id":"o","sha256":"x"}]}]}`,
		"no outcome recorded": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","objects":[{"id":"o","sha256":"x"}]}]}`,
		"no objects": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","status_code":200,"objects":[]}]}`,
		"object without hash": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","status_code":200,"objects":[{"id":"o"}]}]}`,
		"object with conflicting hashes": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","status_code":200,"objects":[{"id":"o","sha256":"x"}]},
			{"id":"b","workflow":"w","status_code":200,"objects":[{"id":"o","sha256":"y"}]}]}`,
		"negative bytes": `{"run":{"profile":"p"},"attempts":[
			{"id":"a","workflow":"w","status_code":200,"sent_bytes":-1,"objects":[{"id":"o","sha256":"x"}]}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, doc)
			if _, err := LoadClientTrace(path); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestClientTraceRejectsUnknownFields(t *testing.T) {
	// A typo in a field name would otherwise be silently dropped and change
	// the classification of a finding.
	doc := `{"run":{"profile":"p"},"attempts":[
		{"id":"a","workflow":"w","status_code":200,"user_visible_errors":true,"objects":[{"id":"o","sha256":"x"}]}]}`
	if _, err := LoadClientTrace(writeTemp(t, doc)); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestServerStateRejectsDuplicateIDs(t *testing.T) {
	doc := `{"source":"s","objects":[{"id":"o"},{"id":"o"}]}`
	if _, err := LoadServerState(writeTemp(t, doc)); err == nil {
		t.Fatal("expected an error for a duplicate server object id")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := LoadClientTrace(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestHumanise(t *testing.T) {
	for in, want := range map[string]string{
		"photo_batch_upload": "photo batch upload",
		"silent_loss":        "silent loss",
		"job-card":           "job card",
		"":                   "",
	} {
		if got := Humanise(in); got != want {
			t.Errorf("Humanise(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeTemp(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
