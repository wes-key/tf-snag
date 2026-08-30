package plan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

// Diagnostic is one entry from a `terraform plan -json` diagnostic line. It is
// the raw notice; deciding which diagnostics are deprecations and de-duplicating
// them is the report layer's job.
type Diagnostic struct {
	Severity string // "warning" | "error" (anything else is passed through)
	Summary  string
	Detail   string
	Address  string // resource address, may be ""
	Filename string // from diagnostic.range.filename, may be ""
	Line     int    // from diagnostic.range.start.line, 0 when absent
	Snippet  string // from diagnostic.snippet.code, may be ""
}

type logLine struct {
	Type       string   `json:"type"`
	Diagnostic *logDiag `json:"diagnostic"`
}

type logDiag struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	Address  string `json:"address"`
	Range    *struct {
		Filename string `json:"filename"`
		Start    struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"start"`
	} `json:"range"`
	Snippet *struct {
		Code      string `json:"code"`
		StartLine int    `json:"start_line"`
	} `json:"snippet"`
}

// ParseLog decodes the newline-delimited JSON produced by `terraform plan -json`
// and returns its diagnostic entries. Lines that are not JSON, JSON objects that
// are not `"type":"diagnostic"`, and diagnostic lines with no `diagnostic`
// object are skipped silently — the log interleaves plenty of other event types
// and, on Windows, sometimes plain-text noise. Only an I/O failure or a failed
// UTF transcode is returned as an error.
func ParseLog(raw []byte) ([]Diagnostic, error) {
	raw, err := DecodeUTF(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing plan log: %w", err)
	}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var out []Diagnostic
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ll logLine
		if json.Unmarshal(line, &ll) != nil {
			continue
		}
		if ll.Type != "diagnostic" || ll.Diagnostic == nil {
			continue
		}
		d := ll.Diagnostic
		diag := Diagnostic{
			Severity: d.Severity,
			Summary:  d.Summary,
			Detail:   d.Detail,
			Address:  d.Address,
		}
		if d.Range != nil {
			diag.Filename = d.Range.Filename
			diag.Line = d.Range.Start.Line
		}
		if d.Snippet != nil {
			diag.Snippet = d.Snippet.Code
		}
		out = append(out, diag)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading plan log: %w", err)
	}
	return out, nil
}
