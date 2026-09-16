package retire

import (
	"fmt"
	"sort"
	"time"

	"github.com/wes-key/tf-snag/internal/plan"
)

// Urgency is how close a retirement is, worked out at run time rather than
// stored: the same catalogue entry is informational a year out and a blocker
// the week before, and only the run knows which.
type Urgency string

const (
	// Retired: the date has passed. The resource may still work - Azure often
	// leaves retired SKUs running, unsupported and outside the SLA - but nobody
	// is coming to fix it.
	Retired Urgency = "retired"
	// Imminent: inside 90 days. Short enough that a change has to be planned now.
	Imminent Urgency = "imminent"
	// Approaching: inside 180 days, so it belongs in the next planning round.
	Approaching Urgency = "approaching"
	// Scheduled: announced, but far enough out to be a note rather than a task.
	Scheduled Urgency = "scheduled"
)

// Thresholds for the two urgent bands, in days.
const (
	imminentDays    = 90
	approachingDays = 180
)

// Finding is one catalogue entry matched against the estate, with every
// resource it applies to. Findings are per entry rather than per resource: the
// decision ("stop using Basic public IPs") is one piece of work whatever the
// instance count, and a finding per instance would raise ten work items for it.
type Finding struct {
	Entry     Entry
	Instances []Instance
	Urgency   Urgency
	// Days until retirement, negative once it has passed.
	Days int
}

// Instance is one affected resource.
type Instance struct {
	Address string
	Type    string
	// Matched records the attribute values that made this resource match, so a
	// report can show why rather than asserting it.
	Matched map[string]any
}

// Check matches the catalogue against the estate at now.
//
// A catalogue entry with no matching resources produces no finding: tf-snag
// reports what is in this estate, not everything Azure has ever retired.
func Check(c *Catalogue, resources []plan.StateResource, now time.Time) []Finding {
	var out []Finding
	for _, e := range c.Entries {
		var instances []Instance
		for _, r := range resources {
			if r.Type != e.Match.Type {
				continue
			}
			matched, ok := matchAttributes(e.Match, r.Values)
			if !ok {
				continue
			}
			instances = append(instances, Instance{Address: r.Address, Type: r.Type, Matched: matched})
		}
		if len(instances) == 0 {
			continue
		}
		sort.Slice(instances, func(i, j int) bool { return instances[i].Address < instances[j].Address })
		days := daysUntil(e.RetiresOn, now)
		out = append(out, Finding{Entry: e, Instances: instances, Urgency: urgency(days), Days: days})
	}
	// Soonest first, so the top of a report is what needs doing first; ties
	// break on id to keep output stable between runs.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Days != out[j].Days {
			return out[i].Days < out[j].Days
		}
		return out[i].Entry.ID < out[j].Entry.ID
	})
	return out
}

// matchAttributes reports whether a resource satisfies every attribute test,
// and which values matched. An entry with no attributes matches the type
// outright - a whole resource type being retired.
func matchAttributes(m Match, values map[string]any) (map[string]any, bool) {
	matched := make(map[string]any, len(m.Attributes))
	for path, predicate := range m.Attributes {
		candidates := lookup(values, path)
		if !predicate.test(candidates) {
			return nil, false
		}
		matched[path] = candidates[0]
	}
	return matched, true
}

func daysUntil(d Date, now time.Time) int {
	if d.IsZero() {
		return 0
	}
	// Whole days between calendar dates, so a run at 23:00 and one at 01:00 the
	// same day report the same number.
	day := now.UTC().Truncate(24 * time.Hour)
	return int(d.Time.UTC().Sub(day).Hours() / 24)
}

func urgency(days int) Urgency {
	switch {
	case days < 0:
		return Retired
	case days <= imminentDays:
		return Imminent
	case days <= approachingDays:
		return Approaching
	default:
		return Scheduled
	}
}

// Summary is a one-line description of a finding, e.g.
// "azurerm_public_ip: Basic SKU public IP addresses retire 2025-09-30 (retired 351 days ago) - 3 instance(s)".
func (f Finding) Summary() string {
	return fmt.Sprintf("%s: %s %s (%s) - %d instance(s)",
		f.Entry.Match.Type, f.Entry.Title, f.Entry.RetiresOn, f.When(), len(f.Instances))
}

// When phrases the deadline in relation to the run.
func (f Finding) When() string {
	switch {
	case f.Days < 0:
		return fmt.Sprintf("retired %d day(s) ago", -f.Days)
	case f.Days == 0:
		return "retires today"
	default:
		return fmt.Sprintf("in %d day(s)", f.Days)
	}
}
