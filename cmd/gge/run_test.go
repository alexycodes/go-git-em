package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/alexycodes/go-git-em/internal/config"
)

// aRepo returns the shortest valid backup command: one repository from flags,
// in a temporary directory. Its input doesn't exist, so its backup fails.
func aRepo(t *testing.T) []string {
	t.Helper()
	return append([]string{"backup"}, repoFlags(t)...)
}

// aDaemon is aRepo for the cron command.
func aDaemon(t *testing.T) []string {
	t.Helper()
	return append(append([]string{"cron"}, repoFlags(t)...), "--schedule", "0 0 * * *")
}

func repoFlags(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{"--input", filepath.Join(dir, "src"), "--output", filepath.Join(dir, "out")}
}

func TestRun(t *testing.T) {
	t.Chdir(t.TempDir()) // no go-git-em.yaml there
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring of stdout; "" means stdout must be empty
		wantStderr string // substring of stderr; "" means stderr must be empty
	}{
		{"no command", nil, exitUsage, "", "Usage of go-git-em: <command> [flags]\n"},
		{"help", []string{"-h"}, exitOK, "", "\n  check-config  check the config and git"},
		{"help with two dashes", []string{"--help"}, exitOK, "", "\n  cron          back up each repository on its schedule"},
		{"unknown command", []string{"restore"}, exitUsage, "", "unknown command \"restore\"\nUsage of go-git-em:"},
		{"flag before the command", []string{"-config", "x.yaml", "backup"}, exitUsage, "", "flags go after a command: -config\n"},
		{"version", []string{"version"}, exitOK, "go-git-em ", ""},
		{"version flag", []string{"--version"}, exitOK, "go-git-em ", ""},
		{"version flag with one dash", []string{"-version"}, exitOK, "go-git-em ", ""},
		{"version has no flags", []string{"version", "-h"}, exitOK, "", "Usage of go-git-em version: print the version.\n"},
		{"version takes no argument", []string{"version", "now"}, exitUsage, "", `unexpected argument "now"`},
		{"command help", []string{"backup", "-h"}, exitOK, "", "Usage of go-git-em backup: back up every repository once, then exit.\n"},
		{"command help writes two dashes", []string{"cron", "-h"}, exitOK, "", "\n  --run-on-start\n"},
		{"command help lists the variables", []string{"check-config", "-h"}, exitOK, "", "GOGITEM_LOG_LEVEL"},
		{"unknown flag", []string{"backup", "-nope"}, exitUsage, "", "flag provided but not defined: -nope"},
		{"backup ignores schedules", []string{"backup", "--schedule", "0 0 * * *"}, exitUsage, "", "flag provided but not defined: -schedule"},
		{"only cron backs up at startup", []string{"backup", "--run-on-start=false"}, exitUsage, "", "flag provided but not defined: -run-on-start"},
		{"check-config runs no backups", []string{"check-config", "--parallel", "2"}, exitUsage, "", "flag provided but not defined: -parallel"},
		{"positional argument", []string{"backup", "now"}, exitUsage, "", `unexpected argument "now"`},
		{"bad log level", []string{"backup", "--log-level", "loud"}, exitUsage, "", `invalid value "loud" for flag -log-level: must be one of debug, info, warn, error`},
		{"bad log format", []string{"backup", "--log-format", "xml"}, exitUsage, "", `invalid value "xml" for flag -log-format: must be one of text, json`},
		{"parallel 0", []string{"cron", "--parallel", "0"}, exitUsage, "", `invalid value "0" for flag -parallel: must be at least 1`},
		{"parallel not a number", []string{"backup", "--parallel", "many"}, exitUsage, "", `invalid value "many" for flag -parallel: must be a whole number`},
		{"input without output", []string{"backup", "--input", "src"}, exitUsage, "", "--input needs --output\n"},
		{"output without input", []string{"backup", "--output", "out"}, exitUsage, "", "--output needs --input: it applies only to the --input repository\n"},
		{"schedule without input", []string{"cron", "--schedule", "0 0 * * *"}, exitUsage, "", "--schedule needs --input"},
		{"retention without input", []string{"backup", "--retention", "3"}, exitUsage, "", "--retention needs --input"},
		{"no repositories", []string{"backup"}, exitUsage, "", `level=ERROR msg="invalid config" problem="no repositories to back up`},
		{"cron needs a schedule", []string{"cron", "--input", "src", "--output", "out"}, exitUsage, "", `problem="--input repository (src): no schedule: the cron command needs one`},
		{"bad schedule", []string{"cron", "--input", "src", "--output", "out", "--schedule", "* * *"}, exitUsage, "", `schedule \"* * *\": expected exactly 5 fields, found 3`},
		{"negative retention", append(aRepo(t), "--retention", "-1"), exitUsage, "", "retention must be 0 or more, got -1"},
		{"missing config file", []string{"backup", "--config", "testdata/nope.yaml"}, exitUsage, "", `msg="can't read the config file" err="open `},
		{"flags with one dash", []string{"backup", "-input", "src", "-output", "out", "-retention", "-1"}, exitUsage, "", "retention must be 0 or more, got -1"},
		{"logs git version", aRepo(t), exitFail, "", `level=INFO msg="found git" version=`},
		{"a failed backup", aRepo(t), exitFail, "", `result=failed pruned=0 duration=`},
		{"failures are counted", aRepo(t), exitFail, "", `level=ERROR msg="some backups failed" failed=1 repositories=1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runWith(t, tt.args, nil)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			checkOutput(t, "stdout", stdout, tt.wantStdout)
			checkOutput(t, "stderr", stderr, tt.wantStderr)
		})
	}
}

func TestRunEnv(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStderr string // substring of stderr
	}{
		{
			name:       "a variable sets a flag",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_LOG_FORMAT": "json"},
			wantCode:   exitFail,
			wantStderr: `"msg":"found git"`,
		},
		{
			name:       "the command line wins, so a bad variable doesn't matter",
			args:       append(aRepo(t), "--log-format", "text"),
			env:        map[string]string{"GOGITEM_LOG_FORMAT": "xml"},
			wantCode:   exitFail,
			wantStderr: `level=INFO msg="found git"`,
		},
		{
			name:       "a repository from variables alone",
			args:       []string{"backup"},
			env:        map[string]string{"GOGITEM_INPUT": filepath.Join(dir, "src"), "GOGITEM_OUTPUT": filepath.Join(dir, "out")},
			wantCode:   exitFail,
			wantStderr: "msg=backup output=" + filepath.Join(dir, "out"),
		},
		{
			name:       "an empty variable isn't set",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_LOG_FORMAT": ""},
			wantCode:   exitFail,
			wantStderr: `level=INFO msg="found git"`,
		},
		{
			name:       "a command ignores the variables of flags it doesn't take",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_SCHEDULE": "bad", "GOGITEM_RUN_ON_START": "maybe"},
			wantCode:   exitFail,
			wantStderr: "result=failed",
		},
		{
			name:       "version ignores the variables",
			args:       []string{"version"},
			env:        map[string]string{"GOGITEM_RETENTION": "many"},
			wantCode:   exitOK,
			wantStderr: "",
		},
		{
			name:       "invalid values are reported by variable",
			args:       aDaemon(t),
			env:        map[string]string{"GOGITEM_PARALLEL": "0", "GOGITEM_RUN_ON_START": "maybe"},
			wantCode:   exitUsage,
			wantStderr: "invalid value \"0\" for GOGITEM_PARALLEL: must be at least 1\ninvalid value \"maybe\" for GOGITEM_RUN_ON_START: parse error\n",
		},
		{
			name:       "flag combinations count variables",
			args:       []string{"backup"},
			env:        map[string]string{"GOGITEM_OUTPUT": "out"},
			wantCode:   exitUsage,
			wantStderr: "--output needs --input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runWith(t, tt.args, tt.env)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, tt.wantCode, stderr)
			}
			checkOutput(t, "stderr", stderr, tt.wantStderr)
		})
	}
}

func TestRunCheck(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(cfg, []byte(`defaults:
  schedule: "0 0 * * *"
  retention: 10
repositories:
  - input: https://example.com/a.git
    output: ./a
`), 0o644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	args := []string{"check-config", "--config", cfg, "--input", "src", "--output", "out", "--retention", "0", "--ssh-key", key}
	env := map[string]string{"GOGITEM_RETENTION": "5"}
	code, stdout, stderr := runWith(t, args, env)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitOK, stderr)
	}
	if strings.Contains(stderr, "WARN") {
		t.Errorf("warnings, with a schedule for every repository:\n%s", stderr)
	}

	var report checkReport
	dec := yaml.NewDecoder(strings.NewReader(stdout))
	dec.KnownFields(true)
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("stdout isn't the YAML report: %v\n%s", err, stdout)
	}
	if report.Git == "" || report.Config != cfg {
		t.Errorf("report = git %q, config %q", report.Git, report.Config)
	}
	want := []config.Repository{
		{Input: "https://example.com/a.git", Output: filepath.Join(dir, "a"), Schedule: "0 0 * * *", Retention: 10, Timeout: 30 * time.Minute, SSHKey: key},
		// --retention 0 wins over GOGITEM_RETENTION=5 and defaults.retention.
		{Input: filepath.Join(wd, "src"), Output: filepath.Join(wd, "out"), Schedule: "0 0 * * *", Retention: 0, Timeout: 30 * time.Minute, SSHKey: key},
	}
	if !reflect.DeepEqual(report.Repositories, want) {
		t.Errorf("repositories =\n%+v\nwant\n%+v", report.Repositories, want)
	}
}

// check-config passes a repository without a schedule, which only the cron
// command needs, with a warning.
func TestRunCheckNoSchedule(t *testing.T) {
	code, stdout, stderr := runWith(t, []string{"check-config", "--input", "https://example.com/a.git", "--output", "out"}, nil)
	if code != exitOK || !strings.Contains(stdout, "  - input: https://example.com/a.git\n") {
		t.Errorf("exit code %d, stdout:\n%s", code, stdout)
	}
	checkOutput(t, "stderr", stderr, `level=WARN msg="no schedule: the cron command needs one" output=`)
}

// A go-git-em.yaml that can't be read is reported, not taken for no file.
func TestRunDefaultConfigUnreadable(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Symlink("nowhere.yaml", config.DefaultFile); err != nil {
		t.Skipf("can't make a symlink: %v", err)
	}
	code, _, stderr := runWith(t, []string{"check-config"}, nil)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	checkOutput(t, "stderr", stderr, `msg="can't read the config file" err="open `)
}

func TestRunConfigProblems(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(cfg, []byte(`repositories:
  - input: https://example.com/a.git
    retention: -1
  - input: -x
    output: b
`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runWith(t, []string{"backup", "--config", cfg}, nil)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	// One log entry per problem, and nothing else: no git check, no backups.
	want := []string{
		`level=ERROR msg="invalid config" problem="repository 1 (https://example.com/a.git): output is required"`,
		`level=ERROR msg="invalid config" problem="repository 1 (https://example.com/a.git): retention must be 0 or more, got -1"`,
		`level=ERROR msg="invalid config" problem="repository 2 (-x): input starts with \"-\", which git would read as an option"`,
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("stderr has %d lines, want %d:\n%s", len(lines), len(want), stderr)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, want[i]) {
			t.Errorf("stderr line %d = %q, want it to end with %q", i+1, line, want[i])
		}
	}
}

func TestRunYAMLProblems(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(cfg, []byte("repositories:\n  - input: a\n    output: b\n    retension: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runWith(t, []string{"backup", "--config", cfg}, nil)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	// Quoting doubles the backslashes of a Windows path.
	checkOutput(t, "stderr", stderr, `msg="invalid config" problem=`+strconv.Quote(cfg+`: line 4: unknown key "retension"`))
}

func TestRunWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{aRepo(t), append([]string{"check-config"}, repoFlags(t)...)} {
		code, stdout, stderr := runWith(t, args, nil)
		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
		checkOutput(t, "stdout", stdout, "")
		checkOutput(t, "stderr", stderr, `msg="go-git-em needs git on PATH"`)
	}

	// version doesn't need git.
	if code, _, _ := runWith(t, []string{"version"}, nil); code != exitOK {
		t.Errorf("version without git: exit code = %d, want %d", code, exitOK)
	}
}

func TestEnvName(t *testing.T) {
	for flag, want := range map[string]string{
		"config":       "GOGITEM_CONFIG",
		"log-level":    "GOGITEM_LOG_LEVEL",
		"run-on-start": "GOGITEM_RUN_ON_START",
		"ssh-key":      "GOGITEM_SSH_KEY",
	} {
		if got := envName(flag); got != want {
			t.Errorf("envName(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestSetFromEnv(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	a := fs.String("a", "default", "")
	b := fs.String("b", "default", "")
	n := fs.Int("n", 0, "")
	if err := fs.Parse([]string{"-a", "from-flag"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GOGITEM_A": "from-env", "GOGITEM_B": "from-env", "GOGITEM_N": "7", "GOGITEM_OTHER": "x"}
	if err := setFromEnv(fs, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if *a != "from-flag" || *b != "from-env" || *n != 7 {
		t.Errorf("a, b, n = %q, %q, %d; want from-flag, from-env, 7", *a, *b, *n)
	}
	// Variables count as set, for the flag combination checks.
	var set []string
	fs.Visit(func(f *flag.Flag) { set = append(set, f.Name) })
	if want := []string{"a", "b", "n"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set flags = %v, want %v", set, want)
	}
}

// ~ expands to $HOME, or on Windows without it, to %USERPROFILE%.
func TestRunHome(t *testing.T) {
	home, profile := t.TempDir(), t.TempDir()
	args := []string{"check-config", "--input", "https://example.com/a.git", "--output", "~/backups"}
	type homeTest struct {
		env  map[string]string
		want string // "" for none
	}
	tests := []homeTest{{map[string]string{"HOME": home, "USERPROFILE": profile}, home}}
	if runtime.GOOS == "windows" {
		tests = append(tests, homeTest{map[string]string{"USERPROFILE": profile}, profile})
	} else {
		tests = append(tests, homeTest{map[string]string{"USERPROFILE": profile}, ""})
	}
	for _, tt := range tests {
		code, stdout, stderr := runWith(t, args, tt.env)
		if tt.want == "" {
			if code != exitUsage || !strings.Contains(stderr, "HOME isn't set") {
				t.Errorf("with %v: exit code %d, stderr:\n%s\nwant HOME's absence reported", tt.env, code, stderr)
			}
			continue
		}
		if code != exitOK || !strings.Contains(stdout, "output: "+filepath.Join(tt.want, "backups")+"\n") {
			t.Errorf("with %v: exit code %d, stdout:\n%s\nstderr:\n%s", tt.env, code, stdout, stderr)
		}
	}
}

// runWith calls run with args and the given environment, and returns its exit
// code and output.
func runWith(t *testing.T, args []string, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string { return env[k] }
	code = run(t.Context(), args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func checkOutput(t *testing.T, name, got, want string) {
	t.Helper()
	if want == "" && got != "" {
		t.Errorf("%s = %q, want it empty", name, got)
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", name, got, want)
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, slog.LevelWarn, "json", nil)
	logger.Info("hidden")
	logger.Warn("shown", "output", "/backups/x")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("want one JSON log line, got %q: %v", buf.String(), err)
	}
	if entry["level"] != "WARN" || entry["msg"] != "shown" || entry["output"] != "/backups/x" {
		t.Errorf("log entry = %v", entry)
	}
}

// Ctrl-C during startup isn't a missing git.
func TestRunInterruptedAtStart(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantLog  string
	}{
		{"backup", aRepo(t), exitFail, `level=ERROR msg=interrupted`},
		{"check-config", append([]string{"check-config"}, repoFlags(t)...), exitFail, `level=ERROR msg=interrupted`},
		// Being stopped is how a daemon ends, even this early.
		{"cron", aDaemon(t), exitOK, `level=INFO msg=stopped`},
	}
	for _, tt := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stdout, stderr bytes.Buffer
		code := run(ctx, tt.args, func(string) string { return "" }, &stdout, &stderr)
		if code != tt.wantCode || !strings.Contains(stderr.String(), tt.wantLog) || strings.Contains(stderr.String(), "needs git") {
			t.Errorf("%s: exit code %d, stderr:\n%s", tt.name, code, stderr.String())
		}
	}
}

// The cron command starts the daemon: it backs up at once, and returns exitOK
// when stopped.
func TestRunDaemonMode(t *testing.T) {
	code, stderr := runUntil(t, "msg=backup ", aDaemon(t))
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	for _, want := range []string{`msg=scheduled `, ` schedule="0 0 * * *" next=`, `msg=backup `, ` result=failed `, "msg=stopping\n", "msg=stopped\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// With --run-on-start=false, the daemon only waits for the schedules. Stopped
// as soon as it starts, it logs no backup, not even a skipped one at debug
// level.
func TestRunDaemonNoRunOnStart(t *testing.T) {
	code, stderr := runUntil(t, "msg=scheduled ", append(aDaemon(t), "--run-on-start=false", "--log-level", "debug"))
	if code != exitOK || strings.Contains(stderr, "msg=backup") || !strings.Contains(stderr, "msg=stopped\n") {
		t.Errorf("exit code %d, stderr:\n%s", code, stderr)
	}
}

// runUntil calls run, and stops it, as a signal would, once it logs a line
// that contains marker. Its deadline only bounds a failure.
func runUntil(t *testing.T, marker string, args []string) (code int, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var buf bytes.Buffer
	seen := false
	logs := writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte(marker)) {
			seen = true
			cancel()
		}
		return buf.Write(p)
	})
	code = run(ctx, args, func(string) string { return "" }, io.Discard, logs)
	if !seen {
		t.Errorf("run never logged %q, and its deadline stopped it:\n%s", marker, buf.String())
	}
	return code, buf.String()
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
