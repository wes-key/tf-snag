package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/wes-key/tf-snag/internal/retire"
)

// Retirement is one catalogue entry matched against this estate: a dated
// deadline from the cloud provider, with every resource it applies to.
//
// Unlike drift and deprecations, a retirement is not derived from the plan
// alone - it needs the catalogue tf-snag ships with, which is why the report
// also carries CatalogueInfo saying how old that catalogue is.
type Retirement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	RetiresOn   string `json:"retires_on"`
	URL         string `json:"url,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Type        string `json:"resource_type,omitempty"`
	// Urgency is retired, imminent, approaching or scheduled, worked out
	// against the time of the run rather than stored in the catalogue.
	Urgency string `json:"urgency"`
	// Days until retirement, negative once the date has passed.
	Days      int                  `json:"days"`
	Instances []RetirementInstance `json:"instances,omitempty"`

	Suppressed     bool   `json:"suppressed,omitempty"`
	SuppressReason string `json:"suppress_reason,omitempty"`
	SuppressSrc    string `json:"suppress_source,omitempty"`
	SuppressKind   string `json:"-"`

	// Set by PriorResults.StampReport when -baseline was given.
	BaselineState string `json:"baseline_state,omitempty"`
	FirstSeen     string `json:"first_seen,omitempty"`
	FirstRunURL   string `json:"first_run_url,omitempty"`
	Unsuppressed  bool   `json:"unsuppressed,omitempty"`

	// Set by the work-item pass when -ado-url is given.
	WorkItem    int    `json:"work_item,omitempty"`
	WorkItemURL string `json:"work_item_url,omitempty"`
}

// RetirementInstance is one affected resource.
type RetirementInstance struct {
	Address string `json:"address"`
	Module  string `json:"module,omitempty"`
	// Matched is the attribute value that made this resource match, so a reader
	// can see why rather than take the finding on trust.
	Matched map[string]any `json:"matched,omitempty"`

	// Set by AttachSourceLocations when -source was given.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
}

// CatalogueInfo records which retirement catalogue produced the findings and
// how old it is. A stale catalogue turns "no retirements" from a result into a
// claim tf-snag has not actually checked, so the report says so rather than
// leaving a reader to assume.
type CatalogueInfo struct {
	Source  string `json:"source"`            // "built-in", or the -retirements path
	Updated string `json:"updated,omitempty"` // YYYY-MM-DD
	AgeDays int    `json:"age_days,omitempty"`
	Stale   bool   `json:"stale,omitempty"`
}

// StaleAfterDays is when a catalogue stops being trustworthy enough to report a
// clean result without a caveat. Retirements are announced months ahead, so a
// quarter-old catalogue has probably missed one.
const StaleAfterDays = 90

// AttachRetirements converts matched catalogue findings into report entries.
func (r *Report) AttachRetirements(findings []retire.Finding, c *retire.Catalogue, now time.Time) {
	for _, f := range findings {
		rt := Retirement{
			ID:          f.Entry.ID,
			Title:       f.Entry.Title,
			RetiresOn:   f.Entry.RetiresOn.String(),
			URL:         f.Entry.URL,
			Severity:    f.Entry.Severity,
			Remediation: strings.TrimSpace(f.Entry.Remediation),
			Type:        f.Entry.Match.Type,
			Urgency:     string(f.Urgency),
			Days:        f.Days,
		}
		for _, in := range f.Instances {
			rt.Instances = append(rt.Instances, RetirementInstance{
				Address: in.Address,
				Module:  moduleOf(in.Address),
				Matched: in.Matched,
			})
		}
		r.Retirements = append(r.Retirements, rt)
	}
	if c != nil {
		age := int(c.Age(now).Hours() / 24)
		r.Catalogue = &CatalogueInfo{
			Source:  c.Source,
			Updated: c.Updated.String(),
			AgeDays: age,
			Stale:   age > StaleAfterDays,
		}
	}
}

// moduleOf recovers the module address from a resource address, so a retirement
// instance reads like the rest of the report.
func moduleOf(address string) string {
	i := strings.LastIndex(address, "module.")
	if i < 0 {
		return ""
	}
	parts := strings.SplitN(address[i:], ".", 3)
	if len(parts) < 3 {
		return ""
	}
	return address[:i] + parts[0] + "." + parts[1]
}

// HasRetirements reports whether any retirement matched this estate.
func (r *Report) HasRetirements() bool { return len(r.Retirements) > 0 }

func (r *Report) gatingRetirements() (gating, ignored []Retirement) {
	for _, rt := range r.Retirements {
		switch {
		case rt.Suppressed:
			ignored = append(ignored, rt)
		case r.RetirementGateDays > 0 && rt.Days > r.RetirementGateDays:
			// Outside the -retirements-fail-within window: reported, but not a
			// reason to fail a build today.
			continue
		default:
			gating = append(gating, rt)
		}
	}
	return
}

// When phrases a retirement's deadline in relation to the run.
func (rt Retirement) When() string {
	switch {
	case rt.Days < 0:
		return fmt.Sprintf("retired %s ago", days(-rt.Days))
	case rt.Days == 0:
		return "retires today"
	default:
		return fmt.Sprintf("in %s", days(rt.Days))
	}
}

func days(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// writeRetirements prints the un-suppressed retirement section, soonest first.
func (r *Report) writeRetirements(bw *errWriter, c palette) {
	rets, _ := r.gatingRetirements()
	if len(rets) == 0 {
		return
	}
	bw.printf("\n%sretirement(s): %d%s\n", c.dim, len(rets), c.reset)
	for _, rt := range rets {
		colour := c.yellow
		if rt.Urgency == string(retire.Retired) || rt.Urgency == string(retire.Imminent) {
			colour = c.red
		}
		bw.printf("  %s!%s %s%s%s — %s (%s), %d instance(s)\n",
			colour, c.reset, c.bold, rt.Type, c.reset, rt.Title, rt.RetiresOn, len(rt.Instances))
		for _, in := range rt.Instances {
			bw.printf("      %s%s%s\n", c.dim, in.Address, c.reset)
		}
		bw.printf("      %s%s", c.dim, rt.When())
		if rt.Remediation != "" {
			bw.printf(" — %s", rt.Remediation)
		}
		bw.printf("%s\n", c.reset)
		if rt.URL != "" {
			bw.printf("      %s%s%s\n", c.dim, rt.URL, c.reset)
		}
	}
	if r.Catalogue != nil && r.Catalogue.Stale {
		bw.printf("\n%sretirement catalogue (%s) was updated %s, %d days ago: it may be missing newer notices%s\n",
			c.yellow, r.Catalogue.Source, r.Catalogue.Updated, r.Catalogue.AgeDays, c.reset)
	}
}
