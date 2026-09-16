// Package retire reports resources that Azure (or another provider) has
// announced it is retiring, by matching a catalogue of retirement notices
// against the estate in a Terraform plan.
//
// The catalogue ships inside the binary: tf-snag makes no network call to
// check a retirement, so using it needs no credentials, no outbound access and
// no cloud permissions - the same bargain as every other check. The cost of
// that is a catalogue that ages, so every report says how old it is, and
// -retirements <file> lets a team supply its own.
package retire

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DateLayout is the date format used throughout the catalogue: a plain
// calendar date, since retirements are announced as days, not timestamps.
const DateLayout = "2006-01-02"

// The catalogue lives beside this file because go:embed cannot reach outside
// the package directory.
//
//go:embed catalogue.yaml
var embedded []byte

// Catalogue is a set of retirement notices plus the date it was assembled.
type Catalogue struct {
	// Updated is when the catalogue was last reviewed, which is what a report
	// shows to say how much to trust a clean result.
	Updated Date    `yaml:"updated"`
	Entries []Entry `yaml:"retirements"`

	// Source is where the catalogue came from: "built-in", or a file path.
	Source string `yaml:"-"`
}

// Entry is one retirement notice.
type Entry struct {
	// ID is stable and citable: it keys ignore rules, work items and baselines,
	// so it must not change once published.
	ID          string `yaml:"id"`
	Title       string `yaml:"title"`
	RetiresOn   Date   `yaml:"retires_on"`
	Announced   Date   `yaml:"announced"`
	URL         string `yaml:"url"`
	Severity    string `yaml:"severity"`
	Remediation string `yaml:"remediation"`
	// ProviderMinVersion notes the provider major version the attribute names
	// were written against; azurerm renames attributes between majors, so a
	// match written for v4 may quietly stop matching later.
	ProviderMinVersion string `yaml:"provider_min_version"`
	Match              Match  `yaml:"match"`
}

// Match selects the resources an entry applies to. An entry with no attributes
// matches every resource of its type, which is what a whole-resource-type
// retirement looks like (azurerm_mysql_server, say).
type Match struct {
	Type       string               `yaml:"type"`
	Attributes map[string]Predicate `yaml:"attributes"`
}

// Load reads a catalogue from path, or the built-in one when path is empty.
func Load(path string) (*Catalogue, error) {
	raw, source := embedded, "built-in"
	if path != "" {
		var err error
		if raw, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("reading retirement catalogue: %w", err)
		}
		source = path
	}
	c, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing retirement catalogue (%s): %w", source, err)
	}
	c.Source = source
	return c, nil
}

// Age is how long ago the catalogue was updated, as at now. A caller warns on
// an old catalogue: every other check is derived from the plan and cannot go
// stale, but this one can, and a clean report from a stale catalogue is a
// claim tf-snag has not actually checked.
func (c *Catalogue) Age(now time.Time) time.Duration {
	if c.Updated.IsZero() {
		return 0
	}
	return now.Sub(c.Updated.Time)
}

// validate rejects a catalogue that would mislead rather than fail loudly: a
// duplicate id silently shadows an entry, an entry with no type matches
// nothing, and a missing date makes every urgency wrong.
func (c *Catalogue) validate() error {
	if len(c.Entries) == 0 {
		return fmt.Errorf("catalogue has no retirements")
	}
	seen := make(map[string]bool, len(c.Entries))
	var problems []string
	for i, e := range c.Entries {
		where := e.ID
		if where == "" {
			where = fmt.Sprintf("entry %d", i+1)
			problems = append(problems, where+": no id")
		}
		if seen[e.ID] && e.ID != "" {
			problems = append(problems, where+": duplicate id")
		}
		seen[e.ID] = true
		if e.Title == "" {
			problems = append(problems, where+": no title")
		}
		if e.RetiresOn.IsZero() {
			problems = append(problems, where+": no retires_on date")
		}
		if e.URL == "" {
			problems = append(problems, where+": no url - a finding nobody can verify is not actionable")
		}
		if e.Match.Type == "" {
			problems = append(problems, where+": match has no type")
		}
		switch e.Severity {
		case "high", "medium", "low", "":
		default:
			problems = append(problems, where+": severity "+e.Severity+" is not high, medium or low")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// parse decodes a catalogue, refusing unknown fields: a misspelled key
// (`retired_on`, `attribute`) would otherwise be dropped in silence and the
// entry would match nothing, or everything.
func parse(raw []byte) (*Catalogue, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Catalogue
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
