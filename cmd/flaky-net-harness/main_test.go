package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../testdata/runs"

// exec runs the CLI in-process and returns stdout, stderr and the exit code.
func exec(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, &out, &errOut)
	code := 0
	if err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			code = int(ec)
		} else {
			code = 1
			errOut.WriteString(err.Error() + "\n")
		}
	}
	return out.String(), errOut.String(), code
}

func TestProfilesText(t *testing.T) {
	out, _, code := exec(t, "profiles")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
	for _, want := range []string{
		"nairobi-1700  v1  fingerprint a9ca345d8411f9ef",
		"depot-basement",
		"motorway-handover",
		"at  60% of transfer: disconnect for 90s",
		"320 kbit up / 1200 kbit down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("profiles output is missing %q:\n%s", want, out)
		}
	}
}

func TestProfilesJSON(t *testing.T) {
	out, _, code := exec(t, "profiles", "--format", "json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"downlink_kbit": 1200`) {
		t.Errorf("json output looks wrong:\n%s", out)
	}
}

func TestProfilesRejectsUnknownFormat(t *testing.T) {
	_, errOut, code := exec(t, "profiles", "--format", "yaml")
	if code == 0 {
		t.Fatal("expected a non-zero exit")
	}
	if !strings.Contains(errOut, "unknown format") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestPlanScript(t *testing.T) {
	out, _, code := exec(t, "plan", "--profile", "depot-basement")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"# profile depot-basement v1 (faa2e8fd3d56ea0c)",
		"tc qdisc add dev eth0 root handle 1: netem",
		"netem loss 100%",
		"sleep 40",
		"### teardown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan is missing %q:\n%s", want, out)
		}
	}
}

func TestPlanHonoursTopologyFlags(t *testing.T) {
	out, _, code := exec(t, "plan", "--profile", "nairobi-1700",
		"--interface", "wlan0", "--ifb", "ifb7", "--app-container", "vendor-apk")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"dev wlan0", "dev ifb7", "docker kill --signal=KILL vendor-apk"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dev eth0") {
		t.Errorf("plan should not fall back to eth0:\n%s", out)
	}
}

func TestPlanRequiresAKnownProfile(t *testing.T) {
	_, errOut, code := exec(t, "plan")
	if code == 0 || !strings.Contains(errOut, "--profile is required") {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	_, errOut, code = exec(t, "plan", "--profile", "atlantic-crossing")
	if code == 0 || !strings.Contains(errOut, "unknown profile") {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestAnalyzeFailingRunExitsTwo(t *testing.T) {
	out, _, code := exec(t, "analyze",
		"--profile", "nairobi-1700",
		"--trace", filepath.Join(fixtures, "nairobi-1700", "client-trace.json"),
		"--server", filepath.Join(fixtures, "nairobi-1700", "server-state.json"))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (silent data loss)", code)
	}
	if !strings.Contains(out, "**Verdict: FAIL") {
		t.Errorf("report should open on the verdict:\n%s", out)
	}
}

func TestAnalyzeCleanRunExitsZero(t *testing.T) {
	_, _, code := exec(t, "analyze",
		"--profile", "motorway-handover",
		"--trace", filepath.Join(fixtures, "motorway-handover", "client-trace.json"),
		"--server", filepath.Join(fixtures, "motorway-handover", "server-state.json"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestAnalyzeWritesHTMLToDisk(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nested", "report.html")
	_, errOut, code := exec(t, "analyze",
		"--profile", "nairobi-1700",
		"--trace", filepath.Join(fixtures, "nairobi-1700", "client-trace.json"),
		"--server", filepath.Join(fixtures, "nairobi-1700", "server-state.json"),
		"--format", "html", "--out", dst)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "wrote "+dst) {
		t.Errorf("stderr should confirm the write, got %q", errOut)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("<!doctype html>")) {
		t.Error("written file is not the HTML report")
	}
	if !bytes.Contains(data, []byte("a9ca345d8411f9ef")) {
		t.Error("written report should carry the profile fingerprint")
	}
}

func TestAnalyzeFailOnModes(t *testing.T) {
	base := []string{"analyze",
		"--profile", "nairobi-1700",
		"--trace", filepath.Join(fixtures, "nairobi-1700", "client-trace.json"),
		"--server", filepath.Join(fixtures, "nairobi-1700", "server-state.json"),
		"--format", "json"}

	if _, _, code := exec(t, append(base, "--fail-on", "never")...); code != 0 {
		t.Errorf("--fail-on never: exit = %d, want 0", code)
	}
	if _, _, code := exec(t, append(base, "--fail-on", "any")...); code != 2 {
		t.Errorf("--fail-on any: exit = %d, want 2", code)
	}
	_, errOut, code := exec(t, append(base, "--fail-on", "sometimes")...)
	if code == 0 || !strings.Contains(errOut, "unknown --fail-on") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestAnalyzeReportsMismatchedProfileOnStderr(t *testing.T) {
	_, errOut, _ := exec(t, "analyze",
		"--profile", "depot-basement",
		"--trace", filepath.Join(fixtures, "nairobi-1700", "client-trace.json"),
		"--server", filepath.Join(fixtures, "nairobi-1700", "server-state.json"),
		"--format", "json")
	if !strings.Contains(errOut, "warning: trace was recorded under") {
		t.Errorf("stderr should warn about the profile mismatch, got %q", errOut)
	}
}

func TestAnalyzeRequiresItsInputs(t *testing.T) {
	cases := [][]string{
		{"analyze", "--profile", "nairobi-1700"},
		{"analyze", "--profile", "nairobi-1700", "--trace", filepath.Join(fixtures, "nairobi-1700", "client-trace.json")},
	}
	for _, args := range cases {
		if _, _, code := exec(t, args...); code == 0 {
			t.Errorf("%v: expected a non-zero exit", args)
		}
	}
}

func TestAnalyzeRejectsABrokenTrace(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(bad, []byte(`{"run":{"profile":"nairobi-1700"},"attempts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := exec(t, "analyze", "--profile", "nairobi-1700",
		"--trace", bad, "--server", filepath.Join(fixtures, "nairobi-1700", "server-state.json"))
	if code == 0 {
		t.Fatal("expected a non-zero exit")
	}
	if !strings.Contains(errOut, "no attempts captured") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestVersionAndHelp(t *testing.T) {
	out, _, code := exec(t, "version")
	if code != 0 || !strings.HasPrefix(out, "flaky-net-harness ") {
		t.Errorf("version = %q, exit = %d", out, code)
	}
	out, _, code = exec(t, "--help")
	if code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("help = %q, exit = %d", out, code)
	}
	if _, _, code := exec(t); code != 2 {
		t.Errorf("bare invocation: exit = %d, want 2", code)
	}
	if _, _, code := exec(t, "teleport"); code == 0 {
		t.Error("unknown command should exit non-zero")
	}
}
