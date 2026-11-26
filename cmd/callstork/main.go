// Command callstork is the CallStork inbound SIP dispatcher. Phase 1 starts
// with `check`, which validates a config file without sending any SIP.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/chrisnock-personal/callstork/internal/config"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `Usage:
  callstork check [--json] [--strict] <config.yaml>
  callstork version

check validates a config file and reports every problem as
file:line:column: severity: message. Exit status: 0 when there are no errors
(warnings allowed), 1 when there are errors (or warnings with --strict),
2 when the file can't be read or the command is wrong.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "check":
		return check(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "callstork", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "callstork: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func check(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the problems as JSON")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := fs.Arg(0)
	cfg, diags, err := config.Load(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			fmt.Fprintf(stderr, "callstork: %s: %v\n", path, pe.Err)
		} else {
			fmt.Fprintf(stderr, "callstork: %v\n", err)
		}
		return 2
	}
	if cfg != nil {
		diags = append(diags, config.Check(cfg)...)
	}

	errs, warns := 0, 0
	for _, d := range diags {
		if d.Severity == config.Error {
			errs++
		} else {
			warns++
		}
	}
	if *asJSON {
		out := struct {
			File     string        `json:"file"`
			Errors   int           `json:"errors"`
			Warnings int           `json:"warnings"`
			Problems []config.Diag `json:"problems"`
		}{path, errs, warns, diags}
		if out.Problems == nil {
			out.Problems = []config.Diag{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		for _, d := range diags {
			fmt.Fprintf(stdout, "%s:%s\n", path, d)
		}
		fmt.Fprintf(stdout, "%s: %d error(s), %d warning(s)\n", path, errs, warns)
	}
	if errs > 0 || (*strict && warns > 0) {
		return 1
	}
	return 0
}
