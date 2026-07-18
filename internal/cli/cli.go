// Package cli parses command-line flags with environment-variable fallbacks and
// renders the help screen.
//
// It replaces jpillora/opts, which derived all of this by reflection over struct
// tags and pulled in posener/complete for a shell-completion feature this
// program never enabled. With nine flags, registering them explicitly is both
// shorter and easier to follow than the machinery that inferred them.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// App collects flag definitions so they can be parsed and rendered together.
type App struct {
	name    string
	version string
	repo    string
	fs      *flag.FlagSet
	entries []entry
	out     io.Writer

	showHelp    bool
	showVersion bool
}

// entry is a registered flag, remembered so help can be rendered in declaration
// order with its default and environment variable.
type entry struct {
	long, short string
	env         string
	help        string
	def         string
}

// ErrHelp reports that help or version was requested and already printed. The
// caller should exit successfully.
var ErrHelp = flag.ErrHelp

// New starts an app. repo is printed at the foot of the help screen.
func New(name, version, repo string) *App {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// Suppress the stdlib's own usage and error output; both are replaced below.
	fs.SetOutput(io.Discard)
	a := &App{name: name, version: version, repo: repo, fs: fs, out: os.Stdout}
	a.fs.BoolVar(&a.showHelp, "help", false, "")
	a.fs.BoolVar(&a.showHelp, "h", false, "")
	a.fs.BoolVar(&a.showVersion, "version", false, "")
	a.fs.BoolVar(&a.showVersion, "v", false, "")
	return a
}

// String registers a string flag. short and env may be empty. The current value
// of p is its default, so callers pre-populate the struct and pass it in.
func (a *App) String(p *string, long, short, env, help string) {
	a.record(long, short, env, help, *p)
	a.fs.StringVar(p, long, *p, "")
	if short != "" {
		a.fs.StringVar(p, short, *p, "")
	}
}

// Int registers an integer flag.
func (a *App) Int(p *int, long, short, env, help string) {
	a.record(long, short, env, help, strconv.Itoa(*p))
	a.fs.IntVar(p, long, *p, "")
	if short != "" {
		a.fs.IntVar(p, short, *p, "")
	}
}

// Bool registers a boolean flag. Booleans show no default: "(default false)" is
// noise on a flag whose absence means false.
func (a *App) Bool(p *bool, long, short, env, help string) {
	a.record(long, short, env, help, "")
	a.fs.BoolVar(p, long, *p, "")
	if short != "" {
		a.fs.BoolVar(p, short, *p, "")
	}
}

func (a *App) record(long, short, env, help, def string) {
	a.entries = append(a.entries, entry{long: long, short: short, env: env, help: help, def: def})
}

// Parse applies environment variables, then command-line flags, so an explicit
// flag always wins over the environment. It returns ErrHelp when help or the
// version was printed.
func (a *App) Parse(args []string) error {
	// Environment first: registration already stored each default into its
	// target, and Parse below only writes the flags actually supplied.
	for _, e := range a.entries {
		if e.env == "" {
			continue
		}
		v, ok := os.LookupEnv(e.env)
		if !ok || v == "" {
			continue
		}
		if err := a.fs.Set(e.long, v); err != nil {
			return fmt.Errorf("environment variable %s: %w", e.env, err)
		}
	}
	if err := a.fs.Parse(args); err != nil {
		a.usage(os.Stderr)
		return err
	}
	if a.showHelp {
		a.usage(a.out)
		return ErrHelp
	}
	if a.showVersion {
		fmt.Fprintln(a.out, a.version)
		return ErrHelp
	}
	if extra := a.fs.Args(); len(extra) > 0 {
		a.usage(os.Stderr)
		return fmt.Errorf("unexpected argument %q", extra[0])
	}
	return nil
}

// usage renders the help screen, aligning descriptions past the widest flag.
func (a *App) usage(w io.Writer) {
	names := make([]string, len(a.entries))
	width := 0
	for i, e := range a.entries {
		names[i] = "--" + e.long
		if e.short != "" {
			names[i] += ", -" + e.short
		}
		width = max(width, len(names[i]))
	}

	fmt.Fprintf(w, "\n  Usage: %s [options]\n\n  Options:\n", a.name)
	for i, e := range a.entries {
		fmt.Fprintf(w, "  %-*s  %s%s\n", width, names[i], e.help, suffix(e))
	}
	// Not padded: these have no description, and padding would leave trailing
	// whitespace on every help screen.
	fmt.Fprint(w, "  --help, -h\n  --version, -v\n")
	if a.repo != "" {
		fmt.Fprintf(w, "\n  Read more: %s\n", a.repo)
	}
	fmt.Fprintln(w)
}

// suffix renders the "(default X, env Y)" tail, omitting whichever half is absent.
func suffix(e entry) string {
	var parts []string
	if e.def != "" {
		parts = append(parts, "default "+e.def)
	}
	if e.env != "" {
		parts = append(parts, "env "+e.env)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
