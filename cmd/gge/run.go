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
	"regexp"
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

// Exit codes. A daemon stopped by a signal exits exitOK. exitFail also covers
// a backup or check-config interrupted at startup, and runtime errors.
const (
	exitOK    = 0 // success
	exitFail  = 1 // with backup, a backup failed, was interrupted or found its output locked
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

// A command is one of go-git-em's commands.
type command struct{ name, summary string }

// commands are go-git-em's commands, in the order usage lists them.
var commands = []command{
	{"backup", "back up every repository once, then exit"},
	{"cron", "back up each repository on its schedule, until stopped"},
	{"check-config", "check the config and git, print the repositories to back up, then exit"},
	{"version", "print the version"},
}

// run is the whole program. It takes its arguments, environment and output
// streams as parameters, so tests can call it directly.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	name, args := args[0], args[1:]
	switch name {
	case "-h", "-help", "--h", "--help":
		usage(stderr)
		return exitOK
	case "-version", "--version":
		name = "version"
	}
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == name })
	if i < 0 {
		if strings.HasPrefix(name, "-") {
			fmt.Fprintf(stderr, "flags go after a command: %s\n", name)
		} else {
			fmt.Fprintf(stderr, "unknown command %q\n", name)
		}
		usage(stderr)
		return exitUsage
	}

	f := newFlags(commands[i], stderr)
	if err := f.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage // the flag package has already printed the error and usage
	}
	if f.fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", f.fs.Arg(0))
		f.fs.Usage()
		return exitUsage
	}

	if name == "version" {
		fmt.Fprintln(stdout, versionString())
		return exitOK
	}

	if err := setFromEnv(f.fs, getenv); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	set := make(map[string]bool) // flags set on the command line or by their variable
	f.fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if err := checkFlagCombinations(set); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	logger := newLogger(stderr, logLevels[f.logLevel.value], f.logFormat.value, nil)

	wd, err := os.Getwd()
	if err != nil {
		logger.ErrorContext(ctx, "can't get the current directory", "err", err)
		return exitFail
	}
	home := getenv("HOME")
	if home == "" && runtime.GOOS == "windows" {
		home = getenv("USERPROFILE")
	}
	opts := config.Options{Cron: name == "cron", WorkDir: wd, Home: home}
	if set["ssh-key"] {
		opts.SSHKey = f.sshKey
	}
	if set["input"] {
		opts.Input = &config.Repo{Input: f.input, Output: f.output}
		if set["schedule"] {
			opts.Input.Schedule = f.schedule
		}
		if set["retention"] {
			opts.Input.Retention = &f.retention
		}
	}
	var file *config.File
	if path, byDefault := configPath(f.config, set); path != "" {
		if file, err = config.Load(path); err != nil {
			logConfigError(ctx, logger, err)
			return exitUsage
		}
		if byDefault {
			logger.InfoContext(ctx, "using the config file in the current directory", "config", file.Path)
		}
	}
	repos, err := config.Resolve(file, opts)
	if err != nil {
		logConfigError(ctx, logger, err)
		return exitUsage
	}
	logger = newLogger(stderr, logLevels[f.logLevel.value], f.logFormat.value, credentials(repos))
	if name == "check-config" {
		for _, repo := range repos {
			if repo.Schedule == "" {
				repoLogger(logger, repo).WarnContext(ctx, "no schedule: the cron command needs one")
			}
		}
	}

	git := &gitcmd.Runner{}
	gitVersion, err := git.Version(ctx)
	if ctx.Err() != nil {
		if name == "cron" {
			return stopped(ctx, logger)
		}
		logger.ErrorContext(ctx, "interrupted")
		return exitFail
	}
	if err != nil {
		logger.ErrorContext(ctx, "go-git-em needs git on PATH", "err", err)
		return exitUsage
	}
	logger.InfoContext(ctx, "found git", "version", gitVersion)
	if !backup.CanLock {
		logger.WarnContext(ctx, "this system can't lock outputs: two go-git-em processes could use one at once")
	}

	runBackup := func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		return (&backup.Job{Repo: repo, Git: git, Log: log}).Run(ctx)
	}
	switch name {
	case "check-config":
		return printReport(ctx, logger, stdout, checkReport{Git: gitVersion, Config: filePath(file), Repositories: repos})
	case "backup":
		return runOnce(ctx, logger, repos, f.parallel.value, runBackup)
	default:
		return runDaemon(ctx, logger, repos, f.parallel.value, f.runOnStart, runBackup)
	}
}

// flags holds a command's flags. A command defines only the flags it takes:
// the others keep their defaults.
type flags struct {
	fs                                      *flag.FlagSet
	config, input, output, schedule, sshKey string
	retention                               int
	parallel                                minIntFlag
	runOnStart                              bool
	logLevel, logFormat                     choiceFlag
}

// newFlags defines cmd's flags, which print their errors and usage to w.
func newFlags(cmd command, w io.Writer) *flags {
	f := &flags{
		fs:         flag.NewFlagSet("go-git-em "+cmd.name, flag.ContinueOnError),
		parallel:   minIntFlag{value: 4, min: 1},
		runOnStart: true,
		logLevel:   choiceFlag{value: "info", choices: []string{"debug", "info", "warn", "error"}},
		logFormat:  choiceFlag{value: "text", choices: []string{"text", "json"}},
	}
	fs := f.fs
	fs.SetOutput(w)
	fs.Usage = func() { commandUsage(fs, cmd) }
	if cmd.name == "version" {
		return f
	}
	fs.StringVar(&f.config, "config", "", "path to the YAML config file (default: "+config.DefaultFile+" in the current directory, if it's there and --input isn't given)")
	fs.StringVar(&f.input, "input", "", "add a repository: its URL or local path (needs --output)")
	fs.StringVar(&f.output, "output", "", "output directory for the --input repository")
	if cmd.name != "backup" { // backup ignores schedules
		fs.StringVar(&f.schedule, "schedule", "", "cron schedule for the --input repository (default: defaults.schedule)")
	}
	fs.IntVar(&f.retention, "retention", 0, "snapshots to keep for the --input repository, 0 for all (default: defaults.retention, else all)")
	fs.StringVar(&f.sshKey, "ssh-key", "", "private key file for ssh inputs; replaces defaults.ssh_key")
	if cmd.name != "check-config" {
		fs.Var(&f.parallel, "parallel", "maximum number of backups running at once")
	}
	if cmd.name == "cron" {
		fs.BoolVar(&f.runOnStart, "run-on-start", true, "back up every repository at startup")
	}
	fs.Var(&f.logLevel, "log-level", "log level: "+f.logLevel.list())
	fs.Var(&f.logFormat, "log-format", "log format: "+f.logFormat.list())
	return f
}

// configPath returns the config file to read, or "": --config, else the
// default file in the current directory if it's there and --input isn't set,
// which byDefault reports. An empty --config counts as unset.
func configPath(flagValue string, set map[string]bool) (path string, byDefault bool) {
	if flagValue != "" || set["input"] {
		return flagValue, false
	}
	// Lstat, so that a broken symlink is reported rather than taken for no file.
	if _, err := os.Lstat(config.DefaultFile); errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	return config.DefaultFile, true // config.Load reports any other error
}

// checkReport is what check-config prints: the resolved configuration, as
// YAML, with credentials in URLs hidden.
type checkReport struct {
	Git          string              `yaml:"git"`
	Config       string              `yaml:"config,omitempty"`
	Repositories []config.Repository `yaml:"repositories"`
}

// printReport prints report to w, with credentials hidden.
func printReport(ctx context.Context, logger *slog.Logger, w io.Writer, report checkReport) int {
	report.Repositories = slices.Clone(report.Repositories)
	for i := range report.Repositories {
		report.Repositories[i].Input = config.Redact(report.Repositories[i].Input)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := errors.Join(enc.Encode(report), enc.Close()); err != nil {
		logger.ErrorContext(ctx, "can't print the report", "err", err)
		return exitFail
	}
	if _, err := out.WriteTo(w); err != nil {
		logger.ErrorContext(ctx, "can't print the report", "err", err)
		return exitFail
	}
	return exitOK
}

// filePath returns the config file's path, or "" if there is none.
func filePath(file *config.File) string {
	if file == nil {
		return ""
	}
	return file.Path
}

// usage prints the commands.
func usage(w io.Writer) {
	fmt.Fprint(w, "Usage of go-git-em: <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-13s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "\nA command's flags come after it, and -h lists them.")
}

// flagLine matches the start of each flag in PrintDefaults' output.
var flagLine = regexp.MustCompile(`(?m)^  -`)

// commandUsage prints cmd's flags, and how to set them with environment
// variables.
func commandUsage(fs *flag.FlagSet, cmd command) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage of go-git-em %s: %s.\n", cmd.name, cmd.summary)
	var defaults strings.Builder
	fs.SetOutput(&defaults)
	fs.PrintDefaults()
	fs.SetOutput(w)
	if defaults.Len() == 0 {
		return // no flags
	}
	// The docs write flags with two dashes, which the flag package also takes.
	fmt.Fprintf(w, "\n%s\nEach flag can also be set with an environment variable: %s, then the\n"+
		"flag's name in capitals with _ for -, such as %sLOG_LEVEL. A flag on the\n"+
		"command line wins over its variable.\n", flagLine.ReplaceAllString(defaults.String(), "  --"), envPrefix, envPrefix)
}

// envName returns the environment variable of a flag: "log-level" → "GOGITEM_LOG_LEVEL".
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// setFromEnv sets each flag not given on the command line from its environment
// variable, if that isn't empty, checked as on the command line. Variables for
// other commands' flags are ignored.
func setFromEnv(fs *flag.FlagSet, getenv func(string) string) error {
	onCommandLine := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { onCommandLine[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if onCommandLine[f.Name] {
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

// checkFlagCombinations checks that --input and --output come together, and
// that --schedule and --retention come with them.
func checkFlagCombinations(set map[string]bool) error {
	var errs []error
	if set["input"] && !set["output"] {
		errs = append(errs, errors.New("--input needs --output"))
	}
	for _, name := range []string{"output", "schedule", "retention"} {
		if set[name] && !set["input"] {
			errs = append(errs, fmt.Errorf("--%s needs --input: it applies only to the --input repository", name))
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
