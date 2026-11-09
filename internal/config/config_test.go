package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeFile writes a file with the given mode (umask aside) and returns its path.
func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// problems returns the problems in err, which must be an *Error.
func problems(t *testing.T, err error) []string {
	t.Helper()
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("error = %v, want an *Error", err)
	}
	return cfgErr.Problems
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "go-git-em.yaml", `
defaults:
  schedule: "@daily"
  retention: 30
  timeout: 1h30m
  ssh_key: ~/.ssh/id_ed25519
repositories:
  - input: https://example.com/project.git
    output: ./backups/project
    schedule: "0 * * * *"
    retention: 0
  - input: git@example.com:other.git
    output: ./backups/other
    timeout: 10m
    ssh_key: ./other_key
`, 0o644)

	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &File{
		Defaults: Settings{Schedule: "@daily", Retention: new(30), Timeout: new(90 * time.Minute), SSHKey: "~/.ssh/id_ed25519"},
		Repositories: []Repo{
			{Input: "https://example.com/project.git", Output: "./backups/project", Settings: Settings{Schedule: "0 * * * *", Retention: new(0)}},
			{Input: "git@example.com:other.git", Output: "./backups/other", Settings: Settings{Timeout: new(10 * time.Minute), SSHKey: "./other_key"}},
		},
		Path: path,
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", f, want)
	}
}

func TestLoadRelativePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cfg.yaml", "", 0o644)
	t.Chdir(dir)
	f, err := Load("cfg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "cfg.yaml"); f.Path != want {
		t.Errorf("Path = %q, want %q", f.Path, want)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "cfg.yaml", "# nothing yet\n", 0o644)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&File{Path: path}); !reflect.DeepEqual(f, want) {
		t.Errorf("Load = %+v, want %+v", f, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string // every problem, after the file's path and ": "
	}{
		{
			name: "unknown keys everywhere, all reported",
			content: `defaults:
  retension: 5
repositories:
  - input: a
    output: b
    shedule: "@daily"
verbose: true
`,
			want: []string{
				`line 2: unknown key "retension"`,
				`line 6: unknown key "shedule"`,
				`line 7: unknown key "verbose"`,
			},
		},
		{
			name:    "wrong types",
			content: "defaults:\n  retention: many\n  timeout: 30\n",
			want: []string{
				"line 2: cannot unmarshal !!str `many` into int",
				"line 3: cannot unmarshal !!int `30` into a duration such as 30m",
			},
		},
		{
			name:    "not a duration",
			content: "defaults:\n  timeout: soon\n",
			want:    []string{"line 2: cannot unmarshal !!str `soon` into a duration such as 30m"},
		},
		{
			name:    "repositories isn't a list",
			content: "repositories:\n  input: a\n",
			want:    []string{"line 2: cannot unmarshal !!map into a list of repositories"},
		},
		{
			name:    "not a mapping",
			content: "- input: a\n",
			want:    []string{"line 1: cannot unmarshal !!seq into a config file"},
		},
		{
			name:    "duplicate key",
			content: "repositories:\n  - input: a\n    input: b\n",
			want:    []string{`line 3: mapping key "input" already defined at line 2`},
		},
		{
			name:    "syntax error",
			content: "defaults:\n  schedule: @daily\n",
			want:    []string{"line 2: found character that cannot start any token"},
		},
		{
			name:    "two documents",
			content: "repositories: []\n---\nrepositories: []\n",
			want:    []string{"holds more than one YAML document"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "cfg.yaml", tt.content, 0o644)
			_, err := Load(path)
			got := problems(t, err)
			var want []string
			for _, w := range tt.want {
				want = append(want, path+": "+w)
			}
			if !slices.Equal(got, want) {
				t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestResolveSettings(t *testing.T) {
	f := &File{
		Path: "/etc/go-git-em/cfg.yaml",
		Defaults: Settings{
			Schedule:  "@daily",
			Retention: new(10),
			Timeout:   new(time.Hour),
		},
		Repositories: []Repo{
			{Input: "https://example.com/a.git", Output: "/b/a"},
			{Input: "https://example.com/b.git", Output: "/b/b", Settings: Settings{
				Schedule:  "0 * * * *",
				Retention: new(0), // 0 is set: keep everything, not the default
				Timeout:   new(time.Minute),
			}},
		},
	}
	got, err := Resolve(f, Options{WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{
		{Input: "https://example.com/a.git", Output: "/b/a", Schedule: "@daily", Retention: 10, Timeout: time.Hour},
		{Input: "https://example.com/b.git", Output: "/b/b", Schedule: "0 * * * *", Retention: 0, Timeout: time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve =\n%+v\nwant\n%+v", got, want)
	}
}

func TestResolveBuiltInDefaults(t *testing.T) {
	f := &File{
		Path:         "/etc/go-git-em/cfg.yaml",
		Repositories: []Repo{{Input: "https://example.com/a.git", Output: "/b/a"}},
	}
	got, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	// No schedule, and retention 0: without a retention, every snapshot is kept.
	want := []Repository{{Input: "https://example.com/a.git", Output: "/b/a", Retention: 0, Timeout: DefaultTimeout}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve =\n%+v\nwant\n%+v", got, want)
	}
	if DefaultTimeout != 30*time.Minute {
		t.Errorf("DefaultTimeout = %v, want 30m", DefaultTimeout)
	}
}

func TestResolvePaths(t *testing.T) {
	f := &File{
		Path: "/etc/go-git-em/cfg.yaml",
		Repositories: []Repo{
			// Local inputs and outputs resolve against the file's directory...
			{Input: "./src", Output: "out"},
			{Input: "src2", Output: "../backups/x/"},
			{Input: "/srv/git/abs.git", Output: "/backups/abs"},
			// ...after ~/ expands to the home directory.
			{Input: "~/repos/mine", Output: "~/backups/mine"},
			{Input: "~", Output: "~/backups/home"},
			// URLs stay as written.
			{Input: "https://example.com/a.git", Output: "o1"},
			{Input: "ssh://git@example.com:2222/a.git", Output: "o2"},
			{Input: "git@example.com:a.git", Output: "o3"},
			{Input: "file:///srv/git/a.git", Output: "o4"},
			{Input: "example.com:a.git", Output: "o5"},
		},
	}
	got, err := Resolve(f, Options{
		Input:   &Repo{Input: "flag-src", Output: "flag-out"}, // against WorkDir
		Once:    true,
		WorkDir: "/work",
		Home:    "/home/me",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"/etc/go-git-em/src", "/etc/go-git-em/out"},
		{"/etc/go-git-em/src2", "/etc/backups/x"},
		{"/srv/git/abs.git", "/backups/abs"},
		{"/home/me/repos/mine", "/home/me/backups/mine"},
		{"/home/me", "/home/me/backups/home"},
		{"https://example.com/a.git", "/etc/go-git-em/o1"},
		{"ssh://git@example.com:2222/a.git", "/etc/go-git-em/o2"},
		{"git@example.com:a.git", "/etc/go-git-em/o3"},
		{"file:///srv/git/a.git", "/etc/go-git-em/o4"},
		{"example.com:a.git", "/etc/go-git-em/o5"},
		{"/work/flag-src", "/work/flag-out"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d repositories, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Input != w[0] || got[i].Output != w[1] {
			t.Errorf("repository %d: input, output = %q, %q; want %q, %q", i+1, got[i].Input, got[i].Output, w[0], w[1])
		}
	}
}

func TestResolveRelativeFilePath(t *testing.T) {
	// A File built by hand may have a relative Path: it resolves against WorkDir.
	f := &File{Path: "conf/cfg.yaml", Repositories: []Repo{{Input: "src", Output: "out"}}}
	got, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Input != "/work/conf/src" || got[0].Output != "/work/conf/out" {
		t.Errorf("input, output = %q, %q", got[0].Input, got[0].Output)
	}
}

func TestResolveInputRepo(t *testing.T) {
	f := &File{
		Path:         "/etc/go-git-em/cfg.yaml",
		Defaults:     Settings{Schedule: "@daily", Retention: new(10)},
		Repositories: []Repo{{Input: "https://example.com/a.git", Output: "/b/a"}},
	}
	tests := []struct {
		name  string
		input Repo
		want  Repository
	}{
		{
			name:  "defaults apply",
			input: Repo{Input: "https://example.com/b.git", Output: "b"},
			want:  Repository{Input: "https://example.com/b.git", Output: "/work/b", Schedule: "@daily", Retention: 10, Timeout: DefaultTimeout},
		},
		{
			name:  "-schedule and -retention",
			input: Repo{Input: "https://example.com/b.git", Output: "b", Settings: Settings{Schedule: "@hourly", Retention: new(0)}},
			want:  Repository{Input: "https://example.com/b.git", Output: "/work/b", Schedule: "@hourly", Retention: 0, Timeout: DefaultTimeout},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(f, Options{Input: &tt.input, WorkDir: "/work"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || !reflect.DeepEqual(got[1], tt.want) {
				t.Errorf("Resolve = %+v, want the file's repository then %+v", got, tt.want)
			}
		})
	}
}

func TestResolveFlagsOnly(t *testing.T) {
	got, err := Resolve(nil, Options{Input: &Repo{Input: "src", Output: "out"}, Once: true, WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{{Input: "/work/src", Output: "/work/out", Retention: 0, Timeout: DefaultTimeout}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveSSHKey(t *testing.T) {
	fileDir, workDir := t.TempDir(), t.TempDir()
	fileKey := writeFile(t, fileDir, "file_key", "key", 0o600)
	repoKey := writeFile(t, fileDir, "repo_key", "key", 0o600)
	flagKey := writeFile(t, workDir, "flag_key", "key", 0o400)
	f := &File{
		Path:     filepath.Join(fileDir, "cfg.yaml"),
		Defaults: Settings{SSHKey: "file_key"},
		Repositories: []Repo{
			{Input: "git@example.com:a.git", Output: "a"},
			{Input: "git@example.com:b.git", Output: "b", Settings: Settings{SSHKey: "repo_key"}},
		},
	}

	got, err := Resolve(f, Options{Once: true, WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SSHKey != fileKey || got[1].SSHKey != repoKey {
		t.Errorf("without -ssh-key: keys = %q, %q; want %q, %q", got[0].SSHKey, got[1].SSHKey, fileKey, repoKey)
	}

	// -ssh-key replaces defaults.ssh_key, resolved against the current
	// directory. A repository's own key still wins.
	got, err = Resolve(f, Options{SSHKey: "flag_key", Once: true, WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SSHKey != flagKey || got[1].SSHKey != repoKey {
		t.Errorf("with -ssh-key: keys = %q, %q; want %q, %q", got[0].SSHKey, got[1].SSHKey, flagKey, repoKey)
	}
}

func TestResolveSSHKeyProblems(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "open_key", "key", 0o644)
	writeFile(t, dir, "group_key", "key", 0o640)
	writeFile(t, dir, "good_key", "key", 0o600)
	if err := os.Mkdir(filepath.Join(dir, "key_dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &File{
		Path:     filepath.Join(dir, "cfg.yaml"),
		Defaults: Settings{SSHKey: "missing_key"},
		Repositories: []Repo{
			{Input: "git@example.com:a.git", Output: "a"},
			{Input: "git@example.com:b.git", Output: "b"}, // the same default key: reported once
			{Input: "git@example.com:c.git", Output: "c", Settings: Settings{SSHKey: "open_key"}},
			{Input: "git@example.com:d.git", Output: "d", Settings: Settings{SSHKey: "group_key"}},
			{Input: "git@example.com:e.git", Output: "e", Settings: Settings{SSHKey: "key_dir"}},
			{Input: "git@example.com:f.git", Output: "f", Settings: Settings{SSHKey: "good_key"}},
		},
	}
	_, err := Resolve(f, Options{Once: true, WorkDir: dir})
	want := []string{
		"repository 3 (git@example.com:c.git): ssh_key " + dir + "/open_key has permissions 0644, and OpenSSH refuses a private key that other users can access: run chmod 600 " + dir + "/open_key",
		"repository 4 (git@example.com:d.git): ssh_key " + dir + "/group_key has permissions 0640, and OpenSSH refuses a private key that other users can access: run chmod 600 " + dir + "/group_key",
		"repository 5 (git@example.com:e.git): ssh_key " + dir + "/key_dir isn't a regular file",
		"defaults: ssh_key " + dir + "/missing_key doesn't exist",
	}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestResolveUnusedDefaultKey(t *testing.T) {
	// Every repository has its own key, so a missing default key doesn't matter.
	dir := t.TempDir()
	writeFile(t, dir, "own_key", "key", 0o600)
	f := &File{
		Path:         filepath.Join(dir, "cfg.yaml"),
		Defaults:     Settings{SSHKey: "missing_key"},
		Repositories: []Repo{{Input: "git@example.com:a.git", Output: "a", Settings: Settings{SSHKey: "own_key"}}},
	}
	if _, err := Resolve(f, Options{Once: true, WorkDir: dir}); err != nil {
		t.Errorf("Resolve: %v", err)
	}
	// Nor does one under ~/ without a HOME.
	f.Defaults.SSHKey = "~/.ssh/id_ed25519"
	if _, err := Resolve(f, Options{Once: true, WorkDir: dir}); err != nil {
		t.Errorf("Resolve with ~/ and no HOME: %v", err)
	}
	// -ssh-key replaces the missing default key, which then isn't checked either.
	writeFile(t, dir, "flag_key", "key", 0o600)
	f.Defaults.SSHKey = "missing_key"
	f.Repositories[0].SSHKey = ""
	if _, err := Resolve(f, Options{SSHKey: "flag_key", Once: true, WorkDir: dir}); err != nil {
		t.Errorf("Resolve with -ssh-key: %v", err)
	}
}

func TestResolveDefaultKeyNoHome(t *testing.T) {
	// A default key that two repositories use and that can't be resolved: one problem.
	f := &File{
		Path:     "/etc/go-git-em/cfg.yaml",
		Defaults: Settings{SSHKey: "~/.ssh/id_ed25519"},
		Repositories: []Repo{
			{Input: "git@example.com:a.git", Output: "a"},
			{Input: "git@example.com:b.git", Output: "b"},
		},
	}
	_, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	want := []string{`defaults: ssh_key "~/.ssh/id_ed25519": can't expand ~ because HOME isn't set`}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}

func TestResolveProblems(t *testing.T) {
	f := &File{
		Path: "/etc/go-git-em/cfg.yaml",
		Defaults: Settings{
			Schedule:  "0 * * *",
			Retention: new(-1),
			Timeout:   new(time.Duration(0)),
		},
		Repositories: []Repo{
			{Output: "a"},
			{Input: "./src"},
			{Input: "-oProxyCommand=evil", Output: "c"},
			{Input: "--upload-pack=evil", Output: "d"},
			{Input: "https://example.com/e.git", Output: "./a/", Settings: Settings{
				Schedule:  "@fortnightly",
				Retention: new(-5),
				Timeout:   new(-time.Minute),
			}},
			{Input: "https://example.com/f.git", Output: "/etc/go-git-em/a"},
		},
	}
	_, err := Resolve(f, Options{
		Input: &Repo{Input: "https://example.com/g.git", Output: "/etc/go-git-em/c", Settings: Settings{Schedule: "@every soon"}},
		Once:  true, WorkDir: "/work",
	})
	want := []string{
		`defaults: schedule "0 * * *": expected exactly 5 fields, found 4: [0 * * *]`,
		"defaults: retention must be 0 or more, got -1",
		"defaults: timeout must be more than 0, got 0s",
		"repository 1: input is required",
		"repository 2 (./src): output is required",
		`repository 3 (-oProxyCommand=evil): input starts with "-", which git would read as an option`,
		`repository 4 (--upload-pack=evil): input starts with "-", which git would read as an option`,
		"repository 5 (https://example.com/e.git): output /etc/go-git-em/a is already used by repository 1",
		`repository 5 (https://example.com/e.git): schedule "@fortnightly": unrecognized descriptor: @fortnightly`,
		"repository 5 (https://example.com/e.git): retention must be 0 or more, got -5",
		"repository 5 (https://example.com/e.git): timeout must be more than 0, got -1m0s",
		"repository 6 (https://example.com/f.git): output /etc/go-git-em/a is already used by repository 1",
		"-input repository (https://example.com/g.git): output /etc/go-git-em/c is already used by repository 3 (-oProxyCommand=evil)",
		`-input repository (https://example.com/g.git): schedule "@every soon": failed to parse duration @every soon: time: invalid duration "soon"`,
	}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "14 problems: defaults: schedule") {
		t.Errorf("Error() = %q", msg)
	}
}

func TestResolveDaemonNeedsSchedule(t *testing.T) {
	f := &File{
		Path: "/etc/go-git-em/cfg.yaml",
		Repositories: []Repo{
			{Input: "https://example.com/a.git", Output: "a"},
			{Input: "https://example.com/b.git", Output: "b", Settings: Settings{Schedule: "@daily"}},
		},
	}
	// -once runs without schedules.
	if _, err := Resolve(f, Options{Once: true, WorkDir: "/work"}); err != nil {
		t.Fatalf("-once: %v", err)
	}

	_, err := Resolve(f, Options{WorkDir: "/work"})
	want := []string{"repository 1 (https://example.com/a.git): no schedule: daemon mode needs one, on the repository or in defaults (-once runs without)"}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}

	// A schedule in defaults covers every repository.
	f.Defaults.Schedule = "@hourly"
	got, err := Resolve(f, Options{WorkDir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Schedule != "@hourly" || got[1].Schedule != "@daily" {
		t.Errorf("schedules = %q, %q", got[0].Schedule, got[1].Schedule)
	}
}

func TestResolveNoRepositories(t *testing.T) {
	for _, f := range []*File{nil, {Path: "/etc/go-git-em/cfg.yaml"}} {
		_, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
		want := []string{"no repositories to back up: list them in a config file (-config), or give one with -input and -output"}
		if got := problems(t, err); !slices.Equal(got, want) {
			t.Errorf("Resolve(%v): problems = %q, want %q", f, got, want)
		}
	}
}

func TestResolveNoHome(t *testing.T) {
	f := &File{Path: "/etc/go-git-em/cfg.yaml", Repositories: []Repo{{Input: "~/src", Output: "~/out"}}}
	_, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	want := []string{
		`repository 1 (~/src): input "~/src": can't expand ~ because HOME isn't set`,
		`repository 1 (~/src): output "~/out": can't expand ~ because HOME isn't set`,
	}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestIsLocal(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"src", true},
		{"./src", true},
		{"/srv/git/a.git", true},
		{"~/repos/a", true},
		{"./dir:with:colons", true}, // a slash before the first colon
		{"a/b:c", true},
		{"https://example.com/a.git", false},
		{"ssh://git@example.com/a.git", false},
		{"git+ssh://example.com/a.git", false},
		{"file:///srv/git/a.git", false},
		{"git@example.com:a.git", false},
		{"example.com:a.git", false},
		{"host:", false},
		{"1http://x", false}, // not a URL (bad scheme), but scp-like: host "1http"
	}
	for _, tt := range tests {
		if got := isLocal(tt.input); got != tt.want {
			t.Errorf("isLocal(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsURL(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"https://example.com/a.git", true},
		{"git+ssh://x/y", true},
		{"a.b-c+d://x", true},
		{"file:///x", true},
		{"1http://x", false},
		{"://x", false},
		{"ht tp://x", false},
		{"https:/x", false},
		{"git@example.com:a.git", false},
		{"./x://y", false},
	}
	for _, tt := range tests {
		if got := isURL(tt.s); got != tt.want {
			t.Errorf("isURL(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestResolveSameDirectory(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	f := &File{
		Path: filepath.Join(dir, "cfg.yaml"),
		Repositories: []Repo{
			{Input: "https://example.com/a.git", Output: "real/out"},
			// The same directory through a symlink, even though "out" doesn't exist yet.
			{Input: "https://example.com/b.git", Output: "link/out"},
			{Input: "https://example.com/c.git", Output: "real/other"},
		},
	}
	_, err := Resolve(f, Options{Once: true, WorkDir: dir})
	want := []string{"repository 2 (https://example.com/b.git): output " + dir + "/link/out is the same directory as " +
		dir + "/real/out, already used by repository 1 (https://example.com/a.git)"}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestResolveSameDirectoryCase(t *testing.T) {
	f := &File{
		Path: "/etc/go-git-em/cfg.yaml",
		Repositories: []Repo{
			{Input: "https://example.com/a.git", Output: "/backups/Project"},
			{Input: "https://example.com/b.git", Output: "/backups/project"},
		},
	}
	// Where file names keep case, as on Linux by default, these are two directories...
	t.Cleanup(func(was bool) func() { return func() { caseInsensitive = was } }(caseInsensitive))
	caseInsensitive = false
	if _, err := Resolve(f, Options{Once: true, WorkDir: "/work"}); err != nil {
		t.Errorf("case-sensitive: %v", err)
	}
	// ...and where they don't, as on macOS and Windows by default, one.
	caseInsensitive = true
	_, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	want := []string{"repository 2 (https://example.com/b.git): output /backups/project is the same directory as /backups/Project, already used by repository 1 (https://example.com/a.git)"}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("case-insensitive: problems = %q, want %q", got, want)
	}
}

func TestDirKey(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir) // the temporary directory may itself be behind a symlink (macOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "target"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func(was bool) func() { return func() { caseInsensitive = was } }(caseInsensitive))
	caseInsensitive = false
	tests := []struct{ path, want string }{
		{dir, real},
		{filepath.Join(dir, "new", "deeper"), filepath.Join(real, "new", "deeper")},
		// A symlink to a directory that doesn't exist yet can't be resolved: it stays.
		{filepath.Join(dir, "dangling", "x"), filepath.Join(real, "dangling", "x")},
		{"/no/such/dir", "/no/such/dir"},
	}
	for _, tt := range tests {
		if got := dirKey(tt.path); got != tt.want {
			t.Errorf("dirKey(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestResolveKeyOwnedByAnotherUser(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, to give the key to another user")
	}
	// OpenSSH uses a key that another user owns, whatever its mode, such as
	// a key mounted read-only from a Kubernetes secret. So does go-git-em.
	dir := t.TempDir()
	key := writeFile(t, dir, "key", "key", 0o644)
	if err := os.Chown(key, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	f := &File{Path: filepath.Join(dir, "cfg.yaml"), Defaults: Settings{SSHKey: "key"},
		Repositories: []Repo{{Input: "git@example.com:a.git", Output: "a"}}}
	if _, err := Resolve(f, Options{Once: true, WorkDir: dir}); err != nil {
		t.Errorf("Resolve: %v", err)
	}
}

func TestResolveUnreadableKey(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read any file")
	}
	dir := t.TempDir()
	writeFile(t, dir, "key", "key", 0o000)
	f := &File{Path: filepath.Join(dir, "cfg.yaml"), Defaults: Settings{SSHKey: "key"},
		Repositories: []Repo{{Input: "git@example.com:a.git", Output: "a"}}}
	_, err := Resolve(f, Options{Once: true, WorkDir: dir})
	want := []string{"defaults: ssh_key " + dir + "/key can't be read: permission denied"}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct{ input, want string }{
		{"https://user:secret@example.com/a.git", "https://xxxxx@example.com/a.git"},
		{"https://ghp_token@github.com/a.git", "https://xxxxx@github.com/a.git"},
		{"HTTP://user:secret@example.com/a.git", "HTTP://xxxxx@example.com/a.git"},
		{"https://user:p@ss@example.com/a.git", "https://xxxxx@example.com/a.git"},
		{"ftps://user:secret@example.com/a.git", "ftps://xxxxx@example.com/a.git"},
		{"ssh://git:secret@example.com/a.git", "ssh://git:xxxxx@example.com/a.git"},
		{"git+ssh://me:secret@example.com/a.git", "git+ssh://me:xxxxx@example.com/a.git"},
		// Nothing to hide.
		{"ssh://git@example.com:2222/a.git", "ssh://git@example.com:2222/a.git"},
		{"https://example.com/a.git", "https://example.com/a.git"},
		{"https://example.com/users/@me/a.git", "https://example.com/users/@me/a.git"},
		{"file:///srv/git/a.git", "file:///srv/git/a.git"},
		{"git@example.com:a.git", "git@example.com:a.git"},
		{"/srv/git/me@home", "/srv/git/me@home"},
	}
	for _, tt := range tests {
		if got := Redact(tt.input); got != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveProblemsHideCredentials(t *testing.T) {
	f := &File{Path: "/etc/go-git-em/cfg.yaml", Repositories: []Repo{{Input: "https://me:hunter2@example.com/a.git"}}}
	_, err := Resolve(f, Options{Once: true, WorkDir: "/work"})
	want := []string{"repository 1 (https://xxxxx@example.com/a.git): output is required"}
	if got := problems(t, err); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}

func TestSplitSecret(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@example.com/a.git": "user:secret",
		"https://ghp_token@github.com/a.git":    "ghp_token",
		"https://user:p@ss@example.com/a.git":   "user:p@ss",
		"ssh://git:secret@example.com/a.git":    "secret",
		"ssh://git@example.com:2222/a.git":      "",
		"https://example.com/users/@me/a.git":   "",
		"git@example.com:a.git":                 "",
		"/srv/git/me@home":                      "",
	}
	for input, want := range tests {
		before, secret, after := splitSecret(input)
		if secret != want || before+secret+after != input {
			t.Errorf("splitSecret(%q) = %q, %q, %q; want the secret %q", input, before, secret, after, want)
		}
	}
}

func TestCredentialReplacements(t *testing.T) {
	tests := []struct {
		input      string
		text, want string
	}{
		{"https://example.com/a.git", "", ""}, // nothing to hide: no pairs
		{"https://me:hunter2@example.com/a.git", "clone https://me:hunter2@example.com/a.git: me:hunter2@example.com", "clone https://xxxxx@example.com/a.git: xxxxx@example.com"},
		// Without "@<host>" after it, a password could be a word anywhere.
		{"https://me:backup@example.com/a.git", "a backup of me:backup@example.com", "a backup of xxxxx@example.com"},
		// With it, the password is hidden even when the text lost what came before.
		{"https://me:hunter2@example.com/a.git", "...unter2@example.com, ...hunter2@example.com", "...unter2@example.com, ...xxxxx@example.com"},
		{"https://me:abc:defgh@example.com/a.git", "...c:defgh@example.com, me:abc:defgh@example.com", "...c:defgh@example.com, xxxxx@example.com"},
		{"https://ghp_0123456789abcdefghij:x-oauth-basic@github.com/a.git", "...ghp_0123456789abcdefghij:x-oauth-basic@github.com", "...xxxxx@github.com"},
		{"https://shorttok@git.example.com/a.git", "fatal: shorttok@git.example.com/a.git", "fatal: xxxxx@git.example.com/a.git"},
		// A user name isn't rewritten anywhere else.
		{"https://backup-robot-account-01@github.com/a.git", "/backups/backup-robot-account-01, github.com/backup-robot-account-01", "/backups/backup-robot-account-01, github.com/backup-robot-account-01"},
		// For ssh only the password is secret.
		{"ssh://deploy:pw@example.com/a.git", "deploy:pw@example.com, rpw@example.org", "deploy:xxxxx@example.com, rpw@example.org"},
	}
	for _, tt := range tests {
		pairs := CredentialReplacements(tt.input)
		if tt.text == "" {
			if pairs != nil {
				t.Errorf("CredentialReplacements(%q) = %q, want nil", tt.input, pairs)
			}
			continue
		}
		if got := strings.NewReplacer(pairs...).Replace(tt.text); got != tt.want {
			t.Errorf("%s: %q became %q, want %q", tt.input, tt.text, got, tt.want)
		}
	}
}
