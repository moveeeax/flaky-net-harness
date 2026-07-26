// Package netem turns a profile into the exact tc/netem and container commands
// that reproduce it. The rendering is pure and testable: `flaky-net-harness
// plan` prints what would run, so a vendor's own network engineer can audit the
// conditions before accepting the findings.
package netem

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/moveeeax/flaky-net-harness/internal/profile"
)

// Options describe the topology the commands are rendered against.
type Options struct {
	// Interface is the shaped interface inside the network namespace.
	Interface string
	// IFBDevice carries redirected ingress traffic so the downlink can be
	// shaped too; netem is egress-only on a real device.
	IFBDevice string
	// AppContainer is the container running the target app, used by
	// process-kill events.
	AppContainer string
}

// DefaultOptions matches the docker-compose topology shipped with the harness.
func DefaultOptions() Options {
	return Options{
		Interface:    "eth0",
		IFBDevice:    "ifb0",
		AppContainer: "fnh-target",
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.Interface == "" {
		o.Interface = d.Interface
	}
	if o.IFBDevice == "" {
		o.IFBDevice = d.IFBDevice
	}
	if o.AppContainer == "" {
		o.AppContainer = d.AppContainer
	}
	return o
}

// Command is a single shell command with the reason it exists.
type Command struct {
	Argv []string
	// Why explains the command in the plan output and the report appendix.
	Why string
	// AllowFailure marks cleanup commands that fail harmlessly on a fresh
	// namespace (for example deleting a qdisc that is not there).
	AllowFailure bool
}

// String renders the command as a copy-pasteable shell line.
func (c Command) String() string {
	parts := make([]string, 0, len(c.Argv))
	for _, a := range c.Argv {
		parts = append(parts, shellQuote(a))
	}
	line := strings.Join(parts, " ")
	if c.AllowFailure {
		line += " || true"
	}
	return line
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '%' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// OrderTransfer marks a step whose trigger is a byte counter rather than a
// clock, so it cannot be scheduled up front.
const OrderTransfer = 1 << 30

// Step is one entry on the run timeline.
type Step struct {
	// Trigger is when the step fires, e.g. "t=45s" or "upload=60%".
	Trigger string
	// OrderSeconds is the scheduled time for time-based steps. Transfer-based
	// steps have no schedulable time — the proxy fires them off its byte
	// counter — so they carry OrderTransfer and are printed last.
	OrderSeconds int
	Kind         profile.EventKind
	Note         string
	Apply        []Command
	// Revert runs ForSeconds after Apply, for self-closing outages.
	Revert     []Command
	ForSeconds int
}

// Plan is the full command sequence for a profile.
type Plan struct {
	Profile  profile.Profile
	Options  Options
	Setup    []Command
	Timeline []Step
	Teardown []Command
}

// BuildPlan renders the tc/netem plan for a profile.
func BuildPlan(p profile.Profile, opts Options) Plan {
	o := opts.withDefaults()
	pl := Plan{Profile: p, Options: o}

	pl.Setup = append(pl.Setup,
		Command{
			Argv:         []string{"tc", "qdisc", "del", "dev", o.Interface, "root"},
			Why:          "clear any qdisc left by a previous run",
			AllowFailure: true,
		},
		Command{
			Argv:         []string{"tc", "qdisc", "del", "dev", o.Interface, "ingress"},
			Why:          "clear the ingress redirect from a previous run",
			AllowFailure: true,
		},
		Command{
			Argv: []string{"ip", "link", "add", o.IFBDevice, "type", "ifb"},
			Why:  "netem shapes egress only; the downlink is shaped on an intermediate functional block device",
			// The device survives between runs inside a long-lived namespace.
			AllowFailure: true,
		},
		Command{
			Argv: []string{"ip", "link", "set", "dev", o.IFBDevice, "up"},
			Why:  "bring the ingress shaping device up",
		},
		Command{
			Argv: netemArgs("add", o.Interface, p.Shape, p.Shape.UplinkKbit),
			Why:  fmt.Sprintf("uplink: %d kbit, %d ms delay ±%d ms, %.4g%% loss", p.Shape.UplinkKbit, p.Shape.DelayMS, p.Shape.JitterMS, p.Shape.LossPct),
		},
		Command{
			Argv: []string{"tc", "qdisc", "add", "dev", o.Interface, "handle", "ffff:", "ingress"},
			Why:  "attach the ingress qdisc so downlink traffic can be redirected",
		},
		Command{
			Argv: []string{"tc", "filter", "add", "dev", o.Interface, "parent", "ffff:", "protocol", "all", "u32",
				"match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", o.IFBDevice},
			Why: "redirect all ingress traffic onto the ifb device",
		},
		Command{
			Argv: netemArgs("add", o.IFBDevice, p.Shape, p.Shape.DownlinkKbit),
			Why:  fmt.Sprintf("downlink: %d kbit under the same delay and loss model", p.Shape.DownlinkKbit),
		},
	)

	for _, e := range p.Events {
		pl.Timeline = append(pl.Timeline, buildStep(p, e, o))
	}
	sort.SliceStable(pl.Timeline, func(i, j int) bool {
		return pl.Timeline[i].OrderSeconds < pl.Timeline[j].OrderSeconds
	})

	pl.Teardown = append(pl.Teardown,
		Command{Argv: []string{"tc", "qdisc", "del", "dev", o.Interface, "root"}, Why: "remove uplink shaping", AllowFailure: true},
		Command{Argv: []string{"tc", "qdisc", "del", "dev", o.Interface, "ingress"}, Why: "remove the ingress redirect", AllowFailure: true},
		Command{Argv: []string{"tc", "qdisc", "del", "dev", o.IFBDevice, "root"}, Why: "remove downlink shaping", AllowFailure: true},
	)
	return pl
}

func buildStep(p profile.Profile, e profile.Event, o Options) Step {
	s := Step{Kind: e.Kind, Note: e.Note, ForSeconds: e.ForSeconds}
	if e.AtTransferPct > 0 {
		s.Trigger = fmt.Sprintf("upload=%d%%", e.AtTransferPct)
		// Transfer-triggered steps are ordered last in the printed plan:
		// their wall-clock time depends on the target's own throughput.
		s.OrderSeconds = OrderTransfer
	} else {
		s.Trigger = fmt.Sprintf("t=%ds", e.AtSeconds)
		s.OrderSeconds = e.AtSeconds
	}

	restore := []Command{
		{Argv: netemArgs("change", o.Interface, p.Shape, p.Shape.UplinkKbit), Why: "restore the steady-state uplink shape"},
		{Argv: netemArgs("change", o.IFBDevice, p.Shape, p.Shape.DownlinkKbit), Why: "restore the steady-state downlink shape"},
	}

	switch e.Kind {
	case profile.EventStall:
		s.Apply = []Command{
			{Argv: []string{"tc", "qdisc", "change", "dev", o.Interface, "root", "netem", "loss", "100%"},
				Why: "blackhole the uplink without tearing the interface down: sockets stay open"},
			{Argv: []string{"tc", "qdisc", "change", "dev", o.IFBDevice, "root", "netem", "loss", "100%"},
				Why: "blackhole the downlink so no ACK returns either"},
		}
		s.Revert = restore
	case profile.EventDisconnect:
		s.Apply = []Command{
			{Argv: []string{"ip", "link", "set", "dev", o.Interface, "down"},
				Why: "hard teardown: the address goes away mid-transfer, as in a cell handover"},
		}
		s.Revert = append([]Command{
			{Argv: []string{"ip", "link", "set", "dev", o.Interface, "up"}, Why: "the radio comes back"},
		}, restore...)
	case profile.EventRestore:
		s.Apply = restore
	case profile.EventKillApp:
		s.Apply = []Command{
			{Argv: []string{"docker", "kill", "--signal=KILL", o.AppContainer},
				Why: "SIGKILL the target the way Android reclaims a backgrounded app: no crash handler runs"},
		}
	}
	return s
}

// netemArgs renders `tc qdisc <verb> dev <dev> root handle 1: netem ...` for a
// shape at a given rate.
func netemArgs(verb, dev string, sh profile.Shape, rateKbit int) []string {
	args := []string{"tc", "qdisc", verb, "dev", dev, "root", "handle", "1:", "netem"}
	args = append(args, "limit", strconv.Itoa(sh.QueueLimitPackets))
	if sh.DelayMS > 0 {
		args = append(args, "delay", ms(sh.DelayMS))
		if sh.JitterMS > 0 {
			args = append(args, ms(sh.JitterMS), "distribution", "normal")
		}
	}
	if sh.LossPct > 0 {
		args = append(args, "loss", pct(sh.LossPct))
		if sh.LossCorrelationPct > 0 {
			// The correlation term makes loss bursty, which is what a shared
			// cell actually does; uniform loss flatters the client.
			args = append(args, pct(sh.LossCorrelationPct))
		}
	}
	if sh.DuplicatePct > 0 {
		args = append(args, "duplicate", pct(sh.DuplicatePct))
	}
	if sh.ReorderPct > 0 && sh.DelayMS > 0 {
		args = append(args, "reorder", pct(sh.ReorderPct))
	}
	if rateKbit > 0 {
		args = append(args, "rate", strconv.Itoa(rateKbit)+"kbit")
	}
	return args
}

func ms(v int) string { return strconv.Itoa(v) + "ms" }
func pct(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}

// Script renders the plan as an annotated shell script. Steps that depend on
// the transfer counter are emitted as comments: the harness fires them from the
// proxy, they are not schedulable up front.
func (p Plan) Script() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# profile %s v%d (%s)\n", p.Profile.Name, p.Profile.Version, p.Profile.Fingerprint())
	fmt.Fprintf(&b, "# interface %s, ingress via %s, run budget %ds\n\n", p.Options.Interface, p.Options.IFBDevice, p.Profile.RunSeconds)
	b.WriteString("### setup\n")
	writeCommands(&b, p.Setup)
	b.WriteString("\n### timeline\n")
	if len(p.Timeline) == 0 {
		b.WriteString("# (no adversarial events: steady state only)\n")
	}
	for _, s := range p.Timeline {
		fmt.Fprintf(&b, "\n# [%s] %s\n", s.Trigger, s.Kind)
		if s.Note != "" {
			fmt.Fprintf(&b, "# %s\n", s.Note)
		}
		writeCommands(&b, s.Apply)
		if len(s.Revert) > 0 && s.ForSeconds > 0 {
			fmt.Fprintf(&b, "sleep %d\n", s.ForSeconds)
			writeCommands(&b, s.Revert)
		}
	}
	b.WriteString("\n### teardown\n")
	writeCommands(&b, p.Teardown)
	return b.String()
}

func writeCommands(b *strings.Builder, cmds []Command) {
	for _, c := range cmds {
		if c.Why != "" {
			fmt.Fprintf(b, "# %s\n", c.Why)
		}
		b.WriteString(c.String())
		b.WriteString("\n")
	}
}
