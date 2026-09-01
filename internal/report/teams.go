package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Microsoft Teams notification, as an Adaptive Card wrapped in the envelope a
// Power Automate "Workflows" HTTP trigger expects ({type:"message",
// attachments:[…]}). The retired Office 365 connector / MessageCard shape is
// deliberately not supported — Microsoft has switched those off.
//
// Teams renders only a subset of Markdown inside a TextBlock: **bold**,
// _italic_, [links](url) and lists. Backticks are NOT code-formatted, they show
// literally, so resource addresses are bolded rather than quoted.

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
	if len(drift) > 0 {
		body = append(body, teamsGroup("drift", "Changed outside Terraform", len(drift),
			teamsDriftItems(drift, max))...)
	}
	if len(depr) > 0 {
		body = append(body, teamsGroup("depr", "Deprecations", len(depr),
			teamsDeprItems(depr, max))...)
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

// teamsFinding is one row inside a group: a coloured glyph in a narrow column,
// then the title and its detail.
func teamsFinding(glyph, colour, title, detail string, separator bool) acElement {
	lines := []acElement{{Type: "TextBlock", Text: title, Wrap: true}}
	if detail != "" {
		lines = append(lines, acElement{
			Type: "TextBlock", Text: detail,
			Wrap: true, IsSubtle: true, Spacing: "None",
		})
	}
	return acElement{
		Type:      "ColumnSet",
		Spacing:   "Small",
		Separator: separator,
		Columns: []acColumn{
			{Type: "Column", Width: "auto", Items: []acElement{{
				Type: "TextBlock", Text: glyph, Color: colour, Weight: "Bolder",
			}}},
			{Type: "Column", Width: "stretch", Items: lines},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func (r *Report) teamsFacts(drift []ResourceReport, depr []Deprecation, ignored int) []acFact {
	add, chg, del := tally(r.Pending)
	facts := []acFact{
		{Title: "Drift", Value: teamsCount(len(drift), driftBreakdown(drift))},
		{Title: "Deprecations", Value: fmt.Sprintf("%d", len(depr))},
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

func teamsDriftItems(drift []ResourceReport, max int) []acElement {
	out := make([]acElement, 0, max+1)
	for i, rr := range drift {
		if i == max {
			out = append(out, teamsMoreItem(len(drift)-max, "resource"))
			break
		}
		title := "**" + teamsText(rr.Address) + "**"
		if rr.Module != "" {
			title += " _(" + teamsText(rr.Module) + ")_"
		}
		out = append(out, teamsFinding(
			sign(rr.Action), teamsActionColor(rr.Action), title, teamsAttrDetail(rr), i > 0))
	}
	return out
}

// teamsAttrDetail is the sub-line under a drifted resource: the first couple of
// attribute changes, or the one-line summary when there are none to show.
func teamsAttrDetail(rr ResourceReport) string {
	if len(rr.Attrs) == 0 {
		return rr.summaryLine()
	}
	const shown = 2
	parts := make([]string, 0, shown+1)
	for i, a := range rr.Attrs {
		if i == shown {
			parts = append(parts, fmt.Sprintf("+%d more", len(rr.Attrs)-shown))
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %s → %s",
			teamsText(a.Path), teamsText(clip(render(a.Old), 60)), teamsText(clip(render(a.New), 60))))
	}
	return strings.Join(parts, "; ")
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
		var detail []string
		if d.Detail != "" {
			detail = append(detail, teamsText(firstSentence(strings.ReplaceAll(d.Detail, "\n", " "), 200)))
		}
		if s := teamsSites(d); s != "" {
			detail = append(detail, s)
		}
		out = append(out, teamsFinding(
			glyph, colour, "**"+teamsText(d.Summary)+"**", strings.Join(detail, "\n\n"), i > 0))
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
	Type      string   `json:"type"`
	ID        string   `json:"id,omitempty"`
	Text      string   `json:"text,omitempty"`
	Weight    string   `json:"weight,omitempty"`
	Size      string   `json:"size,omitempty"`
	Color     string   `json:"color,omitempty"`
	Style     string   `json:"style,omitempty"`
	Wrap      bool     `json:"wrap,omitempty"`
	Spacing   string   `json:"spacing,omitempty"`
	IsSubtle  bool     `json:"isSubtle,omitempty"`
	Separator bool     `json:"separator,omitempty"`
	Bleed     bool     `json:"bleed,omitempty"`
	IsVisible *bool    `json:"isVisible,omitempty"`
	Facts     []acFact `json:"facts,omitempty"`

	Items        []acElement `json:"items,omitempty"`   // Container
	Columns      []acColumn  `json:"columns,omitempty"` // ColumnSet
	SelectAction *acAction   `json:"selectAction,omitempty"`
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
