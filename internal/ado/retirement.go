package ado

import (
	"fmt"
	"strings"

	"github.com/wes-key/tf-snag/internal/report"
)

// A retirement raises one work item per catalogue entry, not per affected
// resource: "stop using Basic public IPs" is a single piece of work whatever
// the instance count, and an item per instance would bury a team under ten
// copies of the same decision. The instances are listed in the body, and the
// item's identity is the catalogue id, so the same item survives resources
// coming and going.
func retireTitle(rt report.Retirement) string {
	return clip(fmt.Sprintf("Terraform retirement: %s (%s)", rt.Title, rt.RetiresOn), titleMax)
}

func retireBody(rt report.Retirement, opts Options) string {
	fill, glyph := colUpdate, "⚠"
	if rt.Urgency == "retired" || rt.Urgency == "imminent" {
		fill, glyph = colDelete, "✖"
	}

	var b strings.Builder
	b.WriteString(header(glyph, rt.Title, fill, rt.Type))

	b.WriteString(`<div style="margin:0 0 12px 0">`)
	b.WriteString(badge(titleCase(rt.Urgency), fill))
	b.WriteString(provenanceBadge(rt.BaselineState, rt.Unsuppressed, rt.FirstSeen))
	b.WriteString(`</div>`)

	fmt.Fprintf(&b, `<p><strong>Retires %s</strong> — %s.</p>`, esc(rt.RetiresOn), esc(rt.When()))
	if rt.Remediation != "" {
		fmt.Fprintf(&b, `<p>%s</p>`, esc(rt.Remediation))
	}

	if len(rt.Instances) > 0 {
		fmt.Fprintf(&b, `<p style="margin-bottom:4px;color:%s;font-size:12px">Affected resources (%d)</p><ul style="margin-top:0">`,
			colMuted, len(rt.Instances))
		for _, in := range rt.Instances {
			line := mono(in.Address)
			if loc := location(in.File, in.Line); loc != "" {
				line += "&nbsp;&nbsp;" + muted(loc)
			}
			fmt.Fprintf(&b, `<li>%s</li>`, line)
		}
		b.WriteString(`</ul>`)
	}
	if rt.URL != "" {
		fmt.Fprintf(&b, `<p><a href="%s">%s</a></p>`, esc(rt.URL), esc(rt.URL))
	}

	b.WriteString(footer(opts))
	return b.String()
}
