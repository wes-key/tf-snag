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
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

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
	checks := fs.String("check", "drift", "analyses to run: drift, deprecations, or a comma `list` (also: all)")
	format := fs.String("format", "text", "output format: text, json, markdown, junit or sarif")
	exitCode := fs.Bool("exit-code", true, "exit 2 when drift or a deprecation is detected")
	color := fs.String("color", "auto", "colorize text output: auto, always or never")
	source := fs.String("source", "", "Terraform source `dir`; when set, sarif locations link to the .tf file declaring each resource")
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
	if deprOn && *format != "sarif" && *format != "text" {
		fmt.Fprintln(stderr, "tf-snag: -check deprecations supports -format sarif or text only")
		return 2
	}
	if *planLogPath != "" && !deprOn {
		fmt.Fprintln(stderr, "tf-snag: -plan-log set but -check does not include deprecations")
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
	}

	if p == nil {
		p = &plan.Plan{}
	}
	rep := report.Build(p)
	if deprOn {
		rep.Deprecations = report.Deprecations(diags)
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
		var srcIndex map[string]report.SourceLoc
		if *source != "" {
			srcIndex, err = indexTFSources(*source)
			if err != nil {
				fmt.Fprintln(stderr, "tf-snag:", err)
				return 2
			}
		}
		err = rep.WriteSARIF(stdout, srcIndex)
	default:
		fmt.Fprintf(stderr, "tf-snag: unknown format %q (want text, json, markdown, junit or sarif)\n", *format)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "tf-snag:", err)
		return 2
	}

	if *exitCode && (rep.HasDrift() || rep.HasDeprecations()) {
		return 2
	}
	return 0
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
