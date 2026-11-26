// Command callstork is the CallStork inbound SIP dispatcher. Phase 1 starts
// with `check`, which validates a config file without sending any SIP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/chrisnock-personal/callstork/internal/config"
	"github.com/chrisnock-personal/callstork/internal/proxy"
	"github.com/emiago/sipgo/sip"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `Usage:
  callstork check [--json] [--strict] <config.yaml>
  callstork run [--trace] <config.yaml>
  callstork version

check validates a config file and reports every problem as
file:line:column: severity: message. Exit status: 0 when there are no errors
(warnings allowed), 1 when there are errors (or warnings with --strict),
2 when the file can't be read or the command is wrong.

run starts the proxy. It refuses a config with errors. SIGHUP reloads the
config (rules, peers, pools) without dropping calls; SIGINT or SIGTERM stops
it. --trace logs every SIP message sent and received.
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
	case "run":
		return serve(args[1:], stderr)
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

// load reads, checks and compiles a config, printing any problems.
func load(path string, stderr io.Writer) (*proxy.Runtime, error) {
	cfg, diags, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		diags = append(diags, config.Check(cfg)...)
	}
	errs := 0
	for _, d := range diags {
		fmt.Fprintf(stderr, "%s:%s\n", path, d)
		if d.Severity == config.Error {
			errs++
		}
	}
	if errs > 0 {
		return nil, fmt.Errorf("%s has %d error(s)", path, errs)
	}
	return proxy.Compile(cfg)
}

func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trace := fs.Bool("trace", false, "log every SIP message")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := fs.Arg(0)
	level := slog.LevelInfo
	if *trace {
		level = slog.LevelDebug
		sip.SIPDebug = true
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	sipLevel := slog.LevelWarn
	if *trace {
		sipLevel = slog.LevelDebug
	}
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: sipLevel})))

	rt, err := load(path, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "callstork: %v\n", err)
		return 1
	}
	p, err := proxy.New(rt, log)
	if err != nil {
		fmt.Fprintf(stderr, "callstork: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			rt, err := load(path, stderr)
			if err == nil {
				err = p.Reload(rt)
			}
			if err != nil {
				log.Error("reload refused; still running the previous config", "error", err)
				continue
			}
			log.Info("config reloaded", "file", path)
		}
	}()
	if err := p.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "callstork: %v\n", err)
		return 1
	}
	return 0
}
