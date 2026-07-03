package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDiagnosticCommandVersion(t *testing.T) {
	oldVersion := version
	version = "1.2.3"
	defer func() { version = oldVersion }()

	var out bytes.Buffer
	handled, code := runDiagnosticCommand([]string{"version"}, &out)
	if !handled {
		t.Fatal("runDiagnosticCommand handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("runDiagnosticCommand code = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != "1.2.3" {
		t.Fatalf("output = %q, want version", out.String())
	}
}

func TestRunDiagnosticCommandStatus(t *testing.T) {
	oldVersion := version
	oldDate := date
	version = "2.0.0"
	date = "2026-06-16T00:00:00Z"
	defer func() {
		version = oldVersion
		date = oldDate
	}()

	var out bytes.Buffer
	handled, code := runDiagnosticCommand([]string{"diagnose", "status"}, &out)
	if !handled {
		t.Fatal("runDiagnosticCommand handled = false, want true")
	}
	if code != 0 {
		t.Fatalf("runDiagnosticCommand code = %d, want 0", code)
	}
	if got := out.String(); !strings.Contains(got, "status=ok") || !strings.Contains(got, "version=2.0.0") {
		t.Fatalf("output = %q, want status and version", got)
	}
}

func TestRunDiagnosticCommandInvalidDiagnoseUsage(t *testing.T) {
	var out bytes.Buffer
	handled, code := runDiagnosticCommand([]string{"diagnose"}, &out)
	if !handled {
		t.Fatal("runDiagnosticCommand handled = false, want true")
	}
	if code != 2 {
		t.Fatalf("runDiagnosticCommand code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatalf("output = %q, want usage", out.String())
	}
}

func TestRunDiagnosticCommandUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	handled, code := runDiagnosticCommand([]string{"run"}, &out)
	if handled {
		t.Fatal("runDiagnosticCommand handled = true, want false")
	}
	if code != 0 {
		t.Fatalf("runDiagnosticCommand code = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
}
