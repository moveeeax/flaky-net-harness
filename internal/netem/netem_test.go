package netem

import (
	"strings"
	"testing"

	"github.com/moveeeax/flaky-net-harness/internal/profile"
)

func get(t *testing.T, name string) profile.Profile {
	t.Helper()
	p, err := profile.Builtin().Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func joined(cmds []Command) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(c.String())
		b.WriteString("\n")
	}
	return b.String()
}

func TestSetupShapesBothDirections(t *testing.T) {
	p := get(t, "nairobi-1700")
	plan := BuildPlan(p, Options{})
	setup := joined(plan.Setup)

	for _, want := range []string{
		"tc qdisc add dev eth0 root handle 1: netem",
		"rate 320kbit",
		"tc qdisc add dev ifb0 root handle 1: netem",
		"rate 1200kbit",
		"delay 340ms 120ms distribution normal",
		"loss 3.5% 40%",
		"limit 60",
		"action mirred egress redirect dev ifb0",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("setup is missing %q\n%s", want, setup)
		}
	}
	// Deleting a qdisc that is not there is normal on a fresh namespace.
	if !strings.Contains(setup, "tc qdisc del dev eth0 root || true") {
		t.Errorf("stale-qdisc cleanup should tolerate failure\n%s", setup)
	}
}

func TestUplinkRateIsNotUsedForTheDownlink(t *testing.T) {
	p := get(t, "motorway-handover")
	plan := BuildPlan(p, Options{})
	var egress, ingress string
	for _, c := range plan.Setup {
		line := c.String()
		switch {
		case strings.HasPrefix(line, "tc qdisc add dev eth0 root"):
			egress = line
		case strings.HasPrefix(line, "tc qdisc add dev ifb0 root"):
			ingress = line
		}
	}
	if !strings.Contains(egress, "rate 1500kbit") {
		t.Errorf("egress should carry the uplink rate, got %q", egress)
	}
	if !strings.Contains(ingress, "rate 6000kbit") {
		t.Errorf("ingress should carry the downlink rate, got %q", ingress)
	}
}

func TestStallBlackholesWithoutTearingTheLinkDown(t *testing.T) {
	p := get(t, "depot-basement")
	plan := BuildPlan(p, Options{})
	if len(plan.Timeline) != 2 {
		t.Fatalf("expected 2 stalls, got %d", len(plan.Timeline))
	}
	first := plan.Timeline[0]
	if first.Trigger != "t=45s" || first.Kind != profile.EventStall {
		t.Fatalf("first step = %+v", first)
	}
	apply := joined(first.Apply)
	if !strings.Contains(apply, "netem loss 100%") {
		t.Errorf("a stall must blackhole the link, got:\n%s", apply)
	}
	if strings.Contains(apply, "ip link set") {
		t.Errorf("a stall must not tear the interface down, got:\n%s", apply)
	}
	revert := joined(first.Revert)
	if !strings.Contains(revert, "tc qdisc change dev eth0 root handle 1: netem") || !strings.Contains(revert, "rate 768kbit") {
		t.Errorf("revert must restore the steady-state shape, got:\n%s", revert)
	}
	if first.ForSeconds != 40 {
		t.Errorf("stall duration = %d, want 40", first.ForSeconds)
	}
	if plan.Timeline[1].Trigger != "t=200s" {
		t.Errorf("second step = %q, want t=200s", plan.Timeline[1].Trigger)
	}
}

func TestDisconnectTearsTheInterfaceDownAndBringsItBack(t *testing.T) {
	p := get(t, "motorway-handover")
	plan := BuildPlan(p, Options{})
	if len(plan.Timeline) != 1 {
		t.Fatalf("expected 1 event, got %d", len(plan.Timeline))
	}
	step := plan.Timeline[0]
	if step.Trigger != "upload=60%" {
		t.Errorf("trigger = %q, want upload=60%%", step.Trigger)
	}
	if step.OrderSeconds != OrderTransfer {
		t.Errorf("a transfer-triggered step must not claim a wall-clock slot, got %d", step.OrderSeconds)
	}
	if apply := joined(step.Apply); !strings.Contains(apply, "ip link set dev eth0 down") {
		t.Errorf("disconnect must down the interface, got:\n%s", apply)
	}
	revert := joined(step.Revert)
	if !strings.Contains(revert, "ip link set dev eth0 up") {
		t.Errorf("revert must bring the interface back, got:\n%s", revert)
	}
	if !strings.Contains(revert, "rate 1500kbit") {
		t.Errorf("revert must reapply shaping after the interface returns, got:\n%s", revert)
	}
}

func TestKillAppTargetsTheConfiguredContainer(t *testing.T) {
	p := get(t, "nairobi-1700")
	plan := BuildPlan(p, Options{AppContainer: "vendor app"})
	step := plan.Timeline[0]
	if step.Kind != profile.EventKillApp {
		t.Fatalf("kind = %s, want kill_app", step.Kind)
	}
	got := joined(step.Apply)
	if !strings.Contains(got, "docker kill --signal=KILL 'vendor app'") {
		t.Errorf("kill command should quote the container name, got:\n%s", got)
	}
	if len(step.Revert) != 0 {
		t.Errorf("killing the app changes no network state, so nothing should be reverted: %v", step.Revert)
	}
}

func TestTimelineOrdersClockStepsBeforeTransferSteps(t *testing.T) {
	p := get(t, "depot-basement")
	p.Events = append([]profile.Event{{Kind: profile.EventKillApp, AtTransferPct: 30, Note: "n"}}, p.Events...)
	plan := BuildPlan(p, Options{})
	var order []string
	for _, s := range plan.Timeline {
		order = append(order, s.Trigger)
	}
	want := []string{"t=45s", "t=200s", "upload=30%"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("timeline order = %v, want %v", order, want)
	}
}

func TestScriptIsAnnotatedAndReproducible(t *testing.T) {
	p := get(t, "depot-basement")
	script := BuildPlan(p, Options{}).Script()
	for _, want := range []string{
		"# profile depot-basement v1 (" + p.Fingerprint() + ")",
		"### setup",
		"### timeline",
		"### teardown",
		"sleep 40",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q\n%s", want, script)
		}
	}
	if strings.Contains(script, "sleep 0") {
		t.Error("script should not sleep for a zero-length outage")
	}
}

func TestCommandStringQuotesOnlyWhenNeeded(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"tc", "qdisc", "del", "dev", "eth0", "root"}, "tc qdisc del dev eth0 root"},
		{[]string{"tc", "qdisc", "change", "dev", "eth0", "root", "netem", "loss", "100%"}, "tc qdisc change dev eth0 root netem loss 100%"},
		{[]string{"docker", "kill", "my container"}, "docker kill 'my container'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
	}
	for _, tc := range cases {
		if got := (Command{Argv: tc.argv}).String(); got != tc.want {
			t.Errorf("String(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestShapeOmitsUnsetNetemTerms(t *testing.T) {
	p := profile.Profile{
		Name: "plain", Version: 1, RunSeconds: 10,
		Shape: profile.Shape{DownlinkKbit: 1000, UplinkKbit: 500, QueueLimitPackets: 20},
	}
	line := (Command{Argv: netemArgs("add", "eth0", p.Shape, p.Shape.UplinkKbit)}).String()
	for _, unwanted := range []string{"delay", "loss", "reorder", "duplicate"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("unset term %q leaked into %q", unwanted, line)
		}
	}
	if !strings.Contains(line, "limit 20") || !strings.Contains(line, "rate 500kbit") {
		t.Errorf("line = %q", line)
	}
}

func TestReorderRequiresDelay(t *testing.T) {
	// netem silently ignores reorder without delay; emitting it would make the
	// printed plan claim a condition that was never applied.
	sh := profile.Shape{DownlinkKbit: 1000, UplinkKbit: 500, QueueLimitPackets: 20, ReorderPct: 5}
	line := (Command{Argv: netemArgs("add", "eth0", sh, 500)}).String()
	if strings.Contains(line, "reorder") {
		t.Errorf("reorder must not be emitted without a delay, got %q", line)
	}
}

func TestDefaultsApplyToEmptyOptions(t *testing.T) {
	plan := BuildPlan(get(t, "nairobi-1700"), Options{})
	if plan.Options != DefaultOptions() {
		t.Errorf("options = %+v, want %+v", plan.Options, DefaultOptions())
	}
}
