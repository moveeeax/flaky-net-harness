package report

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/flaky-net-harness/internal/analyze"
	"github.com/moveeeax/flaky-net-harness/internal/compare"
	"github.com/moveeeax/flaky-net-harness/internal/profile"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

const fixtureDir = "../../testdata/runs"

func fixtureReport(t *testing.T, name string) *analyze.Report {
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
	r, err := analyze.Analyze(ct, ss, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func render(t *testing.T, r *analyze.Report, f Format) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, r, f); err != nil {
		t.Fatalf("Render(%s): %v", f, err)
	}
	return buf.String()
}

func TestMarkdownReportCarriesTheArgument(t *testing.T) {
	out := render(t, fixtureReport(t, "nairobi-1700"), FormatMarkdown)
	for _, want := range []string{
		"# Loss report — Example FSM 4.2.1 (anonymised trial build)",
		"**Verdict: FAIL",
		"5 item(s) destroyed or corrupted with no user-visible error",
		"`nairobi-1700` v1 (fingerprint `a9ca345d8411f9ef`)",
		"| photo batch upload |",
		"the app received HTTP 200 and showed no error",
		"## Reproducing these conditions",
		"tc qdisc add dev eth0 root handle 1: netem",
		"docker kill --signal=KILL fnh-target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown report is missing %q", want)
		}
	}
	// Every finding must appear as its own row.
	for _, id := range []string{"ph-001", "ph-002", "ph-003", "ph-004", "ph-005", "ph-006", "sig-0091", "jc-4471"} {
		if !strings.Contains(out, "`"+id+"`") {
			t.Errorf("markdown report is missing object %s", id)
		}
	}
}

func TestMarkdownCleanRunReadsAsAPass(t *testing.T) {
	out := render(t, fixtureReport(t, "motorway-handover"), FormatMarkdown)
	if !strings.Contains(out, "**Verdict: PASS") {
		t.Errorf("clean run should render a PASS verdict:\n%s", out)
	}
	if strings.Contains(out, "## Caveats on this run") {
		t.Error("clean run should not render a caveats section")
	}
}

func TestHTMLReportIsSelfContainedAndEscaped(t *testing.T) {
	r := fixtureReport(t, "nairobi-1700")
	r.Run.Target = `Example <script>alert(1)</script> build`
	out := render(t, r, FormatHTML)

	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("target name was not escaped")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("expected the escaped target name in the output")
	}
	for _, forbidden := range []string{"src=\"http", "href=\"http", "@import"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("report must not reference external resources, found %q", forbidden)
		}
	}
	for _, want := range []string{
		"<!doctype html>",
		"prefers-color-scheme: dark",
		"class=\"verdict fail\"",
		"sev-critical",
		"Reproducing these conditions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html report is missing %q", want)
		}
	}
}

func TestJSONReportRoundTrips(t *testing.T) {
	out := render(t, fixtureReport(t, "nairobi-1700"), FormatJSON)
	var back analyze.Report
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("report JSON does not round trip: %v", err)
	}
	if len(back.Findings) != 9 {
		t.Errorf("findings = %d, want 9", len(back.Findings))
	}
	if back.ProfileFprint != "a9ca345d8411f9ef" {
		t.Errorf("fingerprint = %q", back.ProfileFprint)
	}
	if back.SilentLossCount() != 5 {
		t.Errorf("silent count after round trip = %d, want 5", back.SilentLossCount())
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"html": FormatHTML, "HTML": FormatHTML,
		"md": FormatMarkdown, "markdown": FormatMarkdown,
		"json": FormatJSON,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("pdf"); err == nil {
		t.Error("expected an error for an unsupported format")
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		1048576: "1.0 MB", 26214400: "25.0 MB", 1073741824: "1.0 GB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderRejectsUnknownFormat(t *testing.T) {
	if err := Render(&bytes.Buffer{}, fixtureReport(t, "nairobi-1700"), Format("pdf")); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}

func fixtureComparison(t *testing.T) *compare.Comparison {
	t.Helper()
	b := fixtureReport(t, "nairobi-1700")
	ct, err := trace.LoadClientTrace(filepath.Join(fixtureDir, "nairobi-1700-fix", "client-trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := trace.LoadServerState(filepath.Join(fixtureDir, "nairobi-1700-fix", "server-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyze.Analyze(ct, ss, b.Profile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := compare.Compare(b, a, compare.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func renderComparison(t *testing.T, c *compare.Comparison, f Format) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderComparison(&buf, c, f); err != nil {
		t.Fatalf("RenderComparison(%s): %v", f, err)
	}
	return buf.String()
}

func TestMarkdownComparisonShowsTransitions(t *testing.T) {
	out := renderComparison(t, fixtureComparison(t), FormatMarkdown)
	for _, want := range []string{
		"# Fix verification — nairobi-1700",
		"**Verdict: PARTIAL — 4 of 5 silent losses fixed, 1 still lost silently, none new**",
		"| Target | Example FSM 4.2.1 (anonymised trial build) | Example FSM 4.2.2 (vendor fix build, anonymised) |",
		"| Silently lost or corrupted | 5 (19.4 MB) | 1 (89.4 KB) |",
		"| STILL SILENT | `sig-0091`",
		"| FIXED | `ph-003` — Fault — cracked heat exchanger | photo batch upload | silent loss | delivered |",
		"| ONLY BEFORE | `srv-att-77c1` |  | unattributed | absent |",
		"- 4 fixed, 0 regressed, 1 still lost silently.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown comparison is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## Caveats on this comparison") {
		t.Error("matching fingerprints should not render a caveats section")
	}
}

func TestComparisonCaveatsAreRendered(t *testing.T) {
	c := fixtureComparison(t)
	c.FingerprintsMatch = false
	c.Warnings = []string{"NOT COMPARABLE: the network conditions differ"}
	md := renderComparison(t, c, FormatMarkdown)
	for _, want := range []string{"## Caveats on this comparison", "- NOT COMPARABLE:", "(DIFFERS)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown comparison is missing %q", want)
		}
	}
	if html := renderComparison(t, c, FormatHTML); !strings.Contains(html, `class="warn"`) {
		t.Error("html comparison should render the caveats block")
	}
}

func TestHTMLComparisonIsSelfContainedAndEscaped(t *testing.T) {
	c := fixtureComparison(t)
	c.After.Target = `Fix <script>alert(1)</script> build`
	out := renderComparison(t, c, FormatHTML)
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("target name was not escaped")
	}
	for _, forbidden := range []string{"src=\"http", "href=\"http", "@import"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("comparison must not reference external resources, found %q", forbidden)
		}
	}
	for _, want := range []string{
		"<!doctype html>",
		"prefers-color-scheme: dark",
		`class="verdict partial"`,
		"chg-still_silent",
		"chg-fixed",
		"5 → 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html comparison is missing %q", want)
		}
	}
}

func TestJSONComparisonRoundTrips(t *testing.T) {
	out := renderComparison(t, fixtureComparison(t), FormatJSON)
	var back compare.Comparison
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("comparison JSON does not round trip: %v", err)
	}
	if back.Verdict != compare.VerdictPartial || len(back.Transitions) != 9 {
		t.Errorf("verdict = %s, transitions = %d", back.Verdict, len(back.Transitions))
	}
	if back.ChangeCounts[compare.ChangeFixed] != 4 {
		t.Errorf("fixed = %d, want 4", back.ChangeCounts[compare.ChangeFixed])
	}
}

func TestRenderComparisonRejectsUnknownFormat(t *testing.T) {
	if err := RenderComparison(&bytes.Buffer{}, fixtureComparison(t), Format("pdf")); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}
