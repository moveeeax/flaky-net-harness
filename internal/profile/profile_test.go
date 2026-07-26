package profile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinSetIsCompleteAndValid(t *testing.T) {
	set := Builtin()
	want := []string{"depot-basement", "motorway-handover", "nairobi-1700"}
	got := set.Names()
	if len(got) != len(want) {
		t.Fatalf("built-in profiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("built-in profiles = %v, want %v", got, want)
		}
	}
	for _, p := range set.All() {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
		if len(p.Justification) < 80 {
			t.Errorf("%s: justification is too thin to defend in an audit: %q", p.Name, p.Justification)
		}
		if p.Summary == "" {
			t.Errorf("%s: summary is empty", p.Name)
		}
		for i, e := range p.Events {
			if e.Note == "" {
				t.Errorf("%s: event %d has no note explaining why it exists", p.Name, i)
			}
		}
	}
}

// The fingerprint is quoted in every report and is the only thing that makes
// two runs comparable. Changing a shipped profile must change its fingerprint,
// and must be a deliberate act: bump the version when this test fails.
func TestBuiltinFingerprintsArePinned(t *testing.T) {
	want := map[string]string{
		"nairobi-1700":      "a9ca345d8411f9ef",
		"depot-basement":    "faa2e8fd3d56ea0c",
		"motorway-handover": "d8025cca96dcbd0f",
	}
	set := Builtin()
	for name, fp := range want {
		p, err := set.Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if got := p.Fingerprint(); got != fp {
			t.Errorf("%s fingerprint = %s, want %s (bump the profile version if the change is intended)", name, got, fp)
		}
	}
}

func TestFingerprintIgnoresProseButNotParameters(t *testing.T) {
	base, err := Builtin().Get("depot-basement")
	if err != nil {
		t.Fatal(err)
	}
	prose := base
	prose.Summary = "reworded"
	prose.Justification = "reworded"
	prose.Events = append([]Event(nil), base.Events...)
	prose.Events[0].Note = "reworded"
	if prose.Fingerprint() != base.Fingerprint() {
		t.Error("rewording documentation changed the fingerprint; historical runs would stop comparing")
	}

	shaped := base
	shaped.Shape.LossPct += 0.1
	if shaped.Fingerprint() == base.Fingerprint() {
		t.Error("changing loss did not change the fingerprint")
	}

	timed := base
	timed.Events = append([]Event(nil), base.Events...)
	timed.Events[0].ForSeconds = 41
	if timed.Fingerprint() == base.Fingerprint() {
		t.Error("changing an event duration did not change the fingerprint")
	}
}

func TestValidateRejectsUnusableProfiles(t *testing.T) {
	valid := func() Profile {
		return Profile{
			Name: "t", Version: 1, RunSeconds: 60,
			Shape: Shape{DownlinkKbit: 1000, UplinkKbit: 200, DelayMS: 100, JitterMS: 20, QueueLimitPackets: 50},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Profile)
		wantErr string
	}{
		{"no name", func(p *Profile) { p.Name = "" }, "name is empty"},
		{"version zero", func(p *Profile) { p.Version = 0 }, "version must be"},
		{"no uplink", func(p *Profile) { p.Shape.UplinkKbit = 0 }, "must be > 0 kbit"},
		{"jitter over delay", func(p *Profile) { p.Shape.JitterMS = 200 }, "exceeds delay"},
		{"loss over 100", func(p *Profile) { p.Shape.LossPct = 101 }, "loss_pct must be 0..100"},
		{"no queue", func(p *Profile) { p.Shape.QueueLimitPackets = 0 }, "queue_limit_packets"},
		{"no budget", func(p *Profile) { p.RunSeconds = 0 }, "run_seconds must be > 0"},
		{"unknown event", func(p *Profile) {
			p.Events = []Event{{Kind: "explode", AtSeconds: 5}}
		}, "unknown kind"},
		{"event without trigger", func(p *Profile) {
			p.Events = []Event{{Kind: EventStall, ForSeconds: 5}}
		}, "needs at_seconds or at_transfer_pct"},
		{"event with two triggers", func(p *Profile) {
			p.Events = []Event{{Kind: EventStall, AtSeconds: 5, AtTransferPct: 50, ForSeconds: 5}}
		}, "mutually exclusive"},
		{"event past budget", func(p *Profile) {
			p.Events = []Event{{Kind: EventStall, AtSeconds: 900, ForSeconds: 5}}
		}, "after the 60s run budget"},
		{"transfer pct out of range", func(p *Profile) {
			p.Events = []Event{{Kind: EventStall, AtTransferPct: 140, ForSeconds: 5}}
		}, "at_transfer_pct must be 0..100"},
		{"outage never closed", func(p *Profile) {
			p.Events = []Event{{Kind: EventDisconnect, AtSeconds: 5}}
		}, "left down at the end of the run"},
		{"restore without outage", func(p *Profile) {
			p.Events = []Event{{Kind: EventRestore, AtSeconds: 5}}
		}, "link is not down"},
		{"outage inside outage", func(p *Profile) {
			p.Events = []Event{
				{Kind: EventDisconnect, AtSeconds: 5},
				{Kind: EventStall, AtSeconds: 10, ForSeconds: 5},
				{Kind: EventRestore, AtSeconds: 20},
			}
		}, "already down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestOpenEndedOutageClosedByRestoreIsValid(t *testing.T) {
	p := Profile{
		Name: "t", Version: 1, RunSeconds: 60,
		Shape:  Shape{DownlinkKbit: 1000, UplinkKbit: 200, DelayMS: 100, QueueLimitPackets: 50},
		Events: []Event{{Kind: EventDisconnect, AtSeconds: 10}, {Kind: EventRestore, AtSeconds: 40}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid open-ended outage rejected: %v", err)
	}
}

func TestLoadRejectsBadSets(t *testing.T) {
	cases := map[string]string{
		"empty set":      `[]`,
		"unknown field":  `[{"name":"x","version":1,"run_seconds":10,"bandwidth":"fast","shape":{"downlink_kbit":1,"uplink_kbit":1,"queue_limit_packets":1}}]`,
		"invalid member": `[{"name":"x","version":1,"run_seconds":10,"shape":{"downlink_kbit":0,"uplink_kbit":1,"queue_limit_packets":1}}]`,
		"duplicate name": `[
			{"name":"x","version":1,"run_seconds":10,"shape":{"downlink_kbit":1,"uplink_kbit":1,"queue_limit_packets":1}},
			{"name":"x","version":2,"run_seconds":10,"shape":{"downlink_kbit":1,"uplink_kbit":1,"queue_limit_packets":1}}]`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load([]byte(doc)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestSetGetUnknownNameListsAlternatives(t *testing.T) {
	_, err := Builtin().Get("lagos-0300")
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), "nairobi-1700") {
		t.Errorf("error should list the known profiles, got %v", err)
	}
}

func TestProfilesRoundTripThroughJSON(t *testing.T) {
	orig := Builtin()
	data, err := json.Marshal(orig.All())
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(data)
	if err != nil {
		t.Fatalf("re-loading marshalled profiles: %v", err)
	}
	for _, p := range orig.All() {
		q, err := again.Get(p.Name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Fingerprint() != q.Fingerprint() {
			t.Errorf("%s: fingerprint changed across a JSON round trip", p.Name)
		}
	}
}
