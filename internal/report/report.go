// Package report renders an analysis as the artefact that is actually handed
// over: a self-contained HTML page, a Markdown file for the repo, or JSON for
// a pipeline.
package report

import (
	"embed"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"io"
	"strings"
	texttemplate "text/template"

	"github.com/moveeeax/flaky-net-harness/internal/analyze"
	"github.com/moveeeax/flaky-net-harness/internal/netem"
	"github.com/moveeeax/flaky-net-harness/internal/trace"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Format is an output format for a report.
type Format string

const (
	FormatHTML     Format = "html"
	FormatMarkdown Format = "md"
	FormatJSON     Format = "json"
)

// ParseFormat validates a user-supplied format string.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(s)) {
	case FormatHTML:
		return FormatHTML, nil
	case FormatMarkdown, "markdown":
		return FormatMarkdown, nil
	case FormatJSON:
		return FormatJSON, nil
	}
	return "", fmt.Errorf("unknown format %q (want html, md or json)", s)
}

// view is what the templates see: the report plus the reproduction script.
type view struct {
	*analyze.Report
	Script string
}

func funcs() map[string]any {
	return map[string]any{
		"humanise": trace.Humanise,
		"bytes":    humanBytes,
		"pct":      func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		"pct1":     func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
		"upper":    strings.ToUpper,
		"sevClass": func(s analyze.Severity) string { return "sev-" + string(s) },
		"count": func(m map[analyze.Outcome]int, k string) int {
			return m[analyze.Outcome(k)]
		},
		"sevCount": func(m map[analyze.Severity]int, k string) int {
			return m[analyze.Severity(k)]
		},
	}
}

// Render writes the report in the requested format.
func Render(w io.Writer, r *analyze.Report, f Format) error {
	v := view{Report: r, Script: netem.BuildPlan(r.Profile, netem.DefaultOptions()).Script()}
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case FormatMarkdown:
		t, err := texttemplate.New("report.md.tmpl").Funcs(funcs()).ParseFS(templatesFS, "templates/report.md.tmpl")
		if err != nil {
			return fmt.Errorf("report: parsing markdown template: %w", err)
		}
		return t.Execute(w, v)
	case FormatHTML:
		t, err := htmltemplate.New("report.html.tmpl").Funcs(funcs()).ParseFS(templatesFS, "templates/report.html.tmpl")
		if err != nil {
			return fmt.Errorf("report: parsing html template: %w", err)
		}
		return t.Execute(w, v)
	}
	return fmt.Errorf("report: unsupported format %q", f)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
