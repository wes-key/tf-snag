package plan

import (
	"strings"
	"testing"
)

// A representative slice of `terraform plan -json` output: version banner, three
// deprecation-ish diagnostics (one repeated for a second instance), one
// non-deprecation warning, a non-diagnostic JSON event, a plain-text line and a
// change_summary.
const sampleLog = `{"@level":"info","@message":"Terraform 1.9.6","@module":"terraform.ui","type":"version","terraform":"1.9.6","ui":"1.2"}
{"@level":"warning","@message":"Warning: Argument is deprecated","@module":"terraform.ui","diagnostic":{"severity":"warning","summary":"Argument is deprecated","detail":"The \"foo\" argument is deprecated. Use \"bar\" instead.","address":"module.a.azurerm_x.y","range":{"filename":"modules/a/main.tf","start":{"line":12,"column":3},"end":{"line":12,"column":20}},"snippet":{"code":"  foo = true","start_line":12}},"type":"diagnostic"}
{"@level":"warning","@message":"Warning: Argument is deprecated","@module":"terraform.ui","diagnostic":{"severity":"warning","summary":"Argument is deprecated","detail":"The \"foo\" argument is deprecated. Use \"bar\" instead.","address":"module.a.azurerm_x.z","range":{"filename":"modules/a/main.tf","start":{"line":12,"column":3},"end":{"line":12,"column":20}}},"type":"diagnostic"}
{"@level":"warning","@message":"Warning: Deprecated attribute","@module":"terraform.ui","diagnostic":{"severity":"warning","summary":"Deprecated attribute","detail":"The attribute \"bar\" is deprecated.","address":"azurerm_p.q","range":{"filename":"main.tf","start":{"line":5,"column":1},"end":{"line":5,"column":10}}},"type":"diagnostic"}
{"@level":"warning","@message":"Warning: Values may be incompatible","@module":"terraform.ui","diagnostic":{"severity":"warning","summary":"Values may be incompatible","detail":"Automatic type conversion may not preserve intent.","range":{"filename":"main.tf","start":{"line":9,"column":1}}},"type":"diagnostic"}
{"@level":"info","@message":"module.a.azurerm_x.y: Drift detected","@module":"terraform.ui","type":"resource_drift"}
this line is not json at all
{"@level":"info","@message":"Plan: 0 to add, 0 to change, 0 to destroy.","type":"change_summary","changes":{"add":0,"change":0,"remove":0,"operation":"plan"}}`

func TestParseLogExtractsDiagnostics(t *testing.T) {
	diags, err := ParseLog([]byte(sampleLog))
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 4 {
		t.Fatalf("got %d diagnostics, want 4: %+v", len(diags), diags)
	}

	d := diags[0]
	if d.Severity != "warning" || d.Summary != "Argument is deprecated" {
		t.Errorf("diag[0] severity/summary = %q/%q", d.Severity, d.Summary)
	}
	if d.Address != "module.a.azurerm_x.y" || d.Filename != "modules/a/main.tf" || d.Line != 12 {
		t.Errorf("diag[0] address/file/line = %q/%q/%d", d.Address, d.Filename, d.Line)
	}
	if !strings.Contains(d.Detail, "bar") {
		t.Errorf("diag[0] detail = %q", d.Detail)
	}
	if d.Snippet != "  foo = true" {
		t.Errorf("diag[0] snippet = %q", d.Snippet)
	}
}

func TestParseLogSkipsGarbageAndNonDiagnostic(t *testing.T) {
	raw := "not json\n" +
		`{"type":"version","terraform":"1.9.6"}` + "\n" +
		`{"type":"resource_drift","@message":"x"}` + "\n" +
		`{"type":"change_summary","changes":{"add":0}}` + "\n" +
		"\n   \n"
	diags, err := ParseLog([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 {
		t.Fatalf("got %d diagnostics, want 0: %+v", len(diags), diags)
	}
}

func TestParseLogHandlesCRLF(t *testing.T) {
	diags, err := ParseLog([]byte(strings.ReplaceAll(sampleLog, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 4 {
		t.Fatalf("CRLF: got %d diagnostics, want 4", len(diags))
	}
	if diags[3].Filename != "main.tf" || diags[3].Line != 9 {
		t.Errorf("CRLF: diag[3] file/line = %q/%d", diags[3].Filename, diags[3].Line)
	}
}

func TestParseLogHandlesBOM(t *testing.T) {
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(sampleLog)...)
	diags, err := ParseLog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 4 {
		t.Fatalf("BOM: got %d diagnostics, want 4", len(diags))
	}
}

func TestParseLogEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "\r\n"} {
		diags, err := ParseLog([]byte(in))
		if err != nil {
			t.Fatalf("ParseLog(%q) error: %v", in, err)
		}
		if diags != nil {
			t.Errorf("ParseLog(%q) = %+v, want nil", in, diags)
		}
	}
}
