package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/alexycodes/go-git-em/internal/config"
)

// aRepo returns the shortest valid configuration: one repository from flags,
// in -once mode, in a temporary directory. Its input doesn't exist, so its
// backup fails.
func aRepo(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{"-once", "-input", filepath.Join(dir, "src"), "-output", filepath.Join(dir, "out")}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring of stdout; "" means stdout must be empty
		wantStderr string // substring of stderr; "" means stderr must be empty
	}{
		{"version", []string{"-version"}, exitOK, "go-git-em ", ""},
		{"version with two dashes", []string{"--version"}, exitOK, "go-git-em ", ""},
		{"help", []string{"-h"}, exitOK, "", "Usage of go-git-em"},
		{"help lists the variables", []string{"-h"}, exitOK, "", "GOGITEM_LOG_LEVEL"},
		{"unknown flag", []string{"-nope"}, exitUsage, "", "flag provided but not defined: -nope"},
		{"positional argument", []string{"backup"}, exitUsage, "", `unexpected argument "backup"`},
		{"bad log level", []string{"-log-level", "loud"}, exitUsage, "", `invalid value "loud" for flag -log-level: must be one of debug, info, warn, error`},
		{"bad log format", []string{"-log-format", "xml"}, exitUsage, "", `invalid value "xml" for flag -log-format: must be one of text, json`},
		{"parallel 0", []string{"-parallel", "0"}, exitUsage, "", `invalid value "0" for flag -parallel: must be at least 1`},
		{"parallel not a number", []string{"-parallel", "many"}, exitUsage, "", `invalid value "many" for flag -parallel: must be a whole number`},
		{"input without output", []string{"-input", "src"}, exitUsage, "", "-input needs -output\n"},
		{"output without input", []string{"-output", "out"}, exitUsage, "", "-output needs -input: it applies only to the -input repository\n"},
		{"schedule without input", []string{"-schedule", "@daily"}, exitUsage, "", "-schedule needs -input"},
		{"retention without input", []string{"-retention", "3"}, exitUsage, "", "-retention needs -input"},
		{"no repositories", nil, exitUsage, "", `level=ERROR msg="invalid config" problem="no repositories to back up`},
		{"daemon mode needs a schedule", []string{"-input", "src", "-output", "out"}, exitUsage, "", `problem="-input repository (src): no schedule: daemon mode needs one`},
		{"bad schedule", []string{"-input", "src", "-output", "out", "-schedule", "* * *"}, exitUsage, "", `schedule \"* * *\": expected exactly 5 fields, found 3`},
		{"negative retention", append([]string{"-retention", "-1"}, aRepo(t)...), exitUsage, "", "retention must be 0 or more, got -1"},
		{"missing config file", []string{"-once", "-config", "testdata/nope.yaml"}, exitUsage, "", `msg="can't read the config file" err="open `},
		{"logs git version", aRepo(t), exitFail, "", `level=INFO msg="found git" version=`},
		{"a failed backup", aRepo(t), exitFail, "", `result=failed pruned=0 duration=`},
		{"failures are counted", aRepo(t), exitFail, "", `level=ERROR msg="some backups failed" failed=1 repositories=1`},
		{"no daemon yet", append(aRepo(t)[1:], "-schedule", "@daily"), exitFail, "", `level=ERROR msg="daemon mode isn't implemented yet: run with -once"`},
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
			args:       append([]string{"-log-format", "text"}, aRepo(t)...),
			env:        map[string]string{"GOGITEM_LOG_FORMAT": "xml"},
			wantCode:   exitFail,
			wantStderr: `level=INFO msg="found git"`,
		},
		{
			name:       "a repository from variables alone",
			env:        map[string]string{"GOGITEM_ONCE": "1", "GOGITEM_INPUT": filepath.Join(dir, "src"), "GOGITEM_OUTPUT": filepath.Join(dir, "out")},
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
			name:       "-check has no variable",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_CHECK": "true"},
			wantCode:   exitFail,
			wantStderr: "result=failed",
		},
		{
			name:       "-version has no variable",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_VERSION": "true"},
			wantCode:   exitFail,
			wantStderr: "result=failed",
		},
		{
			name:       "-version ignores the variables",
			args:       []string{"-version"},
			env:        map[string]string{"GOGITEM_RETENTION": "many"},
			wantCode:   exitOK,
			wantStderr: "",
		},
		{
			name:       "invalid values are reported by variable",
			args:       aRepo(t),
			env:        map[string]string{"GOGITEM_PARALLEL": "0", "GOGITEM_RUN_ON_START": "maybe"},
			wantCode:   exitUsage,
			wantStderr: "invalid value \"0\" for GOGITEM_PARALLEL: must be at least 1\ninvalid value \"maybe\" for GOGITEM_RUN_ON_START: parse error\n",
		},
		{
			name:       "flag combinations count variables",
			env:        map[string]string{"GOGITEM_OUTPUT": "out"},
			wantCode:   exitUsage,
			wantStderr: "-output needs -input",
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
  schedule: "@daily"
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

	args := []string{"-check", "-config", cfg, "-input", "src", "-output", "out", "-retention", "0", "-ssh-key", key}
	env := map[string]string{"GOGITEM_PARALLEL": "8", "GOGITEM_RUN_ON_START": "false", "GOGITEM_RETENTION": "5"}
	code, stdout, stderr := runWith(t, args, env)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitOK, stderr)
	}

	var report checkReport
	dec := yaml.NewDecoder(strings.NewReader(stdout))
	dec.KnownFields(true)
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("stdout isn't the YAML report: %v\n%s", err, stdout)
	}
	if report.Git == "" || report.Config != cfg || report.Mode != "daemon" || report.Parallel != 8 ||
		report.RunOnStart == nil || *report.RunOnStart {
		t.Errorf("report = git %q, config %q, mode %q, parallel %d, run_on_start %v",
			report.Git, report.Config, report.Mode, report.Parallel, report.RunOnStart)
	}
	want := []config.Repository{
		{Input: "https://example.com/a.git", Output: filepath.Join(dir, "a"), Schedule: "@daily", Retention: 10, Timeout: 30 * time.Minute, SSHKey: key},
		// -retention 0 wins over GOGITEM_RETENTION=5 and defaults.retention.
		{Input: filepath.Join(wd, "src"), Output: filepath.Join(wd, "out"), Schedule: "@daily", Retention: 0, Timeout: 30 * time.Minute, SSHKey: key},
	}
	if !reflect.DeepEqual(report.Repositories, want) {
		t.Errorf("repositories =\n%+v\nwant\n%+v", report.Repositories, want)
	}

	// -once: no run_on_start, which only applies to daemon mode.
	_, stdout, _ = runWith(t, []string{"-check", "-once", "-config", cfg}, nil)
	if !strings.Contains(stdout, "\nmode: once\n") || strings.Contains(stdout, "run_on_start") {
		t.Errorf("-check -once printed:\n%s", stdout)
	}
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
	code, stdout, stderr := runWith(t, []string{"-once", "-config", cfg}, nil)
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
	code, _, stderr := runWith(t, []string{"-once", "-config", cfg}, nil)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	checkOutput(t, "stderr", stderr, `msg="invalid config" problem="`+cfg+`: line 4: unknown key \"retension\""`)
}

func TestRunWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{aRepo(t), append([]string{"-check"}, aRepo(t)...)} {
		code, stdout, stderr := runWith(t, args, nil)
		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
		checkOutput(t, "stdout", stdout, "")
		checkOutput(t, "stderr", stderr, `msg="go-git-em needs git on PATH"`)
	}

	// -version doesn't need git.
	if code, _, _ := runWith(t, []string{"-version"}, nil); code != exitOK {
		t.Errorf("-version without git: exit code = %d, want %d", code, exitOK)
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
	check := fs.Bool("check", false, "")
	if err := fs.Parse([]string{"-a", "from-flag"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GOGITEM_A": "from-env", "GOGITEM_B": "from-env", "GOGITEM_N": "7", "GOGITEM_CHECK": "true"}
	if err := setFromEnv(fs, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if *a != "from-flag" || *b != "from-env" || *n != 7 || *check {
		t.Errorf("a, b, n, check = %q, %q, %d, %v; want from-flag, from-env, 7, false", *a, *b, *n, *check)
	}
	// Variables count as set, for the flag combination checks.
	var set []string
	fs.Visit(func(f *flag.Flag) { set = append(set, f.Name) })
	if want := []string{"a", "b", "n"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set flags = %v, want %v", set, want)
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, aRepo(t), func(string) string { return "" }, &stdout, &stderr)
	if code != exitFail || !strings.Contains(stderr.String(), `level=ERROR msg=interrupted`) || strings.Contains(stderr.String(), "needs git") {
		t.Errorf("exit code %d, stderr:\n%s", code, stderr.String())
	}
}
