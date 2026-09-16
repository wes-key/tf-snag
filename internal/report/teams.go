package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wes-key/tf-snag/internal/retire"
)

// Microsoft Teams notification, as an Adaptive Card wrapped in the envelope a
// Power Automate "Workflows" HTTP trigger expects ({type:"message",
// attachments:[…]}). The retired Office 365 connector / MessageCard shape is
// deliberately not supported — Microsoft has switched those off.
//
// Teams renders only a subset of Markdown inside a TextBlock: **bold**,
// _italic_, [links](url) and lists. Backticks are NOT code-formatted, they show
// literally, so resource addresses are bolded rather than quoted.
//
// Two limits worth knowing before reaching for a nicer layout:
//   - Images must be PNG, JPG or GIF. An inline data:image/svg+xml URI renders
//     as a broken-image icon, so badges cannot be drawn as SVG (verified).
//   - There is no way to put white text on a solid colour: TextRun.highlight
//     picks its background from the text colour, and no text element takes a
//     background, border or corner radius at the versions Teams supports.

// teamsCardVersion is the Adaptive Card schema version. 1.4 is the highest that
// every current Teams surface (desktop, web, mobile) renders reliably.
const teamsCardVersion = "1.4"

// teamsMaxItems is the default per-section cap before a "+N more" line. Teams
// silently truncates very large cards, so this keeps the payload well inside
// the ~28 KB the webhook accepts.
const teamsMaxItems = 5

// TeamsOptions tunes the card WriteTeams builds. Every field is optional.
type TeamsOptions struct {
	// Context is a small subtle line under the headline — typically the
	// pipeline, branch and run number.
	Context string
	// RunURL, when set, adds a "View run" button to the card.
	RunURL string
	// MaxItems caps how many findings each section lists before a "+N more"
	// line. Zero means teamsMaxItems.
	MaxItems int
}

// WriteTeams renders the report as a Teams Adaptive Card payload, ready to POST
// to a Power Automate Workflows webhook. Suppressed findings are summarised as a
// count only — they are, by definition, the ones nobody needs paging about.
func (r *Report) WriteTeams(w io.Writer, opts TeamsOptions) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r.teamsPayload(opts))
}

func (r *Report) teamsPayload(opts TeamsOptions) teamsMessage {
	drift, ignoredDrift := r.gatingDrift()
	depr, ignoredDepr := r.gatingDeprecations()
	max := opts.MaxItems
	if max <= 0 {
		max = teamsMaxItems
	}

	// Banner: a tinted, full-bleed box carrying the verdict. The tint is the
	// signal, so the text inside stays default-coloured — an Attention-coloured
	// TextBlock on an attention-styled container is unreadable.
	banner := []acElement{{
		Type: "TextBlock", Text: teamsHeadline(len(drift), len(depr)),
		Size: "Large", Weight: "Bolder", Wrap: true,
	}}
	if opts.Context != "" {
		banner = append(banner, acElement{
			Type: "TextBlock", Text: opts.Context,
			IsSubtle: true, Wrap: true, Spacing: "None",
		})
	}
	body := []acElement{{
		Type:  "Container",
		Style: teamsStyle(len(drift), len(depr)),
		Bleed: true,
		Items: banner,
	}}

	body = append(body, acElement{
		Type:    "FactSet",
		Spacing: "Medium",
		Facts:   r.teamsFacts(drift, depr, len(ignoredDrift)+len(ignoredDepr)),
	})

	// Each kind gets a collapsible group. Collapsed by default: the counts above
	// are the at-a-glance view, and a channel post should not be a wall of text.
	// New findings lead, so with the list capped it is the things that appeared
	// since the last run that survive the cut.
	if len(drift) > 0 {
		drift = teamsByRank(drift, func(rr ResourceReport) int {
			return findingRank(rr.Unsuppressed, rr.BaselineState)
		})
		body = append(body, teamsGroup("drift", "Changed outside Terraform", len(drift),
			teamsDriftItems(drift, max))...)
	}
	if len(depr) > 0 {
		depr = teamsByRank(depr, func(d Deprecation) int {
			return findingRank(d.Unsuppressed, d.BaselineState)
		})
		body = append(body, teamsGroup("depr", "Deprecations", len(depr),
			teamsDeprItems(depr, max))...)
	}
	// Retirements arrive soonest-first from the matcher, which is the order that
	// matters here: a card is skimmed, and the deadline is the news.
	if rets := r.reportedRetirements(); len(rets) > 0 {
		body = append(body, teamsGroup("retire", "Retirements", len(rets),
			teamsRetirementItems(rets, max))...)
	}

	// Suppressed findings get their own collapsed groups, as in the run tab:
	// off the gate and out of the counts, but auditable without leaving Teams.
	if n := len(ignoredDrift); n > 0 {
		body = append(body, teamsGroup("ignDrift", "Ignored drift", n,
			teamsIgnoredItems(ignoredDrift, max))...)
	}
	if n := len(ignoredDepr); n > 0 {
		body = append(body, teamsGroup("ignDepr", "Ignored deprecations", n,
			teamsIgnoredDeprItems(ignoredDepr, max))...)
	}

	card := adaptiveCard{
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Type:    "AdaptiveCard",
		Version: teamsCardVersion,
		Body:    body,
		MSTeams: &acMSTeams{Width: "Full"},
	}
	if opts.RunURL != "" {
		card.Actions = []acAction{{Type: "Action.OpenUrl", Title: "View run", URL: opts.RunURL}}
	}

	return teamsMessage{
		Type: "message",
		Attachments: []teamsAttachment{{
			ContentType: "application/vnd.microsoft.card.adaptive",
			Content:     card,
		}},
	}
}

// teamsHeadline is the card's one-line verdict.
func teamsHeadline(nDrift, nDepr int) string {
	switch {
	case nDrift == 0 && nDepr == 0:
		return "No drift or deprecations"
	case nDepr == 0:
		return fmt.Sprintf("%d resource%s changed outside Terraform", nDrift, plural(nDrift))
	case nDrift == 0:
		return fmt.Sprintf("%d deprecation warning%s", nDepr, plural(nDepr))
	}
	return fmt.Sprintf("%d resource%s changed outside Terraform, %d deprecation%s",
		nDrift, plural(nDrift), nDepr, plural(nDepr))
}

// teamsStyle tints the banner container: drift is the gating failure
// (attention), deprecations alone are a warning, nothing is good.
func teamsStyle(nDrift, nDepr int) string {
	switch {
	case nDrift > 0:
		return "attention"
	case nDepr > 0:
		return "warning"
	}
	return "good"
}

// teamsActionColor picks the Adaptive Card text colour for a plan action, so the
// sign glyph carries the same meaning it does in the console report.
func teamsActionColor(action string) string {
	switch action {
	case "create":
		return "Good"
	case "delete", "replace":
		return "Attention"
	default:
		return "Warning"
	}
}

// teamsGroup renders one collapsible section: an emphasis-tinted header row that
// toggles the body, and the body itself, hidden to start. Action.ToggleVisibility
// flips all three targets at once, so the chevron swaps with the body.
func teamsGroup(id, title string, count int, items []acElement) []acElement {
	bodyID, openID, shutID := id+"Body", id+"Open", id+"Shut"
	toggle := &acAction{
		Type:           "Action.ToggleVisibility",
		Title:          title,
		TargetElements: []string{bodyID, openID, shutID},
	}

	header := acElement{
		Type:         "Container",
		Style:        "emphasis",
		Spacing:      "Medium",
		SelectAction: toggle,
		Items: []acElement{{
			Type: "ColumnSet",
			Columns: []acColumn{
				{Type: "Column", Width: "stretch", Items: []acElement{{
					Type:   "TextBlock",
					Text:   fmt.Sprintf("%s  (%d)", title, count),
					Weight: "Bolder",
					Wrap:   true,
				}}},
				{Type: "Column", Width: "auto", Items: []acElement{
					{Type: "TextBlock", ID: shutID, Text: "▸", Weight: "Bolder", IsVisible: boolPtr(true)},
					{Type: "TextBlock", ID: openID, Text: "▾", Weight: "Bolder", IsVisible: boolPtr(false)},
				}},
			},
		}},
	}

	return []acElement{header, {
		Type:      "Container",
		ID:        bodyID,
		IsVisible: boolPtr(false),
		Spacing:   "None",
		Items:     items,
	}}
}

// teamsRow is one finding, laid out to echo the tf-snag run tab: a sign column,
// the resource and its detail, then a coloured category badge and how long the
// finding has been around.
type teamsRow struct {
	Glyph  string     // plan sign or severity glyph
	Colour string     // Adaptive Card colour for the glyph and the badge
	Title  string     // bold primary line (Markdown)
	Where  string     // module, or the .tf that declares it
	Detail []acInline // attribute changes / deprecation detail, as coloured runs
	Badge  string     // "Update", "Warning", ... the tab's Change / Severity column
	New    bool       // absent from the baseline — gets a chip, like the tab's pill
	// Unsuppressed marks a finding whose ignore rule was removed: newly
	// actionable rather than newly detected, so it keeps its real age.
	Unsuppressed bool
	Age          string // "first seen ..." for anything carried over
	Note         string // ignored rows: the rule that matched
	Sep          bool
}

func teamsFinding(r teamsRow) acElement {
	sub := func(text, size string) acElement {
		return acElement{Type: "TextBlock", Text: text, Wrap: true, IsSubtle: true, Size: size, Spacing: "None"}
	}

	main := []acElement{{Type: "TextBlock", Text: r.Title, Wrap: true}}
	if r.Where != "" {
		main = append(main, sub(r.Where, "Small"))
	}
	if len(r.Detail) > 0 {
		// RichTextBlock rather than TextBlock so the before/after values can be
		// coloured individually; it wraps by default.
		main = append(main, acElement{Type: "RichTextBlock", Spacing: "None", Inlines: r.Detail})
	}

	// Right-hand column: the category chip over the provenance, mirroring the
	// tab's Change and First seen columns.
	right := []acElement{}
	if r.Badge != "" {
		right = append(right, teamsChip(r.Badge, r.Colour, "Right"))
	}
	age := acElement{
		Type: "TextBlock", Text: r.Age, IsSubtle: true,
		Size: "Small", HorizontalAlignment: "Right", Spacing: "None", Wrap: true,
	}
	switch {
	case r.Unsuppressed:
		// Its own chip, and it keeps the age: the finding is not new, its
		// visibility is. Calling it "New" would misreport how long the drift
		// has been there.
		right = append(right, teamsChip("No longer ignored", "Accent", "Right"))
		if r.Age != "" {
			right = append(right, age)
		}
	case r.New:
		right = append(right, teamsChip("New", "Accent", "Right"))
	case r.Age != "":
		right = append(right, age)
	}
	if r.Note != "" {
		right = append(right, acElement{
			Type: "TextBlock", Text: r.Note, IsSubtle: true,
			Size: "Small", HorizontalAlignment: "Right", Spacing: "None", Wrap: true,
		})
	}

	cols := []acColumn{
		{Type: "Column", Width: "auto", Items: []acElement{{
			Type: "TextBlock", Text: r.Glyph, Color: r.Colour, Weight: "Bolder",
		}}},
		{Type: "Column", Width: "stretch", Items: main},
	}
	if len(right) > 0 {
		cols = append(cols, acColumn{Type: "Column", Width: "auto", Items: right})
	}

	return acElement{Type: "ColumnSet", Spacing: "Small", Separator: r.Sep, Columns: cols}
}

func boolPtr(b bool) *bool { return &b }

func (r *Report) teamsFacts(drift []ResourceReport, depr []Deprecation, ignored int) []acFact {
	add, chg, del := tally(r.Pending)
	facts := []acFact{
		{Title: "Drift", Value: teamsCount(len(drift), driftBreakdown(drift))},
		{Title: "Deprecations", Value: fmt.Sprintf("%d", len(depr))},
	}
	// Only when the check ran: a "Retirements: 0" fact on a card from a pipeline
	// that never asked for them would read as a clean bill of health.
	if r.Catalogue != nil {
		facts = append(facts, acFact{Title: "Retirements", Value: teamsRetirementFact(r.reportedRetirements())})
	}
	// Only meaningful when the run was given a -baseline to diff against.
	// Deliberately not "since the last run": under -teams-notify new the last
	// card may be days old, so a reader cannot tell whether "the last run" means
	// the previous check or the last message they saw. The baseline is always
	// the previous check, so say what that makes each finding.
	if r.IsBaselined() {
		facts = append(facts, acFact{Title: "New", Value: teamsNewFact(teamsCountNew(drift, depr))})
	}
	if ignored > 0 {
		facts = append(facts, acFact{Title: "Ignored", Value: fmt.Sprintf("%d (suppressed by a rule)", ignored)})
	}
	facts = append(facts, acFact{
		Title: "Pending",
		Value: fmt.Sprintf("%d to add, %d to change, %d to destroy", add, chg, del),
	})
	if r.TerraformVersion != "" {
		facts = append(facts, acFact{Title: "Terraform", Value: r.TerraformVersion})
	}
	return facts
}

func teamsCount(n int, detail string) string {
	if detail == "" {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%d (%s)", n, detail)
}

// teamsNewFact phrases the New count against the check, not against the last
// notification — the two are the same thing only when every run posts.
func teamsNewFact(n int) string {
	if n == 0 {
		return "none — everything here pre-dates this check"
	}
	return fmt.Sprintf("%d first detected in this check", n)
}

func teamsCountNew(drift []ResourceReport, depr []Deprecation) int {
	n := 0
	for _, rr := range drift {
		if rr.BaselineState == "new" {
			n++
		}
	}
	for _, d := range depr {
		if d.BaselineState == "new" {
			n++
		}
	}
	return n
}

// findingRank orders what leads the list. Something whose ignore rule was just
// removed comes first: it was deliberately hidden until now, so its reappearance
// is the most notable thing in the post. Then genuinely new findings, then
// everything carried over.
func findingRank(unsuppressed bool, baselineState string) int {
	switch {
	case unsuppressed:
		return 0
	case baselineState == "new":
		return 1
	default:
		return 2
	}
}

// teamsByRank is a stable sort into rank order, so within a rank the report's
// own ordering survives. It matters because the list is capped: whatever leads
// is what a reader actually sees.
func teamsByRank[T any](items []T, rank func(T) int) []T {
	out := make([]T, 0, len(items))
	for r := 0; r <= 2; r++ {
		for _, it := range items {
			if rank(it) == r {
				out = append(out, it)
			}
		}
	}
	return out
}

// teamsAge is the "first seen …" note for a finding carried over from a previous
// run, linked to the run that first surfaced it when that is known. Empty when
// the run had no -baseline.
//
// Only carried-over findings get this: a new one was first seen by the run the
// card's own "View run" button already points at.
func teamsAge(firstSeen, runURL string) string {
	// ageParens is " (first seen …)" — unwrap it for use as a standalone line.
	age := strings.Trim(ageParens(firstSeen), " ()")
	if age == "" || runURL == "" {
		return age
	}
	return "[" + age + "](" + runURL + ")"
}

// chipPad is a non-breaking space. Ordinary spaces at the edges of a TextRun get
// collapsed or trimmed by the renderer, which leaves the highlight hugging the
// letters and reading as smudged text rather than a chip; U+00A0 survives.
const chipPad = "  "

// chipBlock is the solid colour each category leads with. Teams will not render
// a drawn badge (no SVG) and no text element takes a background colour, but a
// Unicode block *is* solid colour and is plain text, so it always renders — on
// desktop, web and mobile alike. It carries the colour; the highlighted label
// beside it carries the word.
var chipBlock = map[string]string{
	"Good":    "🟩", // create
	"Warning": "🟨", // update, deprecation warning — yellow, not orange: an
	//                    orange block sits too close to the red one to tell
	//                    apart at a glance in a channel
	"Attention": "🟥", // delete, replace, error
	"Accent":    "🟦", // new
	"Default":   "⬛", // ignored / unknown
}

// teamsChip renders a category badge: a solid colour block followed by the label
// on a highlighted run. See the limits noted at the top of this file for why it
// is not a drawn pill.
func teamsChip(text, colour, align string) acElement {
	label := acInline{
		Type:  "TextRun",
		Text:  chipPad + text + chipPad,
		Color: colour,
		// Body size, not Small: at Small the chip is barely legible against
		// the resource name it sits beside.
		Weight:    "Bolder",
		Highlight: true,
	}
	inlines := []acInline{label}
	if block, ok := chipBlock[colour]; ok {
		// Separate run: the block must not take the label's highlight, or the
		// colour reads as a smudge rather than a swatch.
		inlines = []acInline{{Type: "TextRun", Text: block + " "}, label}
	}
	return acElement{
		Type:                "RichTextBlock",
		HorizontalAlignment: align,
		Inlines:             inlines,
	}
}

func teamsDriftItems(drift []ResourceReport, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, rr := range drift {
		if i == max {
			out = append(out, teamsMoreItem(len(drift)-max, "resource"))
			break
		}
		out = append(out, teamsFinding(teamsRow{
			Glyph:        sign(rr.Action),
			Colour:       teamsActionColor(rr.Action),
			Title:        "**" + teamsText(rr.Address) + "**",
			Where:        teamsWhere(rr),
			Detail:       teamsAttrDetail(rr),
			Badge:        teamsBadge(rr.Action),
			New:          rr.BaselineState == "new",
			Unsuppressed: rr.Unsuppressed,
			Age:          teamsAge(rr.FirstSeen, rr.FirstRunURL),
			Sep:          i > 0,
		}))
	}
	return out
}

// teamsBadge capitalises a category word for the right-hand column ("update" ->
// "Update"). Plan actions and severities are ASCII, so a byte swap is enough.
func teamsBadge(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// teamsWhere is the tab's Module column: the module path when the resource is in
// one, else the .tf that declares it.
func teamsWhere(rr ResourceReport) string {
	if rr.Module != "" {
		return teamsText(rr.Module)
	}
	if rr.File == "" {
		return ""
	}
	if rr.Line > 0 {
		return teamsText(fmt.Sprintf("%s:%d", rr.File, rr.Line))
	}
	return teamsText(rr.File)
}

// teamsAttrDetail is the sub-line under a drifted resource: the first couple of
// attribute changes, or the one-line summary when there are none to show. The
// before value is red and the after green, as in the run tab's diff — the path
// and punctuation stay subtle so the values are what the eye lands on.
func teamsAttrDetail(rr ResourceReport) []acInline {
	if len(rr.Attrs) == 0 {
		return teamsPlain(rr.summaryLine())
	}
	const shown = 2
	out := make([]acInline, 0, shown*4+1)
	for i, a := range rr.Attrs {
		if i == shown {
			out = append(out, acInline{
				Type: "TextRun", IsSubtle: true,
				Text: fmt.Sprintf(";  +%d more", len(rr.Attrs)-shown),
			})
			break
		}
		if i > 0 {
			out = append(out, acInline{Type: "TextRun", Text: ";  ", IsSubtle: true})
		}
		out = append(out,
			acInline{Type: "TextRun", Text: teamsText(a.Path) + ": ", IsSubtle: true},
			acInline{Type: "TextRun", Text: teamsText(clip(render(a.Old), 60)), Color: "Attention"},
			acInline{Type: "TextRun", Text: " → ", IsSubtle: true},
			acInline{Type: "TextRun", Text: teamsText(clip(render(a.New), 60)), Color: "Good"},
		)
	}
	return out
}

// teamsPlain is an uncoloured detail line — a deprecation's text, or the reason
// an item was ignored.
func teamsPlain(s string) []acInline {
	if s == "" {
		return nil
	}
	return []acInline{{Type: "TextRun", Text: s, IsSubtle: true}}
}

func teamsDeprItems(depr []Deprecation, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, d := range depr {
		if i == max {
			out = append(out, teamsMoreItem(len(depr)-max, "deprecation"))
			break
		}
		sev := strings.ToLower(d.Severity)
		glyph, colour := "⚠", "Warning"
		if sev == "error" {
			glyph, colour = "✖", "Attention"
		}
		var detail string
		if d.Detail != "" {
			detail = teamsText(firstSentence(strings.ReplaceAll(d.Detail, "\n", " "), 200))
		}
		out = append(out, teamsFinding(teamsRow{
			Glyph:        glyph,
			Colour:       colour,
			Title:        "**" + teamsText(d.Summary) + "**",
			Where:        teamsSites(d),
			Detail:       teamsPlain(detail),
			Badge:        teamsBadge(sev),
			New:          d.BaselineState == "new",
			Unsuppressed: d.Unsuppressed,
			Age:          teamsAge(d.FirstSeen, d.FirstRunURL),
			Sep:          i > 0,
		}))
	}
	return out
}

// teamsSites lists the addresses a deprecation fires at, capped so a
// count/for_each block does not fill the card.
func teamsSites(d Deprecation) string {
	const shown = 3
	seen := make([]string, 0, shown)
	for _, s := range d.Sites {
		label := s.Address
		if label == "" {
			label = siteLabel(s)
		}
		if label == "" {
			continue
		}
		if len(seen) == shown {
			seen = append(seen, fmt.Sprintf("+%d more", len(d.Sites)-shown))
			break
		}
		seen = append(seen, teamsText(label))
	}
	return strings.Join(seen, ", ")
}

// teamsIgnoredItems lists suppressed drift with the reason and the rule that
// matched — the same three things the run tab's Ignored table shows.
func teamsIgnoredItems(drift []ResourceReport, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, rr := range drift {
		if i == max {
			out = append(out, teamsMoreItem(len(drift)-max, "resource"))
			break
		}
		out = append(out, teamsFinding(teamsRow{
			Glyph:  "🔕",
			Colour: "Default",
			Title:  "**" + teamsText(rr.Address) + "**",
			Where:  teamsWhere(rr),
			Detail: teamsPlain(teamsText(reasonOr(rr.SuppressReason))),
			Badge:  teamsBadge(rr.Action),
			Note:   teamsText(rr.SuppressSrc),
			Sep:    i > 0,
		}))
	}
	return out
}

func teamsIgnoredDeprItems(depr []Deprecation, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, d := range depr {
		if i == max {
			out = append(out, teamsMoreItem(len(depr)-max, "deprecation"))
			break
		}
		out = append(out, teamsFinding(teamsRow{
			Glyph:  "🔕",
			Colour: "Default",
			Title:  "**" + teamsText(d.Summary) + "**",
			Where:  teamsSites(d),
			Detail: teamsPlain(teamsText(reasonOr(d.SuppressReason))),
			Note:   teamsText(d.SuppressSrc),
			Sep:    i > 0,
		}))
	}
	return out
}

func teamsMoreItem(n int, noun string) acElement {
	return acElement{
		Type:      "TextBlock",
		Text:      fmt.Sprintf("_…and %d more %s%s_", n, noun, plural(n)),
		Wrap:      true,
		IsSubtle:  true,
		Spacing:   "Small",
		Separator: true,
	}
}

// teamsText neutralises the Markdown Teams *does* honour inside a TextBlock, so
// a resource address or attribute value cannot break the line's formatting.
func teamsText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	for _, c := range []string{"*", "_", "[", "]"} {
		s = strings.ReplaceAll(s, c, "\\"+c)
	}
	return s
}

// --- payload shapes ---------------------------------------------------------

type teamsMessage struct {
	Type        string            `json:"type"`
	Attachments []teamsAttachment `json:"attachments"`
}

type teamsAttachment struct {
	ContentType string       `json:"contentType"`
	ContentURL  *string      `json:"contentUrl"`
	Content     adaptiveCard `json:"content"`
}

type adaptiveCard struct {
	Schema  string      `json:"$schema"`
	Type    string      `json:"type"`
	Version string      `json:"version"`
	Body    []acElement `json:"body"`
	Actions []acAction  `json:"actions,omitempty"`
	MSTeams *acMSTeams  `json:"msteams,omitempty"`
}

// acElement covers the Adaptive Card element types this card uses — TextBlock,
// FactSet, Container and ColumnSet. omitempty keeps the payload to the fields
// each one actually sets. IsVisible is a pointer because "false" is meaningful
// and must survive omitempty.
type acElement struct {
	Type                string   `json:"type"`
	ID                  string   `json:"id,omitempty"`
	Text                string   `json:"text,omitempty"`
	Weight              string   `json:"weight,omitempty"`
	Size                string   `json:"size,omitempty"`
	Color               string   `json:"color,omitempty"`
	Style               string   `json:"style,omitempty"`
	Wrap                bool     `json:"wrap,omitempty"`
	Spacing             string   `json:"spacing,omitempty"`
	IsSubtle            bool     `json:"isSubtle,omitempty"`
	Separator           bool     `json:"separator,omitempty"`
	Bleed               bool     `json:"bleed,omitempty"`
	HorizontalAlignment string   `json:"horizontalAlignment,omitempty"`
	IsVisible           *bool    `json:"isVisible,omitempty"`
	Facts               []acFact `json:"facts,omitempty"`

	Items        []acElement `json:"items,omitempty"`   // Container
	Columns      []acColumn  `json:"columns,omitempty"` // ColumnSet
	Inlines      []acInline  `json:"inlines,omitempty"` // RichTextBlock
	SelectAction *acAction   `json:"selectAction,omitempty"`
}

// acInline is a run of text inside a RichTextBlock. Highlight is what makes a
// chip: Teams paints a background derived from Color behind the run.
type acInline struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Color     string `json:"color,omitempty"`
	Weight    string `json:"weight,omitempty"`
	Size      string `json:"size,omitempty"`
	Highlight bool   `json:"highlight,omitempty"`
	IsSubtle  bool   `json:"isSubtle,omitempty"`
}

type acColumn struct {
	Type  string      `json:"type"`
	Width string      `json:"width,omitempty"`
	Items []acElement `json:"items,omitempty"`
}

type acFact struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

type acAction struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	// TargetElements drives Action.ToggleVisibility: every id listed flips
	// visibility when the action fires.
	TargetElements []string `json:"targetElements,omitempty"`
}

type acMSTeams struct {
	Width string `json:"width,omitempty"`
}

// teamsRetirementItems renders one row per catalogue entry: the deadline is the
// headline, the affected addresses are the detail, capped like every other list
// so one sprawling finding cannot fill the card.
func teamsRetirementItems(rets []Retirement, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, rt := range rets {
		if i == max {
			out = append(out, teamsMoreItem(len(rets)-max, "retirement"))
			break
		}
		glyph, colour := "⚠", "Warning"
		if rt.Urgency == string(retire.Retired) || rt.Urgency == string(retire.Imminent) {
			glyph, colour = "✖", "Attention"
		}
		out = append(out, teamsFinding(teamsRow{
			Glyph:        glyph,
			Colour:       colour,
			Title:        "**" + teamsText(rt.Title) + "**",
			Where:        teamsRetirementWhere(rt),
			Detail:       teamsPlain(teamsText(rt.Remediation)),
			Badge:        teamsBadge(rt.RetiresOn + " · " + rt.When()),
			New:          rt.BaselineState == "new",
			Unsuppressed: rt.Unsuppressed,
			Age:          teamsAge(rt.FirstSeen, rt.FirstRunURL),
			Sep:          i > 0,
		}))
	}
	return out
}

// teamsRetirementWhere lists the affected addresses, capped: a retirement can
// match dozens of resources and the count is the part that matters in a card.
func teamsRetirementWhere(rt Retirement) string {
	const shown = 3
	seen := make([]string, 0, shown)
	for _, in := range rt.Instances {
		if len(seen) == shown {
			break
		}
		seen = append(seen, in.Address)
	}
	where := strings.Join(seen, ", ")
	if extra := len(rt.Instances) - len(seen); extra > 0 {
		where += fmt.Sprintf(" +%d more", extra)
	}
	return teamsText(where)
}

// teamsRetirementFact summarises the retirement count, calling out the ones
// already past their date - the only ones that are not a future problem.
func teamsRetirementFact(rets []Retirement) string {
	if len(rets) == 0 {
		return "0"
	}
	var past int
	for _, rt := range rets {
		if rt.Days < 0 {
			past++
		}
	}
	if past == 0 {
		return fmt.Sprintf("%d", len(rets))
	}
	return fmt.Sprintf("%d (%d already retired)", len(rets), past)
}
