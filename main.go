// Command tf-snag reads `terraform show -json` output and reports which
// resources changed outside Terraform. With -check deprecations it also reads a
// `terraform plan -json` log and reports the deprecation warnings in it. Parser
// first; Azure DevOps integration (PR comments, scheduled drift dashboard)
// builds on this.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/wes-key/tf-snag/internal/ado"
	"github.com/wes-key/tf-snag/internal/ignore"
	"github.com/wes-key/tf-snag/internal/plan"
	"github.com/wes-key/tf-snag/internal/report"
	"github.com/wes-key/tf-snag/internal/teams"
	"github.com/wes-key/tf-snag/internal/wiki"
)

// version is stamped by the build with -ldflags "-X main.version=<v>" (see
// .github/workflows/ci.yml). Left empty for `go build`/`go run`, where
// versionLine falls back to the module version and the embedded VCS revision.
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// wordmark is the startup banner, drawn for -version and -help and nowhere
// else. Every other invocation writes a report to stdout - JSON, SARIF, JUnit,
// markdown, a Teams card - and a pipeline redirects that straight to a file, so
// there is no run in which decoration on stdout would be anything but corruption.
const wordmark = `
████████╗███████╗   ███████╗███╗   ██╗ █████╗  ██████╗
╚══██╔══╝██╔════╝   ██╔════╝████╗  ██║██╔══██╗██╔════╝
   ██║   █████╗     ███████╗██╔██╗ ██║███████║██║  ███╗
   ██║   ██╔══╝     ╚════██║██║╚██╗██║██╔══██║██║   ██║
   ██║   ██║        ███████║██║ ╚████║██║  ██║╚██████╔╝
   ╚═╝   ╚═╝        ╚══════╝╚═╝  ╚═══╝╚═╝  ╚═╝ ╚═════╝
`

// bannerPurple is #7B42BC, the Terraform purple the extension's logo and task
// icons are drawn in (extension/tools/genlogo/main.go), as its 256-colour
// approximation - the report's own palette is 16-colour, but nothing there is
// trying to match artwork. A terminal that does not know the code ignores it and
// prints the wordmark plain.
const bannerPurple = "\x1b[1;38;5;99m"

// writeWordmark draws the banner one line at a time rather than wrapping the
// whole block in a single colour pair: an Azure DevOps log prefixes every line
// it renders, and colour spanning a newline bleeds into the prefix.
func writeWordmark(w io.Writer, useColor bool) {
	for _, line := range strings.Split(strings.Trim(wordmark, "\n"), "\n") {
		if useColor {
			fmt.Fprintf(w, "%s%s\x1b[0m\n", bannerPurple, line)
		} else {
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintln(w)
}

// bannerColor follows the same rules as the report - -color, then NO_COLOR -
// but auto-detects against the stream the banner is actually going to, which is
// always stderr. Checking stdout instead would colour the banner in a pipeline,
// where the drift step passes -color=always to a log that is not a terminal.
func bannerColor(mode string, stderr io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default:
		return os.Getenv("NO_COLOR") == "" && isTerminal(stderr)
	}
}

// versionLine is a single line identifying the binary, e.g.
//
//	tf-snag 0.1.7 (a1b2c3d4e5f6) linux/amd64 go1.27.1
func versionLine() string {
	v := version
	bi, ok := debug.ReadBuildInfo()
	if v == "" && ok {
		v = bi.Main.Version
	}
	if v == "" || v == "(devel)" {
		v = "dev"
	}

	var rev, dirty string
	if ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" {
		v += " (" + rev + dirty + ")"
	}

	return fmt.Sprintf("tf-snag %s %s/%s %s", v, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tf-snag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	planPath := fs.String("plan", "", "path to `terraform show -json` output (default: stdin)")
	planLogPath := fs.String("plan-log", "", "path to `terraform plan -json` NDJSON log (required by -check deprecations)")
	planLogDir := fs.String("plan-log-dir", "", "directory `terraform plan` ran in, relative to the repo root; prepended to deprecation file locations so their links resolve")
	checks := fs.String("check", "drift", "analyses to run: drift, deprecations, or a comma `list` (also: all)")
	format := fs.String("format", "text", "output format: text, json, markdown, junit, sarif or teams")
	exitCode := fs.Bool("exit-code", true, "exit 2 when drift or a deprecation is detected")
	color := fs.String("color", "auto", "colorize text output: auto, always or never")
	source := fs.String("source", "", "Terraform source `dir`; when set, sarif/json findings carry the .tf file+line declaring each resource")
	baseline := fs.String("baseline", "", "previous run's tf-snag.sarif; `-format sarif`/`json` then stamps each result new/updated (sarif also emits absent)")
	ignorePath := fs.String("ignore", "", "tf-snag ignore YAML (default: .tf-snag-ignore.yml in cwd or -source); suppressed findings stay in the report but do not trip -exit-code")
	teamsHook := fs.String("teams-webhook", "", "Microsoft Teams Power Automate Workflows `url` to POST the report card to (default: $TF_SNAG_TEAMS_WEBHOOK). Treat as a secret")
	teamsNotify := fs.String("teams-notify", "findings", "when to post to Teams: `findings` (anything un-suppressed), new (only findings absent from -baseline) or always")
	teamsContext := fs.String("teams-context", "", "subtle `line` under the Teams card headline, e.g. the pipeline, branch and run number")
	runURL := fs.String("run-url", "", "this CI run's `url`; recorded against findings first seen in this run (so later runs can link back) and used for the Teams card's \"View run\" button")
	adoURL := fs.String("ado-url", "", "Azure DevOps project `url` (https://dev.azure.com/org/project) to raise work items in; needs a token in $TF_SNAG_ADO_TOKEN")
	adoToken := fs.String("ado-token", "", "Azure DevOps PAT or System.AccessToken (default: $TF_SNAG_ADO_TOKEN). Treat as a secret")
	adoType := fs.String("ado-type", "Task", "work item `type` to raise, e.g. Task, Bug or Issue")
	adoArea := fs.String("ado-area", "", "area `path` for new work items (default: the project root)")
	adoRaise := fs.String("ado-raise", "new", "which findings get a work item: `new` (absent from -baseline) or findings (anything un-suppressed)")
	adoClose := fs.Bool("ado-close", false, "close work items whose finding is no longer reported")
	adoClosedState := fs.String("ado-closed-state", "", "`state` to move a resolved finding's work item to; default: the work item type's own completed state (Closed on Agile, Done on Scrum and Basic)")
	adoWorkItems := fs.Bool("ado-work-items", true, "raise work items when -ado-url is set. Turn off to use -ado-url purely as the project locator, e.g. for -wiki-page alone")
	adoDryRun := fs.Bool("ado-dry-run", false, "report the work items that would be raised or closed, without changing anything")
	wikiPage := fs.String("wiki-page", "", "Azure DevOps wiki `path` to publish the exceptions register to, e.g. /tf-snag/Exceptions; needs -ado-url")
	wikiName := fs.String("wiki", "", "wiki `name` or id to write to (default: the project wiki, or the only wiki when there is one)")
	wikiDryRun := fs.Bool("wiki-dry-run", false, "report what would be written to the wiki, and print the page, without changing anything")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		writeWordmark(stderr, bannerColor(*color, stderr))
		fmt.Fprintln(stderr, "usage: terraform show -json PLANFILE | tf-snag [flags]")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		// Banner to stderr, version to stdout. `tf-snag -version` stays exactly
		// one parseable line for anything reading it, and a person still gets
		// the wordmark.
		writeWordmark(stderr, bannerColor(*color, stderr))
		fmt.Fprintln(stdout, versionLine())
		return 0
	}

	driftOn, deprOn, err := parseChecks(*checks)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	switch {
	case !deprOn, *format == "sarif", *format == "text", *format == "markdown", *format == "md",
		*format == "json", *format == "teams":
		// deprecations are carried by these formats
	default:
		fmt.Fprintln(stderr, "tf-snag: -check deprecations supports -format sarif, json, teams, text or markdown only")
		return 2
	}

	if *planLogPath != "" && !deprOn {
		fmt.Fprintln(stderr, "tf-snag: -plan-log set but -check does not include deprecations")
		return 2
	}
	if *planLogDir != "" && !deprOn {
		fmt.Fprintln(stderr, "tf-snag: -plan-log-dir set but -check does not include deprecations")
		return 2
	}
	notify, err := parseTeamsNotify(*teamsNotify)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	// The webhook URL is a credential; prefer the environment so it never has to
	// appear in a command line (or a pipeline log). Resolved before the -baseline
	// check below, which has to know whether a card is being posted at all.
	hook := *teamsHook
	if hook == "" {
		hook = os.Getenv("TF_SNAG_TEAMS_WEBHOOK")
	}

	// -baseline applies with any format when a card is being posted: it is what
	// marks findings new, and what -teams-notify new gates on.
	// -wiki-page is a baseline consumer too: the register reports how long each
	// waived finding has been there, which is provenance the baseline carries.
	if *baseline != "" && *format != "sarif" && *format != "json" && *format != "teams" && hook == "" && *wikiPage == "" {
		fmt.Fprintln(stderr, "tf-snag: -baseline applies to -format sarif, json or teams, or when posting to Teams or publishing -wiki-page")
		return 2
	}
	if *wikiPage != "" && *adoURL == "" {
		fmt.Fprintln(stderr, "tf-snag: -wiki-page needs -ado-url to say which project's wiki to write to")
		return 2
	}
	if *planPath != "" && !driftOn {
		fmt.Fprintln(stderr, "tf-snag: -plan set but -check does not include drift")
		return 2
	}
	if driftOn && *planPath == "" && deprOn && *planLogPath == "" {
		fmt.Fprintln(stderr, "tf-snag: cannot read both the plan and the plan log from stdin; pass -plan or -plan-log")
		return 2
	}

	var useColor bool
	switch *color {
	case "always":
		useColor = true
	case "never":
		useColor = false
	case "auto":
		useColor = os.Getenv("NO_COLOR") == "" && isTerminal(stdout)
	default:
		fmt.Fprintf(stderr, "tf-snag: invalid -color %q (want auto, always or never)\n", *color)
		return 2
	}

	var p *plan.Plan
	if driftOn {
		raw, err := readInput(*planPath, stdin)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		p, err = plan.Parse(raw)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
	}

	var diags []plan.Diagnostic
	if deprOn {
		logRaw, err := readInput(*planLogPath, stdin)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		diags, err = plan.ParseLog(logRaw)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		// `terraform plan -json` reports diagnostic ranges relative to the dir
		// it ran in (e.g. `main.tf`). Prepend that dir so the paths are
		// repo-root-relative and their Scans-tab links resolve.
		if dir := filepath.ToSlash(*planLogDir); dir != "" {
			for i := range diags {
				if f := filepath.ToSlash(diags[i].Filename); f != "" && !path.IsAbs(f) {
					diags[i].Filename = path.Join(dir, f)
				}
			}
		}
	}

	if p == nil {
		p = &plan.Plan{}
	}
	rep := report.Build(p)
	if deprOn {
		rep.Deprecations = report.Deprecations(diags)
	}

	ignoreSet, code := applyIgnores(rep, *ignorePath, *source, stderr)
	if code != 0 {
		return code
	}

	teamsOpts := report.TeamsOptions{Context: *teamsContext, RunURL: *runURL}

	// Read the baseline once. Stamping the report is what gives the JSON and the
	// Teams card their new/updated + first-seen marks; WriteSARIF takes the same
	// prior and stamps its results itself (and re-emits what has gone absent).
	var prior *report.PriorResults
	if *baseline != "" {
		var code int
		if prior, code = loadPrior(*baseline, stderr); code != 0 {
			return code
		}
		// Anything absent from the baseline is being seen for the first time
		// here, so this run is its first-detection run from now on.
		prior.RunURL = *runURL
		prior.StampReport(rep)
	}

	// Resolve -source once: the SARIF writer wants the index for its physical
	// locations, and everything else wants file+line stamped on the findings
	// (the JSON tab shows it, and it goes in a work item's body).
	var srcIndex map[string]report.SourceLoc
	if *source != "" {
		if srcIndex, err = indexTFSources(*source); err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		rep.AttachSourceLocations(srcIndex)
	}

	if *adoURL != "" && *adoWorkItems {
		cfg := adoConfig{
			url:         *adoURL,
			token:       firstNonEmpty(*adoToken, os.Getenv("TF_SNAG_ADO_TOKEN")),
			itemType:    *adoType,
			area:        *adoArea,
			raise:       *adoRaise,
			closeItems:  *adoClose,
			closedState: *adoClosedState,
			dryRun:      *adoDryRun,
			runURL:      *runURL,
			context:     *teamsContext,
		}
		if code := syncWorkItems(rep, cfg, stderr); code != 0 {
			return code
		}
	}

	switch *format {
	case "text":
		if useColor {
			err = rep.WriteTextColor(stdout)
		} else {
			err = rep.WriteText(stdout)
		}
	case "json":
		err = rep.WriteJSON(stdout)
	case "markdown", "md":
		err = rep.WriteMarkdown(stdout)
	case "junit":
		err = rep.WriteJUnit(stdout)
	case "sarif":
		err = rep.WriteSARIF(stdout, srcIndex, prior)
	case "teams":
		err = rep.WriteTeams(stdout, teamsOpts)
	default:
		fmt.Fprintf(stderr, "tf-snag: unknown format %q (want text, json, markdown, junit, sarif or teams)\n", *format)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}

	// After the report is on stdout, not before. The register only reads the
	// report, so an unreachable wiki must not cost the run its tab attachment -
	// which is what happened when this sat above the write. Work items stay
	// before it because they stamp item references onto the report itself.
	if *wikiPage != "" {
		cfg := wikiConfig{
			url:     *adoURL,
			token:   firstNonEmpty(*adoToken, os.Getenv("TF_SNAG_ADO_TOKEN")),
			wiki:    *wikiName,
			page:    *wikiPage,
			dryRun:  *wikiDryRun,
			runURL:  *runURL,
			context: *teamsContext,
		}
		if code := publishExceptions(rep, ignoreSet, cfg, stderr); code != 0 {
			return code
		}
	}

	if hook != "" {
		if code := postTeams(rep, hook, teamsOpts, notify, stderr); code != 0 {
			return code
		}
	}

	if *exitCode && rep.HasGatingFindings() {
		return 2
	}
	return 0
}

// applyIgnores loads the ignore file (explicit -ignore, else an auto-discovered
// .tf-snag-ignore.yml) and inline .tf comments (when -source is set), and marks
// matching findings suppressed. Returns a non-zero exit code on a hard error.
func applyIgnores(rep *report.Report, ignorePath, source string, stderr io.Writer) (*ignore.Set, int) {
	set := &ignore.Set{}

	path := ignorePath
	if path == "" {
		path = discoverIgnore(source)
	}
	s, err := ignore.LoadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return nil, 2
	}
	set.Merge(s)

	if source != "" {
		s, err := ignore.FromSource(source)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return nil, 2
		}
		set.Merge(s)
	}

	set.Apply(rep)
	return set, 0
}

// adoConfig is the resolved -ado-* configuration for one run.
type wikiConfig struct {
	url, token      string
	wiki, page      string
	dryRun          bool
	runURL, context string
}

// publishExceptions renders the exceptions register — every ignore rule, what it
// currently suppresses, and which ones have stopped suppressing anything — and
// writes it to an Azure DevOps wiki page.
//
// Runs after the baseline has been stamped, so the register can say how long the
// estate has been carrying each waived finding. All output goes to stderr: the
// report owns stdout.
func publishExceptions(rep *report.Report, set *ignore.Set, cfg wikiConfig, stderr io.Writer) int {
	orgURL, project, err := ado.ParseURL(cfg.url)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	if cfg.token == "" {
		fmt.Fprintln(stderr, "tf-snag: no Azure DevOps token — set $TF_SNAG_ADO_TOKEN or -ado-token")
		return 2
	}
	// Render before touching Azure DevOps. A dry run is for inspecting the page,
	// and having to be able to reach the wiki before it will show you one makes
	// it useless in exactly the situation you reach for it.
	exs := set.Exceptions()
	wiki.SortExceptions(exs)
	page := wiki.Render(exs, rep, wiki.Options{Context: cfg.context, RunURL: cfg.runURL})

	if cfg.dryRun {
		fmt.Fprintln(stderr, "----- wiki page (dry run) -----")
		fmt.Fprint(stderr, page)
		fmt.Fprintln(stderr, "-------------------------------")
	}

	client := &ado.Client{OrgURL: orgURL, Project: project, Token: cfg.token}
	target, err := client.ResolveWiki(cfg.wiki)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	fmt.Fprintln(stderr, "wiki: writing to "+target.Describe())

	if _, err := wiki.Publish(wiki.Client{Client: client}, target, cfg.page, page, cfg.dryRun, stderr); err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	return 0
}

type adoConfig struct {
	url, token      string
	itemType, area  string
	raise           string
	closeItems      bool
	closedState     string
	dryRun          bool
	runURL, context string
}

// syncWorkItems raises Azure DevOps work items for findings and, with
// -ado-close, closes those whose finding has gone. It validates the
// configuration before touching anything: a bad token, project or work item type
// is worth knowing about up front rather than after half the findings have been
// raised.
//
// Everything it prints goes to stderr. stdout carries the report, and -format
// json/sarif is redirected straight to a file by the pipeline — a progress line
// in the middle of that makes it unparseable.
func syncWorkItems(rep *report.Report, cfg adoConfig, stderr io.Writer) int {
	if strings.TrimSpace(cfg.token) == "" {
		fmt.Fprintln(stderr, "tf-snag: -ado-url needs a token — pass -ado-token or set $TF_SNAG_ADO_TOKEN")
		return 2
	}
	orgURL, project, err := ado.ParseURL(cfg.url)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	eligible, err := adoEligibility(cfg.raise, rep, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}

	client := &ado.Client{OrgURL: orgURL, Project: project, Token: cfg.token}
	opts := ado.Options{
		Type:        cfg.itemType,
		AreaPath:    cfg.area,
		ClosedState: cfg.closedState,
		Close:       cfg.closeItems,
		DryRun:      cfg.dryRun,
		RunURL:      cfg.runURL,
		Context:     cfg.context,
	}

	// Pre-flight: a validateOnly create exercises the same permission and the
	// same work item type as the real thing, without leaving anything behind.
	if err := client.Validate(ado.NewItem{
		FindingID: "preflight",
		Type:      cfg.itemType,
		Title:     "tf-snag permission check",
	}); err != nil {
		fmt.Fprintln(stderr, "tf-snag: cannot raise work items:", err)
		return 2
	}

	// Whether an existing item is still open decides whether a finding links to
	// it or gets a fresh one, so the type's finished states are needed on every
	// run, not only when closing. Best effort: if the lookup fails, fall back to
	// matching the closed state by name rather than abandoning the run.
	if cats, err := client.StateCategories(cfg.itemType); err != nil {
		fmt.Fprintf(stderr, "tf-snag: could not read %s states (%v); treating only %q as closed\n",
			cfg.itemType, err, cfg.closedState)
	} else {
		opts.Terminal = map[string]bool{}
		for name, cat := range cats {
			if cat == "Completed" || cat == "Removed" {
				opts.Terminal[name] = true
			}
		}
	}

	// Closing is a state transition, which the create check above does not
	// exercise at all — so resolve and verify the state now, before a finding
	// disappearing turns into a 400 mid-run. An empty -ado-closed-state picks
	// the type's own completed state, which is what makes this work unchanged on
	// Agile ("Closed"), Scrum and Basic ("Done").
	if cfg.closeItems {
		state, err := client.ResolveClosedState(cfg.itemType, cfg.closedState)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		if cfg.closedState == "" {
			fmt.Fprintf(stderr, "tf-snag: closing resolved %s work items as %q\n", cfg.itemType, state)
		}
		opts.ClosedState = state
	}

	res, err := client.Sync(rep, opts, eligible, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}

	verb := "raised"
	if cfg.dryRun {
		verb = "would raise"
	}
	fmt.Fprintf(stderr, "work items: %s %d, closed %d, commented %d, already tracked %d\n",
		verb, len(res.Created), len(res.Closed), len(res.Noted), res.Existing)
	if n := len(res.Duplicates); n > 0 {
		fmt.Fprintf(stderr, "  %d finding(s) have more than one open work item; see the notes above\n", n)
	}
	for _, ch := range res.Created {
		if ch.ID != 0 {
			fmt.Fprintf(stderr, "  #%d %s\n", ch.ID, ch.URL)
		}
	}
	return 0
}

// adoEligibility turns -ado-raise into the predicate Sync applies to creation.
// "new" needs a -baseline to mean anything; without one it degrades to
// "findings" and says so, rather than silently never raising an item.
func adoEligibility(raise string, rep *report.Report, stderr io.Writer) (func(ado.FindingKind, string) bool, error) {
	switch strings.ToLower(strings.TrimSpace(raise)) {
	case "findings":
		return nil, nil // nil means "everything reaching this point"
	case "", "new":
		if !rep.IsBaselined() {
			fmt.Fprintln(stderr, "tf-snag: -ado-raise new needs -baseline to identify new findings; raising for any un-suppressed finding instead")
			return nil, nil
		}
		// Newly actionable, not merely newly detected: a finding whose ignore
		// rule was just removed needs an item too. Its old one, if it had one,
		// was closed when the rule went in.
		newIDs := map[string]bool{}
		for _, rr := range rep.Drift {
			if rr.BaselineState == "new" || rr.Unsuppressed {
				newIDs[rr.FindingID()] = true
			}
		}
		for _, d := range rep.Deprecations {
			if d.BaselineState == "new" || d.Unsuppressed {
				newIDs[d.FindingID()] = true
			}
		}
		return func(_ ado.FindingKind, id string) bool { return newIDs[id] }, nil
	default:
		return nil, fmt.Errorf("invalid -ado-raise %q (want new or findings)", raise)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseTeamsNotify validates -teams-notify and normalises the empty value.
func parseTeamsNotify(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "":
		return "findings", nil
	case "findings", "always", "new":
		return v, nil
	default:
		return "", fmt.Errorf("invalid -teams-notify %q (want findings, new or always)", s)
	}
}

// teamsShouldPost applies -teams-notify. "new" needs a -baseline to tell a fresh
// finding from one that has been there for weeks; without one it falls back to
// "findings" rather than going silent, because a notifier that quietly never
// fires is the worst failure mode available to it.
func teamsShouldPost(rep *report.Report, notify string, stderr io.Writer) bool {
	switch notify {
	case "always":
		return true
	case "new":
		if !rep.IsBaselined() {
			fmt.Fprintln(stderr, "tf-snag: -teams-notify new needs -baseline to identify new findings; posting as -teams-notify findings would")
			return rep.HasGatingFindings()
		}
		// Also fires for a finding whose ignore rule has just been removed: it
		// was invisible here yesterday and is actionable today, which is the
		// thing "new" is really gating on.
		return rep.HasNewlyActionable()
	default: // findings
		return rep.HasGatingFindings()
	}
}

// postTeams sends the report card to the webhook when -teams-notify says this
// run warrants one. A failure to post is a hard error: a notification silently
// not arriving is worse than a loud one.
func postTeams(rep *report.Report, hook string, opts report.TeamsOptions, notify string, stderr io.Writer) int {
	if !teamsShouldPost(rep, notify, stderr) {
		return 0
	}
	var buf bytes.Buffer
	if err := rep.WriteTeams(&buf, opts); err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	if err := (&teams.Client{}).Post(hook, buf.Bytes()); err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	return 0
}

// loadPrior reads and parses a previous run's tf-snag.sarif for -baseline. On
// error it prints to stderr and returns a non-zero exit code (the second
// return); callers propagate that. A successful parse returns (prior, 0).
func loadPrior(path string, stderr io.Writer) (*report.PriorResults, int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return nil, 2
	}
	prior, err := report.ParsePriorSARIF(raw)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return nil, 2
	}
	return prior, 0
}

// discoverIgnore returns the first existing .tf-snag-ignore.yml (or .yaml) in
// the cwd or the -source root, else "".
func discoverIgnore(source string) string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if source != "" {
		dirs = append(dirs, source)
	}
	for _, d := range dirs {
		for _, name := range []string{".tf-snag-ignore.yml", ".tf-snag-ignore.yaml"} {
			p := filepath.Join(d, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// parseChecks turns the -check value into the set of analyses to run.
func parseChecks(s string) (drift, depr bool, err error) {
	if strings.TrimSpace(s) == "" {
		return false, false, fmt.Errorf("empty -check (want drift, deprecations or all)")
	}
	for _, tok := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "drift":
			drift = true
		case "deprecation", "deprecations":
			depr = true
		case "all", "both":
			drift, depr = true, true
		default:
			return false, false, fmt.Errorf("unknown check %q (want drift, deprecations or all)", strings.TrimSpace(tok))
		}
	}
	return drift, depr, nil
}

// readInput reads path, or stdin when path is empty.
func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return io.ReadAll(stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// isTerminal reports whether w is a character device (a real terminal), so
// `-color=auto` can default colour on for interactive use and off for pipes
// and files.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var reResourceDecl = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"`)

// indexTFSources walks root for *.tf files and maps "<type>.<name>" to the
// first `resource "<type>" "<name>"` declaration found. Best effort: first
// match wins; count/for_each and duplicate local names are not disambiguated.
// Paths are returned relative to root, with forward slashes.
func indexTFSources(root string) (map[string]report.SourceLoc, error) {
	out := map[string]report.SourceLoc{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for ln := 1; sc.Scan(); ln++ {
			m := reResourceDecl.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			key := m[1] + "." + m[2]
			if _, seen := out[key]; !seen {
				out[key] = report.SourceLoc{File: rel, Line: ln}
			}
		}
		return sc.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
