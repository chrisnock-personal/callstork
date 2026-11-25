package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckExampleConfig(t *testing.T) {
	code, out, _ := runCLI("check", "../../config/example.yaml")
	if code != 0 || !strings.Contains(out, "0 error(s), 0 warning(s)") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

func TestCheckExitCodes(t *testing.T) {
	example, err := os.ReadFile("../../config/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// A warning only: an unused pool.
	warnOnly := write(t, string(example)+"  spare:\n    strategy: least-calls\n    members:\n      - uri: 'sip:192.0.2.30'\n")
	if code, out, _ := runCLI("check", warnOnly); code != 0 || !strings.Contains(out, `warning: pool "spare" is not used`) {
		t.Errorf("warning only: exit %d, want 0; output:\n%s", code, out)
	}
	if code, _, _ := runCLI("check", "--strict", warnOnly); code != 1 {
		t.Errorf("warning with --strict: exit %d, want 1", code)
	}
	bad := write(t, strings.Replace(string(example), "version: 1", "version: 9", 1))
	code, out, _ := runCLI("check", bad)
	if code != 1 || !strings.Contains(out, bad+":4:10: error: version 9 is not supported") {
		t.Errorf("error: exit %d, want 1 with a file:line:col message; output:\n%s", code, out)
	}
	if code, _, errOut := runCLI("check", filepath.Join(t.TempDir(), "missing.yaml")); code != 2 || !strings.Contains(errOut, "no such file") {
		t.Errorf("missing file: exit %d, stderr %q", code, errOut)
	}
	if code, _, _ := runCLI("frobnicate"); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
}

func TestCheckJSON(t *testing.T) {
	path := write(t, "version: 1\nrules: []\n")
	code, out, _ := runCLI("check", "--json", path)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	var got struct {
		File     string `json:"file"`
		Errors   int    `json:"errors"`
		Warnings int    `json:"warnings"`
		Problems []struct {
			Severity string `json:"severity"`
			Line     int    `json:"line"`
			Message  string `json:"message"`
		} `json:"problems"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.File != path || got.Errors == 0 || len(got.Problems) != got.Errors+got.Warnings {
		t.Errorf("unexpected JSON: %+v", got)
	}
}
