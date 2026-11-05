package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// Exit codes.
const (
	exitOK    = 0 // success
	exitFail  = 1 // at least one backup failed, or a runtime error
	exitUsage = 2 // usage or configuration error, or git is missing
)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// run is the whole program. It takes its arguments, environment and output
// streams as parameters, so tests can call it directly.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	_ = getenv // read by the environment overlay (M2)

	fs := flag.NewFlagSet("go-git-em", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	logLevel := &choiceFlag{value: "info", choices: []string{"debug", "info", "warn", "error"}}
	fs.Var(logLevel, "log-level", "log level: "+logLevel.list())
	logFormat := &choiceFlag{value: "text", choices: []string{"text", "json"}}
	fs.Var(logFormat, "log-format", "log format: "+logFormat.list())

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage // the flag package has already printed the error and usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return exitUsage
	}

	if *showVersion {
		fmt.Fprintln(stdout, versionString())
		return exitOK
	}

	logger := newLogger(stderr, logLevels[logLevel.value], logFormat.value)

	git := &gitcmd.Runner{}
	gitVersion, err := git.Version(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "go-git-em needs git on PATH", "err", err)
		return exitUsage
	}
	logger.InfoContext(ctx, "found git", "version", gitVersion)

	logger.ErrorContext(ctx, "backups are not implemented yet")
	return exitFail
}

// newLogger returns a logger that writes to w in the given format, "text" or "json".
func newLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// versionString describes this build: its version, commit, build date and Go version.
func versionString() string {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	if v == "" {
		v = "dev"
	}

	var details []string
	if c != "" {
		details = append(details, "commit "+c)
	}
	if d != "" {
		details = append(details, "built "+d)
	}
	details = append(details, runtime.Version())
	return fmt.Sprintf("go-git-em %s (%s)", v, strings.Join(details, ", "))
}

// choiceFlag is a string flag that accepts only the listed values.
type choiceFlag struct {
	value   string
	choices []string
}

func (f *choiceFlag) String() string { return f.value }

func (f *choiceFlag) Set(s string) error {
	if !slices.Contains(f.choices, s) {
		return fmt.Errorf("must be one of %s", f.list())
	}
	f.value = s
	return nil
}

func (f *choiceFlag) list() string { return strings.Join(f.choices, ", ") }
