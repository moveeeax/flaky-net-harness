// Command flaky-net-harness reproduces named, versioned mobile-network
// conditions and proves what a field-service app destroys under them.
//
// This binary owns the parts that are deterministic and auditable: the profile
// definitions, the tc/netem plan they render to, and the diff between what a
// client submitted and what the server actually holds.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/moveeeax/flaky-net-harness/internal/analyze"
	"github.com/moveeeax/flaky-net-harness/internal/netem"
	"github.com/moveeeax/flaky-net-harness/internal/profile"
	"github.com/moveeeax/flaky-net-harness/internal/report"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

const usage = `flaky-net-harness — prove what a field-service app loses on a bad network

Usage:
  flaky-net-harness profiles [--format text|json]
      List the network profiles, their pinned parameters and their fingerprints.

  flaky-net-harness plan --profile <name> [--interface eth0] [--format script|json]
      Print the exact tc/netem and container commands that reproduce a profile.

  flaky-net-harness analyze --profile <name> --trace <client.json> --server <server.json>
                            [--format html|md|json] [--out <path>] [--fail-on silent|any|never]
      Diff what the client submitted against what the server holds and emit the
      loss report. Exit 2 on silent data loss, 1 on loss the operator was told
      about, 0 when everything arrived.

  flaky-net-harness version
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "flaky-net-harness:", err)
		os.Exit(1)
	}
}

// exitCode carries a non-zero result that is a finding, not a failure.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitCode(2)
	}
	switch args[0] {
	case "profiles":
		return cmdProfiles(args[1:], stdout)
	case "plan":
		return cmdPlan(args[1:], stdout)
	case "analyze", "analyse":
		return cmdAnalyze(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, versionString())
		return nil
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdProfiles(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("profiles", flag.ContinueOnError)
	fs.SetOutput(out)
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := profile.Builtin()

	if *format == "json" {
		return writeJSON(out, set.All())
	}
	if *format != "text" {
		return fmt.Errorf("unknown format %q (want text or json)", *format)
	}
	for i, p := range set.All() {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s  v%d  fingerprint %s\n", p.Name, p.Version, p.Fingerprint())
		fmt.Fprintf(out, "  %s\n", p.Summary)
		fmt.Fprintf(out, "  link      %d kbit up / %d kbit down, %d ms ±%d ms, %.4g%% loss (%.4g%% correlated), queue %d pkt\n",
			p.Shape.UplinkKbit, p.Shape.DownlinkKbit, p.Shape.DelayMS, p.Shape.JitterMS,
			p.Shape.LossPct, p.Shape.LossCorrelationPct, p.Shape.QueueLimitPackets)
		fmt.Fprintf(out, "  budget    %s\n", p.Duration())
		if len(p.Events) == 0 {
			fmt.Fprintf(out, "  events    none (steady state only)\n")
			continue
		}
		for j, e := range p.Events {
			label := "events   "
			if j > 0 {
				label = "         "
			}
			fmt.Fprintf(out, "  %s %s %s%s\n", label, eventWhen(e), e.Kind, eventFor(e))
		}
	}
	return nil
}

func eventWhen(e profile.Event) string {
	if e.AtTransferPct > 0 {
		return fmt.Sprintf("at %3d%% of transfer:", e.AtTransferPct)
	}
	return fmt.Sprintf("at t+%-6s     ", fmt.Sprintf("%ds", e.AtSeconds))
}

func eventFor(e profile.Event) string {
	if e.ForSeconds > 0 {
		return fmt.Sprintf(" for %ds", e.ForSeconds)
	}
	return ""
}

func cmdPlan(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(out)
	name := fs.String("profile", "", "profile name (required)")
	iface := fs.String("interface", "", "shaped interface inside the network namespace")
	ifb := fs.String("ifb", "", "ingress shaping device")
	container := fs.String("app-container", "", "container running the target app, for kill events")
	format := fs.String("format", "script", "output format: script or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := lookup(*name)
	if err != nil {
		return err
	}
	plan := netem.BuildPlan(p, netem.Options{Interface: *iface, IFBDevice: *ifb, AppContainer: *container})
	switch *format {
	case "script":
		fmt.Fprint(out, plan.Script())
		return nil
	case "json":
		return writeJSON(out, plan)
	}
	return fmt.Errorf("unknown format %q (want script or json)", *format)
}

func cmdAnalyze(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("profile", "", "profile the run was recorded under (required)")
	tracePath := fs.String("trace", "", "client trace JSON captured by the proxy (required)")
	serverPath := fs.String("server", "", "server state JSON exported after the run (required)")
	format := fs.String("format", "md", "report format: html, md or json")
	outPath := fs.String("out", "", "write the report to this file instead of stdout")
	failOn := fs.String("fail-on", "silent", "exit non-zero on: silent, any, never")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *tracePath == "":
		return errors.New("--trace is required")
	case *serverPath == "":
		return errors.New("--server is required")
	}
	p, err := lookup(*name)
	if err != nil {
		return err
	}
	f, err := report.ParseFormat(*format)
	if err != nil {
		return err
	}
	ct, err := trace.LoadClientTrace(*tracePath)
	if err != nil {
		return err
	}
	ss, err := trace.LoadServerState(*serverPath)
	if err != nil {
		return err
	}
	rep, err := analyze.Analyze(ct, ss, p)
	if err != nil {
		return err
	}

	dst := out
	if *outPath != "" {
		if dir := filepath.Dir(*outPath); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", dir, err)
			}
		}
		fh, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("creating %s: %w", *outPath, err)
		}
		defer fh.Close()
		dst = fh
	}
	if err := report.Render(dst, rep, f); err != nil {
		return err
	}
	if *outPath != "" {
		fmt.Fprintf(errOut, "%s\nwrote %s (%d findings, %d silent)\n",
			rep.Verdict, *outPath, len(rep.Findings), rep.SilentLossCount())
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}

	switch *failOn {
	case "never":
		return nil
	case "silent":
		if rep.SilentLossCount() > 0 {
			return exitCode(2)
		}
		return nil
	case "any":
		if code := rep.ExitCode(); code != 0 {
			return exitCode(code)
		}
		return nil
	}
	return fmt.Errorf("unknown --fail-on %q (want silent, any or never)", *failOn)
}

func lookup(name string) (profile.Profile, error) {
	if name == "" {
		return profile.Profile{}, fmt.Errorf("--profile is required (known: %s)",
			strings.Join(profile.Builtin().Names(), ", "))
	}
	return profile.Builtin().Get(name)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "flaky-net-harness (unknown build)"
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		v = "devel"
	}
	if rev == "" {
		return fmt.Sprintf("flaky-net-harness %s (%s)", v, info.GoVersion)
	}
	return fmt.Sprintf("flaky-net-harness %s %s%s (%s)", v, rev, dirty, info.GoVersion)
}
