package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

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
		{"unknown flag", []string{"-nope"}, exitUsage, "", "flag provided but not defined: -nope"},
		{"positional argument", []string{"backup"}, exitUsage, "", `unexpected argument "backup"`},
		{"bad log level", []string{"-log-level", "loud"}, exitUsage, "", `invalid value "loud" for flag -log-level: must be one of debug, info, warn, error`},
		{"bad log format", []string{"-log-format", "xml"}, exitUsage, "", `invalid value "xml" for flag -log-format: must be one of text, json`},
		{"logs git version", nil, exitFail, "", `level=INFO msg="found git" version=`},
		{"no backups yet", nil, exitFail, "", `level=ERROR msg="backups are not implemented yet"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := func(string) string { return "" }
			code := run(t.Context(), tt.args, getenv, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			checkOutput(t, "stdout", stdout.String(), tt.wantStdout)
			checkOutput(t, "stderr", stderr.String(), tt.wantStderr)
		})
	}
}

func TestRunWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), nil, func(string) string { return "" }, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	checkOutput(t, "stderr", stderr.String(), `msg="go-git-em needs git on PATH"`)

	// -version doesn't need git.
	stderr.Reset()
	if code := run(t.Context(), []string{"-version"}, func(string) string { return "" }, &stdout, &stderr); code != exitOK {
		t.Errorf("-version without git: exit code = %d, want %d", code, exitOK)
	}
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
	logger := newLogger(&buf, slog.LevelWarn, "json")
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
