// Package ignore suppresses known-acceptable drift and deprecation findings,
// from a YAML file and from inline `# tf-snag:ignore` comments in .tf sources.
// Suppressed findings stay in the report (SARIF `suppressions`, an "ignored"
// section in text/markdown) but no longer trip the -exit-code gate.
package ignore

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wes-key/tf-snag/internal/report"
	"gopkg.in/yaml.v3"
)

// Rule is one ignore entry. A file rule sets Addr (drift, glob) or Match
// (deprecation, lower-cased substring of summary+detail). An inline rule sets
// InSrc and reuses Addr as the enclosing resource's "type.name" key, plus
// file/line for deprecation proximity matching.
type Rule struct {
	Addr   string
	Match  string
	Reason string
	Src    string // ".tf-snag-ignore.yml" or "<tf-file>:<line>"
	InSrc  bool

	// id identifies one authored rule across both slices. A bare
	// `# tf-snag:ignore` is filed under drift AND deprecations, and the
	// exceptions register has to report that as one exception with two scopes
	// rather than two rules that happen to look alike.
	id   int
	file string
	line int
}

// Set is the merged collection of ignore rules.
type Set struct {
	drift  []Rule
	depr   []Rule
	retire []Rule

	next int
	// hits records what each rule actually suppressed, by rule id. Populated by
	// Apply, read by Exceptions. A rule with no entry here matched nothing this
	// run, which is the interesting case: the drift it was written for may be
	// fixed and the rule can probably go.
	hits map[int][]Hit
}

// Hit is one finding a rule suppressed.
type Hit struct {
	Kind    string // "drift" or "deprecation"
	Address string // resource address, or the deprecation's summary
}

// Empty reports whether the set has no rules.
func (s *Set) Empty() bool {
	return s == nil || (len(s.drift) == 0 && len(s.depr) == 0 && len(s.retire) == 0)
}

// Merge folds o's rules into s, renumbering so ids stay unique across the merge
// - the file set and the source set are built independently and both start at 0.
func (s *Set) Merge(o *Set) {
	if o == nil {
		return
	}
	base := s.next
	shift := func(rules []Rule) []Rule {
		out := make([]Rule, len(rules))
		for i, r := range rules {
			r.id += base
			out[i] = r
		}
		return out
	}
	s.drift = append(s.drift, shift(o.drift)...)
	s.depr = append(s.depr, shift(o.depr)...)
	s.retire = append(s.retire, shift(o.retire)...)
	s.next = base + o.next
}

// nextID hands out the identity for one authored rule.
func (s *Set) nextID() int {
	id := s.next
	s.next++
	return id
}

// --- YAML file ----------------------------------------------------------------

type fileSchema struct {
	Drift []struct {
		Address string `yaml:"address"`
		Reason  string `yaml:"reason"`
	} `yaml:"drift"`
	Deprecations []struct {
		Match  string `yaml:"match"`
		Reason string `yaml:"reason"`
	} `yaml:"deprecations"`
	// A retirement rule names either the catalogue entry (id) or one affected
	// resource (address) — see retirement.go.
	Retirements []struct {
		ID      string `yaml:"id"`
		Address string `yaml:"address"`
		Reason  string `yaml:"reason"`
	} `yaml:"retirements"`
}

// LoadFile parses a tf-snag ignore YAML. An empty path returns an empty set
// with no error (so an absent auto-discovered file is a no-op).
func LoadFile(path string) (*Set, error) {
	if path == "" {
		return &Set{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fsc fileSchema
	if err := yaml.Unmarshal(raw, &fsc); err != nil {
		return nil, fmt.Errorf("parsing ignore file %s: %w", path, err)
	}
	src := filepath.Base(path)
	s := &Set{}
	for _, d := range fsc.Drift {
		if d.Address == "" {
			continue
		}
		s.drift = append(s.drift, Rule{Addr: d.Address, Reason: d.Reason, Src: src, id: s.nextID()})
	}
	for _, d := range fsc.Deprecations {
		if d.Match == "" {
			continue
		}
		s.depr = append(s.depr, Rule{Match: strings.ToLower(d.Match), Reason: d.Reason, Src: src, id: s.nextID()})
	}
	for _, d := range fsc.Retirements {
		// Match carries the catalogue id and Addr the resource address, so one
		// rule shape serves both and Exceptions reports them alike.
		if d.ID == "" && d.Address == "" {
			continue
		}
		s.retire = append(s.retire, Rule{
			Match: d.ID, Addr: d.Address, Reason: d.Reason, Src: src, id: s.nextID(),
		})
	}
	return s, nil
}

// --- inline .tf comments ----------------------------------------------------

var (
	reResourceDecl = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"`)
	reIgnore       = regexp.MustCompile(`#\s*tf-snag:ignore(-drift|-deprecations?)?\b`)
	reReason       = regexp.MustCompile(`(?i)reason\s*[:=]\s*(.+?)\s*$`)
	reIndex        = regexp.MustCompile(`\[[^\]]*\]`)
)

// FromSource walks root for *.tf files and builds rules from
// `# tf-snag:ignore[-drift|-deprecation]` comments — inside a resource block or
// on the line(s) directly above a `resource` declaration.
func FromSource(root string) (*Set, error) {
	s := &Set{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".terraform", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".tf") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = p
		}
		scanFile(f, filepath.ToSlash(rel), s)
		return nil
	})
	return s, err
}

type directive struct {
	scope  string // "", "drift", "deprecation"
	reason string
	line   int
}

func scanFile(r io.Reader, file string, s *Set) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	depth := 0
	curRes := ""
	var pending []directive // directives at depth 0 awaiting the next `resource`

	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()

		if m := reResourceDecl.FindStringSubmatch(line); m != nil && depth == 0 {
			curRes = m[1] + "." + m[2]
			for _, d := range pending {
				add(s, curRes, file, d)
			}
			pending = nil
		}

		if m := reIgnore.FindStringSubmatch(line); m != nil {
			d := directive{scope: normScope(m[1]), line: ln}
			if rm := reReason.FindStringSubmatch(line); rm != nil {
				d.reason = strings.Trim(rm[1], `"' `)
			}
			if depth > 0 && curRes != "" {
				add(s, curRes, file, d)
			} else {
				pending = append(pending, d)
			}
		}

		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			depth = 0
			curRes = ""
		}
	}
}

func normScope(s string) string {
	switch s {
	case "-drift":
		return "drift"
	case "-deprecation", "-deprecations":
		return "deprecation"
	default:
		return ""
	}
}

func add(s *Set, resKey, file string, d directive) {
	r := Rule{
		Addr:   resKey,
		Reason: d.reason,
		Src:    fmt.Sprintf("%s:%d", file, d.line),
		InSrc:  true,
		id:     s.nextID(),
		file:   file,
		line:   d.line,
	}
	switch d.scope {
	case "drift":
		s.drift = append(s.drift, r)
	case "deprecation":
		s.depr = append(s.depr, r)
	default:
		s.drift = append(s.drift, r)
		s.depr = append(s.depr, r)
	}
}

// --- application ------------------------------------------------------------

// Apply stamps the suppression fields on every drift / deprecation the set
// matches. First matching rule wins.
func (s *Set) Apply(r *report.Report) {
	if s.Empty() {
		return
	}
	for i := range r.Drift {
		if rule, ok := s.matchDrift(r.Drift[i].Address); ok {
			mark(&r.Drift[i].Suppressed, &r.Drift[i].SuppressReason, &r.Drift[i].SuppressSrc, &r.Drift[i].SuppressKind, rule)
			s.record(rule, Hit{Kind: "drift", Address: r.Drift[i].Address})
		}
	}
	for i := range r.Deprecations {
		if rule, ok := s.matchDepr(&r.Deprecations[i]); ok {
			mark(&r.Deprecations[i].Suppressed, &r.Deprecations[i].SuppressReason, &r.Deprecations[i].SuppressSrc, &r.Deprecations[i].SuppressKind, rule)
			s.record(rule, Hit{Kind: "deprecation", Address: r.Deprecations[i].Summary})
		}
	}
	s.applyRetirements(r)
}

func (s *Set) record(r Rule, h Hit) {
	if s.hits == nil {
		s.hits = map[int][]Hit{}
	}
	s.hits[r.id] = append(s.hits[r.id], h)
}

// Exception is one authored rule plus what it suppressed in the run Apply was
// given. Scopes says which checks it covers; a bare `# tf-snag:ignore` covers
// both, and is reported once rather than twice.
type Exception struct {
	Rule
	Scopes []string // "drift" and/or "deprecation", in that order
	Hits   []Hit
}

// Stale reports whether the rule suppressed nothing. Worth surfacing: the drift
// it was written for may have been fixed, leaving a waiver nobody needs and
// nobody will think to remove.
func (e Exception) Stale() bool { return len(e.Hits) == 0 }

// Exceptions returns every rule in the set, in the order they were authored,
// with the findings each suppressed. Call it after Apply - before that, every
// exception looks stale.
func (s *Set) Exceptions() []Exception {
	if s == nil {
		return nil
	}
	byID := map[int]*Exception{}
	var order []int
	collect := func(rules []Rule, scope string) {
		for _, r := range rules {
			e, seen := byID[r.id]
			if !seen {
				e = &Exception{Rule: r}
				byID[r.id] = e
				order = append(order, r.id)
			}
			e.Scopes = append(e.Scopes, scope)
		}
	}
	collect(s.drift, "drift")
	collect(s.depr, "deprecation")
	collect(s.retire, "retirement")

	sort.Ints(order)
	out := make([]Exception, 0, len(order))
	for _, id := range order {
		e := byID[id]
		e.Hits = s.hits[id]
		out = append(out, *e)
	}
	return out
}

func mark(sup *bool, reason, src, kind *string, r Rule) {
	*sup = true
	*reason = r.Reason
	*src = r.Src
	if r.InSrc {
		*kind = "inSource"
	} else {
		*kind = "external"
	}
}

func (s *Set) matchDrift(addr string) (Rule, bool) {
	tn := typeName(addr)
	for _, r := range s.drift {
		if r.InSrc {
			if r.Addr == tn {
				return r, true
			}
			continue
		}
		if globMatch(r.Addr, addr) || globMatch(r.Addr, tn) {
			return r, true
		}
	}
	return Rule{}, false
}

func (s *Set) matchDepr(d *report.Deprecation) (Rule, bool) {
	hay := strings.ToLower(d.Summary + " " + d.Detail)
	for _, r := range s.depr {
		if r.InSrc {
			for _, site := range d.Sites {
				if r.Addr != "" && r.Addr == typeName(site.Address) {
					return r, true
				}
				if r.file != "" && site.File == r.file && abs(site.Line-r.line) <= 2 {
					return r, true
				}
			}
			continue
		}
		if r.Match != "" && strings.Contains(hay, r.Match) {
			return r, true
		}
	}
	return Rule{}, false
}

// typeName reduces a resource address to its trailing "type.name", dropping any
// module prefix and [index] segments — the loose key -source linking already
// uses.
func typeName(addr string) string {
	addr = reIndex.ReplaceAllString(addr, "")
	parts := strings.Split(addr, ".")
	if len(parts) < 2 {
		return addr
	}
	return parts[len(parts)-2] + "." + parts[len(parts)-1]
}

// globMatch matches s against pat where `*` is any run of characters (anchored
// both ends). path.Match is unusable here because `[idx]` is a char class.
func globMatch(pat, s string) bool {
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
