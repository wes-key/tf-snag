// Package wiki renders the exceptions register — every ignore rule in force,
// what each one is currently suppressing, and which ones have stopped
// suppressing anything — and publishes it to an Azure DevOps wiki page.
//
// It lives outside internal/report because ignore already imports report;
// rendering needs both, so it has to sit above them.
package wiki

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wes-key/tf-snag/internal/ignore"
	"github.com/wes-key/tf-snag/internal/report"
)

// The page's entire icon vocabulary. Deliberately small, and each one earns its
// place by carrying a judgement the reader would otherwise have to make: this
// needs attention, this has been waived a long time, nobody said why. Decoration
// that means nothing is noise, and a wiki full of it stops being read.
//
// Emoji rather than colour because an Azure DevOps wiki gives no way to tint a
// table cell — these are the only coloured glyphs available in markdown it will
// render consistently.
const (
	iconLive         = "🔇"  // in effect: actively silencing a finding
	iconStale        = "🧹"  // suppressing nothing: a waiver to sweep up
	iconUnexplained  = "⚠️" // no reason given
	iconLongStanding = "⏳"  // waived for longer than longStanding
)

// longStanding is when a waiver stops being a decision and starts being
// furniture. Nothing enforces it; the page just says so.
const longStanding = 90 * 24 * time.Hour

// Options carries the run's identity, so a reader can tell how current the page
// is and which pipeline wrote it.
type Options struct {
	Context string // e.g. "tf-snag · main · run 20260913.4"
	RunURL  string
	Now     time.Time // zero means time.Now
}

// Render builds the page. Deterministic: the same exceptions and findings
// produce byte-identical markdown, which is what lets the caller skip the write
// when nothing has changed rather than adding a wiki revision every run.
func Render(exs []ignore.Exception, rep *report.Report, o Options) string {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	firstSeen := firstSeenIndex(rep)

	var live, stale []ignore.Exception
	for _, e := range exs {
		if e.Stale() {
			stale = append(stale, e)
		} else {
			live = append(live, e)
		}
	}

	var b strings.Builder
	b.WriteString("# tf-snag exceptions\n\n")
	// A blockquote, so the framing reads as a callout rather than another
	// paragraph of body text — the nearest thing an Azure DevOps wiki has to an
	// admonition.
	b.WriteString("> " + summary(len(exs), len(live), len(stale)) + "\n>\n")
	b.WriteString("> Everything here is still detected and still reported — taken off the exit-code\n")
	b.WriteString("> gate, not hidden. This page is generated: edit the rules, not the page.\n\n")

	if len(exs) == 0 {
		b.WriteString("_No ignore rules are defined._\n")
	} else {
		writeLive(&b, live, firstSeen, now)
		writeStale(&b, stale)
	}

	b.WriteString("\n---\n\n")
	b.WriteString(footer(o, now))
	return b.String()
}

func summary(total, live, stale int) string {
	s := fmt.Sprintf("**%s** · %d in effect", plural(total, "exception"), live)
	if stale > 0 {
		s += fmt.Sprintf(" · %s **%d suppressing nothing**", iconStale, stale)
	}
	return s
}

func writeLive(b *strings.Builder, live []ignore.Exception, firstSeen map[string]string, now time.Time) {
	b.WriteString("## " + iconLive + " In effect\n\n")
	if len(live) == 0 {
		b.WriteString("_None of the rules matched a finding in this run._\n\n")
		return
	}
	b.WriteString("| Exception | Scope | Reason | Defined in | Suppressing | Since |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, e := range live {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n",
			code(subject(e)), scopes(e), reason(e.Reason), definedIn(e),
			suppressing(e), since(e, firstSeen, now))
	}
	b.WriteString("\n")
}

func writeStale(b *strings.Builder, stale []ignore.Exception) {
	if len(stale) == 0 {
		return
	}
	b.WriteString("## " + iconStale + " Suppressing nothing\n\n")
	b.WriteString("These rules matched no finding in this run. The drift they were written for may\nhave been fixed — each is a waiver nobody needs and nobody will think to remove.\n\n")
	b.WriteString("| Exception | Scope | Reason | Defined in |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, e := range stale {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			code(subject(e)), scopes(e), reason(e.Reason), definedIn(e))
	}
	b.WriteString("\n")
}

func scopes(e ignore.Exception) string {
	return strings.Join(e.Scopes, " · ")
}

// subject is what the rule is written against: a resource address for drift, or
// the text a deprecation rule matches on.
func subject(e ignore.Exception) string {
	if e.Addr != "" {
		return e.Addr
	}
	if e.Match != "" {
		return "matching " + e.Match
	}
	return "(unnamed)"
}

// definedIn distinguishes the two authoring routes, because they are governed
// differently: an inline comment is reviewed with the Terraform it sits in, a
// file rule is reviewed on its own.
func definedIn(e ignore.Exception) string {
	if e.InSrc {
		return code(e.Src) + " (inline)"
	}
	return code(e.Src)
}

func suppressing(e ignore.Exception) string {
	var drift, depr int
	for _, h := range e.Hits {
		if h.Kind == "deprecation" {
			depr++
		} else {
			drift++
		}
	}
	var parts []string
	if drift > 0 {
		parts = append(parts, plural(drift, "drift finding"))
	}
	if depr > 0 {
		parts = append(parts, plural(depr, "deprecation"))
	}
	return strings.Join(parts, ", ")
}

// since is the oldest first-detection among the findings this rule covers: how
// long the estate has actually been carrying the thing being waived.
func since(e ignore.Exception, firstSeen map[string]string, now time.Time) string {
	oldest := ""
	for _, h := range e.Hits {
		ts, ok := firstSeen[h.Address]
		if !ok {
			continue
		}
		if oldest == "" || ts < oldest {
			oldest = ts
		}
	}
	if oldest == "" {
		return "—" // no -baseline, so provenance is unknown rather than absent
	}
	t, err := time.Parse(time.RFC3339, oldest)
	if err != nil {
		return "—"
	}
	age := now.Sub(t)
	days := int(age.Hours() / 24)
	var when string
	switch {
	case days <= 0:
		when = "today"
	case days == 1:
		when = "1 day"
	default:
		when = fmt.Sprintf("%d days", days)
	}
	// Bold and flagged once it has been waived long enough to be worth a second
	// look. The date alone does not prompt anyone; "⏳ 412 days" does.
	if age >= longStanding {
		return fmt.Sprintf("%s · %s **%s**", t.Format("2006-01-02"), iconLongStanding, when)
	}
	return fmt.Sprintf("%s · %s", t.Format("2006-01-02"), when)
}

// firstSeenIndex maps what a Hit records to the finding's first-detection time.
// Drift hits carry the address; deprecation hits carry the summary.
func firstSeenIndex(rep *report.Report) map[string]string {
	out := map[string]string{}
	if rep == nil {
		return out
	}
	for _, d := range rep.Drift {
		if d.FirstSeen != "" {
			out[d.Address] = d.FirstSeen
		}
	}
	for _, d := range rep.Deprecations {
		if d.FirstSeen != "" && out[d.Summary] == "" {
			out[d.Summary] = d.FirstSeen
		}
	}
	return out
}

func footer(o Options, now time.Time) string {
	bits := []string{"Generated by [tf-snag](https://github.com/wes-key/tf-snag) at " +
		now.UTC().Format("2006-01-02 15:04 MST")}
	if o.Context != "" {
		bits = append(bits, escape(o.Context))
	}
	if o.RunURL != "" {
		bits = append(bits, "[view run]("+o.RunURL+")")
	}
	return strings.Join(bits, " · ") + "\n"
}

func reason(s string) string {
	if strings.TrimSpace(s) == "" {
		// A waiver with no reason is the one most worth chasing, so say so
		// rather than leaving an empty cell that reads as a rendering bug.
		return iconUnexplained + " _no reason given_"
	}
	return escape(s)
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

// escape neutralises the two characters that would otherwise break a table row
// or start unintended markup.
func escape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// SortExceptions orders the register for reading: by where the rule is defined,
// then by what it targets. Authoring order is an accident of file walking and
// would reshuffle the page for no reason.
func SortExceptions(exs []ignore.Exception) {
	sort.SliceStable(exs, func(i, j int) bool {
		if exs[i].Src != exs[j].Src {
			return exs[i].Src < exs[j].Src
		}
		return subject(exs[i]) < subject(exs[j])
	})
}
