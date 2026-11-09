package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
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

// envPrefix starts the environment variable of each flag (envName).
const envPrefix = "GOGITEM_"

// Flags that have no environment variable: they ask for a one-off action.
var noEnv = []string{"check", "version"}

// run is the whole program. It takes its arguments, environment and output
// streams as parameters, so tests can call it directly.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("go-git-em", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(fs) }
	configPath := fs.String("config", "", "path to the YAML config file")
	once := fs.Bool("once", false, "back up every repository once, then exit")
	input := fs.String("input", "", "add a repository: its URL or local path (needs -output)")
	output := fs.String("output", "", "output directory for the -input repository")
	schedule := fs.String("schedule", "", "cron schedule for the -input repository (default: defaults.schedule)")
	retention := fs.Int("retention", 0, "snapshots to keep for the -input repository, 0 for all (default: defaults.retention, else all)")
	sshKey := fs.String("ssh-key", "", "private key file for ssh inputs; replaces defaults.ssh_key")
	parallel := &minIntFlag{value: 4, min: 1}
	fs.Var(parallel, "parallel", "maximum number of backups running at once")
	runOnStart := fs.Bool("run-on-start", true, "daemon mode: back up every repository at startup")
	logLevel := &choiceFlag{value: "info", choices: []string{"debug", "info", "warn", "error"}}
	fs.Var(logLevel, "log-level", "log level: "+logLevel.list())
	logFormat := &choiceFlag{value: "text", choices: []string{"text", "json"}}
	fs.Var(logFormat, "log-format", "log format: "+logFormat.list())
	check := fs.Bool("check", false, "check the config and git, print the resolved repositories, then exit")
	showVersion := fs.Bool("version", false, "print the version and exit")

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

	if err := setFromEnv(fs, getenv); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	set := make(map[string]bool) // flags set on the command line or by their variable
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := checkFlagCombinations(set); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	logger := newLogger(stderr, logLevels[logLevel.value], logFormat.value, nil)

	wd, err := os.Getwd()
	if err != nil {
		logger.ErrorContext(ctx, "can't get the current directory", "err", err)
		return exitFail
	}
	opts := config.Options{Once: *once, WorkDir: wd, Home: getenv("HOME")}
	if set["ssh-key"] {
		opts.SSHKey = *sshKey
	}
	if set["input"] {
		opts.Input = &config.Repo{Input: *input, Output: *output}
		if set["schedule"] {
			opts.Input.Schedule = *schedule
		}
		if set["retention"] {
			opts.Input.Retention = retention
		}
	}
	var file *config.File
	if *configPath != "" {
		if file, err = config.Load(*configPath); err != nil {
			logConfigError(ctx, logger, err)
			return exitUsage
		}
	}
	repos, err := config.Resolve(file, opts)
	if err != nil {
		logConfigError(ctx, logger, err)
		return exitUsage
	}
	logger = newLogger(stderr, logLevels[logLevel.value], logFormat.value, credentials(repos))

	git := &gitcmd.Runner{}
	gitVersion, err := git.Version(ctx)
	if ctx.Err() != nil {
		logger.ErrorContext(ctx, "interrupted")
		return exitFail
	}
	if err != nil {
		logger.ErrorContext(ctx, "go-git-em needs git on PATH", "err", err)
		return exitUsage
	}
	logger.InfoContext(ctx, "found git", "version", gitVersion)

	if *check {
		report := checkReport{Git: gitVersion, Mode: "daemon", Parallel: parallel.value, Repositories: slices.Clone(repos)}
		for i := range report.Repositories {
			report.Repositories[i].Input = config.Redact(report.Repositories[i].Input)
		}
		if *once {
			report.Mode = "once"
		} else {
			report.RunOnStart = runOnStart
		}
		if file != nil {
			report.Config = file.Path
		}
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := errors.Join(enc.Encode(report), enc.Close()); err != nil {
			logger.ErrorContext(ctx, "can't print the report", "err", err)
			return exitFail
		}
		if _, err := out.WriteTo(stdout); err != nil {
			logger.ErrorContext(ctx, "can't print the report", "err", err)
			return exitFail
		}
		return exitOK
	}

	if !*once {
		logger.ErrorContext(ctx, "daemon mode isn't implemented yet: run with -once")
		return exitFail
	}
	return runOnce(ctx, logger, repos, parallel.value, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		return (&backup.Job{Repo: repo, Git: git, Log: log}).Run(ctx)
	})
}

// checkReport is what -check prints: the resolved configuration, as YAML, with
// credentials in URLs hidden.
type checkReport struct {
	Git          string              `yaml:"git"`
	Config       string              `yaml:"config,omitempty"`
	Mode         string              `yaml:"mode"`
	Parallel     int                 `yaml:"parallel"`
	RunOnStart   *bool               `yaml:"run_on_start,omitempty"` // daemon mode only
	Repositories []config.Repository `yaml:"repositories"`
}

// usage prints the flags, and how to set them with environment variables.
func usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintln(w, "Usage of go-git-em:")
	fs.PrintDefaults()
	fmt.Fprintf(w, "\nEvery flag except -check and -version can also be set with an environment\n"+
		"variable: %s, then the flag's name in capitals with _ for -, such as\n"+
		"%sLOG_LEVEL. A flag on the command line wins over its variable.\n", envPrefix, envPrefix)
}

// envName returns the environment variable of a flag: "log-level" → "GOGITEM_LOG_LEVEL".
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// setFromEnv sets each flag that wasn't given on the command line from its
// environment variable, if that is set and not empty. Values go through the
// flag's own Set, so they're parsed and checked as on the command line. The
// error names the variable of every invalid value.
func setFromEnv(fs *flag.FlagSet, getenv func(string) string) error {
	onCommandLine := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { onCommandLine[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if onCommandLine[f.Name] || slices.Contains(noEnv, f.Name) {
			return
		}
		name := envName(f.Name)
		value := getenv(name)
		if value == "" {
			return
		}
		if err := fs.Set(f.Name, value); err != nil {
			errs = append(errs, fmt.Errorf("invalid value %q for %s: %w", value, name, err))
		}
	})
	return errors.Join(errs...)
}

// checkFlagCombinations checks that -output, -schedule and -retention, which
// describe the -input repository, come with -input, and that -input comes
// with -output.
func checkFlagCombinations(set map[string]bool) error {
	var errs []error
	if set["input"] && !set["output"] {
		errs = append(errs, errors.New("-input needs -output"))
	}
	for _, name := range []string{"output", "schedule", "retention"} {
		if set[name] && !set["input"] {
			errs = append(errs, fmt.Errorf("-%s needs -input: it applies only to the -input repository", name))
		}
	}
	return errors.Join(errs...)
}

// logConfigError logs a configuration error, one entry per problem.
func logConfigError(ctx context.Context, logger *slog.Logger, err error) {
	var cfgErr *config.Error
	if !errors.As(err, &cfgErr) {
		logger.ErrorContext(ctx, "can't read the config file", "err", err)
		return
	}
	for _, problem := range cfgErr.Problems {
		logger.ErrorContext(ctx, "invalid config", "problem", problem)
	}
}

// newLogger returns a logger that writes to w in the given format, "text" or
// "json". If hide isn't nil, it rewrites every message, and every string or
// error value, with it: log anything that could hold an input as one of those.
func newLogger(w io.Writer, level slog.Level, format string, hide *strings.Replacer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if hide != nil {
		opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			switch v := a.Value.Any().(type) {
			case string:
				a.Value = slog.StringValue(hide.Replace(v))
			case error:
				a.Value = slog.StringValue(hide.Replace(v.Error()))
			}
			return a
		}
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// credentials returns a replacer that hides the credentials of every input
// in log lines, or nil if no input has any. git's errors quote their
// arguments, and its stderr may quote an input too.
func credentials(repos []config.Repository) *strings.Replacer {
	var pairs []string
	for _, repo := range repos {
		pairs = append(pairs, config.CredentialReplacements(repo.Input)...)
	}
	if pairs == nil {
		return nil
	}
	return strings.NewReplacer(pairs...)
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

// minIntFlag is an int flag with a minimum value.
type minIntFlag struct {
	value int
	min   int
}

func (f *minIntFlag) String() string { return strconv.Itoa(f.value) }

func (f *minIntFlag) Set(s string) error {
	v, err := strconv.Atoi(s)
	if err != nil {
		return errors.New("must be a whole number")
	}
	if v < f.min {
		return fmt.Errorf("must be at least %d", f.min)
	}
	f.value = v
	return nil
}
