// Command tf-snag reads `terraform show -json` output and reports which
// resources changed outside Terraform. With -check deprecations it also reads a
// `terraform plan -json` log and reports the deprecation warnings in it. Parser
// first; Azure DevOps integration (PR comments, scheduled drift dashboard)
// builds on this.
package main

import (
	"bufio"
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

	"github.com/wes-key/tf-snag/internal/ignore"
	"github.com/wes-key/tf-snag/internal/plan"
	"github.com/wes-key/tf-snag/internal/report"
)

// version is stamped by the build with -ldflags "-X main.version=<v>" (see
// .github/workflows/ci.yml). Left empty for `go build`/`go run`, where
// versionLine falls back to the module version and the embedded VCS revision.
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// versionLine is a single line identifying the binary, e.g.
//
//	tf-snag 0.1.7 (a1b2c3d4e5f6) linux/amd64 go1.23.4
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
	format := fs.String("format", "text", "output format: text, json, markdown, junit or sarif")
	exitCode := fs.Bool("exit-code", true, "exit 2 when drift or a deprecation is detected")
	color := fs.String("color", "auto", "colorize text output: auto, always or never")
	source := fs.String("source", "", "Terraform source `dir`; when set, sarif/json findings carry the .tf file+line declaring each resource")
	baseline := fs.String("baseline", "", "previous run's tf-snag.sarif; `-format sarif`/`json` then stamps each result new/updated (sarif also emits absent)")
	ignorePath := fs.String("ignore", "", "tf-snag ignore YAML (default: .tf-snag-ignore.yml in cwd or -source); suppressed findings stay in the report but do not trip -exit-code")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: terraform show -json PLANFILE | tf-snag [flags]")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, versionLine())
		return 0
	}

	driftOn, deprOn, err := parseChecks(*checks)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	switch {
	case !deprOn, *format == "sarif", *format == "text", *format == "markdown", *format == "md", *format == "json":
		// deprecations are carried by these formats
	default:
		fmt.Fprintln(stderr, "tf-snag: -check deprecations supports -format sarif, json, text or markdown only")
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
	if *baseline != "" && *format != "sarif" && *format != "json" {
		fmt.Fprintln(stderr, "tf-snag: -baseline applies to -format sarif or json")
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

	if code := applyIgnores(rep, *ignorePath, *source, stderr); code != 0 {
		return code
	}

	switch *format {
	case "text":
		if useColor {
			err = rep.WriteTextColor(stdout)
		} else {
			err = rep.WriteText(stdout)
		}
	case "json":
		if *source != "" {
			srcIndex, serr := indexTFSources(*source)
			if serr != nil {
				fmt.Fprintln(stderr, "tf-snag:", serr)
				return 2
			}
			rep.AttachSourceLocations(srcIndex)
		}
		if *baseline != "" {
			prior, code := loadPrior(*baseline, stderr)
			if code != 0 {
				return code
			}
			prior.StampReport(rep)
		}
		err = rep.WriteJSON(stdout)
	case "markdown", "md":
		err = rep.WriteMarkdown(stdout)
	case "junit":
		err = rep.WriteJUnit(stdout)
	case "sarif":
		var srcIndex map[string]report.SourceLoc
		if *source != "" {
			srcIndex, err = indexTFSources(*source)
			if err != nil {
				fmt.Fprintln(stderr, "tf-snag:", err)
				return 2
			}
		}
		var prior *report.PriorResults
		if *baseline != "" {
			var code int
			if prior, code = loadPrior(*baseline, stderr); code != 0 {
				return code
			}
		}
		err = rep.WriteSARIF(stdout, srcIndex, prior)
	default:
		fmt.Fprintf(stderr, "tf-snag: unknown format %q (want text, json, markdown, junit or sarif)\n", *format)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}

	if *exitCode && rep.HasGatingFindings() {
		return 2
	}
	return 0
}

// applyIgnores loads the ignore file (explicit -ignore, else an auto-discovered
// .tf-snag-ignore.yml) and inline .tf comments (when -source is set), and marks
// matching findings suppressed. Returns a non-zero exit code on a hard error.
func applyIgnores(rep *report.Report, ignorePath, source string, stderr io.Writer) int {
	set := &ignore.Set{}

	path := ignorePath
	if path == "" {
		path = discoverIgnore(source)
	}
	s, err := ignore.LoadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}
	set.Merge(s)

	if source != "" {
		s, err := ignore.FromSource(source)
		if err != nil {
			fmt.Fprintln(stderr, "tf-snag:", err)
			return 2
		}
		set.Merge(s)
	}

	set.Apply(rep)
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
