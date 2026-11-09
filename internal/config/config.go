// Package config builds the list of repositories to back up from the config
// file and from the flags and environment variables that add to it. Resolve
// fills in every setting, makes every path absolute and reports every problem
// at once, so the user can fix them all in one go.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/alexycodes/go-git-em/internal/schedule"
)

// DefaultTimeout is the timeout of a repository that neither it nor the defaults
// section sets. There is no default schedule: daemon mode needs one for every
// repository. There is no default retention either: without one, every
// snapshot is kept.
const DefaultTimeout = 30 * time.Minute

// File is a config file as written.
type File struct {
	// Defaults apply to every repository that doesn't set the field itself.
	Defaults     Settings `yaml:"defaults"`
	Repositories []Repo   `yaml:"repositories"`

	// Path is the file's absolute path. Relative paths in the file resolve
	// against its directory.
	Path string `yaml:"-"`
}

// Settings are the fields a repository can set itself or take from defaults.
// An empty string or a nil pointer isn't set.
type Settings struct {
	Schedule  string         `yaml:"schedule"`
	Retention *int           `yaml:"retention"`
	Timeout   *time.Duration `yaml:"timeout"`
	SSHKey    string         `yaml:"ssh_key"`
}

// Repo is one repository as written in the file, or as given with -input.
type Repo struct {
	Input    string `yaml:"input"`
	Output   string `yaml:"output"`
	Settings `yaml:",inline"`
}

// Repository is one repository to back up, with every setting resolved.
type Repository struct {
	Input     string        `yaml:"input"`              // a URL, or an absolute local path
	Output    string        `yaml:"output"`             // absolute
	Schedule  string        `yaml:"schedule,omitempty"` // "" if none: only -once can run it
	Retention int           `yaml:"retention"`          // snapshots to keep; 0, the default, keeps all
	Timeout   time.Duration `yaml:"timeout"`            // for one backup run
	SSHKey    string        `yaml:"ssh_key,omitempty"`  // absolute, or "" to leave it to OpenSSH
}

// Options are the values from flags and environment variables, and what
// Resolve needs to know about the process.
type Options struct {
	// Input is the repository from -input, added after the file's, or nil.
	// Its relative paths resolve against WorkDir.
	Input *Repo
	// SSHKey (-ssh-key) replaces defaults.ssh_key. It resolves against WorkDir.
	SSHKey string
	// Once is -once mode, which needs no schedules.
	Once bool
	// WorkDir is the absolute current directory. Home is $HOME, for "~/".
	WorkDir, Home string
}

// Error lists every problem found in the configuration.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems: %s", len(e.Problems), strings.Join(e.Problems, "; "))
}

// Load reads and decodes the config file at path. Unknown keys are errors, so
// a typo such as "retension" doesn't silently fall back to the default.
// Problems in the file's content are reported as an *Error.
func Load(path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	f := &File{Path: abs}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) { // io.EOF: an empty file
		return nil, decodeError(path, err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, &Error{Problems: []string{path + ": holds more than one YAML document"}}
	}
	return f, nil
}

var (
	unknownKey = regexp.MustCompile(`^(line \d+): field (.+) not found in type \S+$`)
	// Go types in yaml's messages, in words.
	typeNames = strings.NewReplacer(
		"[]config.Repo", "a list of repositories",
		"config.Repo", "a repository",
		"config.Settings", "a set of defaults",
		"config.File", "a config file",
		"time.Duration", "a duration such as 30m",
	)
)

// decodeError turns a yaml error into an *Error with one problem per message.
func decodeError(path string, err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return &Error{Problems: []string{path + ": " + strings.TrimPrefix(err.Error(), "yaml: ")}}
	}
	problems := make([]string, len(typeErr.Errors))
	for i, msg := range typeErr.Errors {
		msg = unknownKey.ReplaceAllString(msg, `$1: unknown key "$2"`)
		problems[i] = path + ": " + typeNames.Replace(msg)
	}
	return &Error{Problems: problems}
}

// Resolve returns the repositories to back up: those in f (which may be nil),
// then opts.Input. Each setting comes from the repository, then the defaults
// section, then the built-in default. Relative paths in f resolve against its
// directory, and those in opts against opts.WorkDir. If anything is wrong, the
// error is an *Error that lists every problem.
func Resolve(f *File, opts Options) ([]Repository, error) {
	if f == nil {
		f = &File{}
	}
	fileDir := filepath.Dir(f.Path)
	if !filepath.IsAbs(fileDir) {
		fileDir = filepath.Join(opts.WorkDir, fileDir)
	}
	r := resolver{home: opts.Home, outputs: map[string]outputUse{}}

	r.checkSettings("defaults", f.Defaults)
	// The default key is defaults.ssh_key, or -ssh-key in its place. It's
	// resolved and checked only if some repository uses it.
	keyLabel, keyPath, keyDir := "defaults", f.Defaults.SSHKey, fileDir
	if opts.SSHKey != "" {
		keyLabel, keyPath, keyDir = "-ssh-key", opts.SSHKey, opts.WorkDir
	}
	defaultKey, defaultKeyUsed := "", false

	type source struct {
		repo  Repo
		dir   string // relative paths resolve against it
		label string // names the repository in problems
	}
	var sources []source
	for i, repo := range f.Repositories {
		sources = append(sources, source{repo, fileDir, label(fmt.Sprintf("repository %d", i+1), repo.Input)})
	}
	if opts.Input != nil {
		sources = append(sources, source{*opts.Input, opts.WorkDir, label("-input repository", opts.Input.Input)})
	}
	if len(sources) == 0 {
		r.addf("no repositories to back up: list them in a config file (-config), or give one with -input and -output")
	}

	var repos []Repository
	for _, s := range sources {
		repo := Repository{
			Input:     r.input(s.label, s.repo.Input, s.dir),
			Output:    r.output(s.label, s.repo.Output, s.dir),
			Schedule:  cmp.Or(s.repo.Schedule, f.Defaults.Schedule),
			Retention: *cmp.Or(s.repo.Retention, f.Defaults.Retention, new(0)),
			Timeout:   *cmp.Or(s.repo.Timeout, f.Defaults.Timeout, new(DefaultTimeout)),
		}
		r.checkSettings(s.label, s.repo.Settings)
		if repo.Schedule == "" && !opts.Once {
			r.addf("%s: no schedule: daemon mode needs one, on the repository or in defaults (-once runs without)", s.label)
		}
		switch {
		case s.repo.SSHKey != "":
			repo.SSHKey = r.path(s.label, "ssh_key", s.repo.SSHKey, s.dir)
			r.checkKey(s.label, repo.SSHKey)
		case keyPath != "":
			if !defaultKeyUsed {
				defaultKey, defaultKeyUsed = r.path(keyLabel, "ssh_key", keyPath, keyDir), true
			}
			repo.SSHKey = defaultKey
		}
		repos = append(repos, repo)
	}
	if defaultKeyUsed {
		r.checkKey(keyLabel, defaultKey)
	}

	if len(r.problems) > 0 {
		return nil, &Error{Problems: r.problems}
	}
	return repos, nil
}

// resolver collects problems while Resolve works through the configuration.
type resolver struct {
	home     string
	outputs  map[string]outputUse // dirKey of a resolved output → who has it
	problems []string
}

// outputUse is the repository that has an output, and its output as resolved.
type outputUse struct{ label, path string }

func (r *resolver) addf(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// input returns the input as git should get it: a local path is made
// absolute, and a URL stays as written.
func (r *resolver) input(label, input, dir string) string {
	switch {
	case input == "":
		r.addf("%s: input is required", label)
	case strings.HasPrefix(input, "-"):
		// git gets inputs after "--", but a leading "-" is a mistake either way.
		r.addf("%s: input starts with \"-\", which git would read as an option", label)
	case isLocal(input):
		return r.path(label, "input", input, dir)
	}
	return input
}

// output returns the absolute output directory, and checks that no other
// repository has it.
func (r *resolver) output(label, output, dir string) string {
	if output == "" {
		r.addf("%s: output is required", label)
		return ""
	}
	abs := r.path(label, "output", output, dir)
	if abs == "" {
		return ""
	}
	key := dirKey(abs)
	switch other, ok := r.outputs[key]; {
	case !ok:
		r.outputs[key] = outputUse{label, abs}
	case other.path == abs:
		r.addf("%s: output %s is already used by %s", label, abs, other.label)
	default:
		r.addf("%s: output %s is the same directory as %s, already used by %s", label, abs, other.path, other.label)
	}
	return abs
}

// caseInsensitive is whether file names usually ignore case here. Then two
// outputs that differ only in case are the same directory.
var caseInsensitive = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// dirKey returns a key that's the same for two paths to the same directory:
// symlinks in the part of path that exists are resolved, and where file names
// usually ignore case, the key is in lower case.
func dirKey(path string) string {
	key := path
	for dir, rest := path, ""; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			key = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	if caseInsensitive {
		key = strings.ToLower(key)
	}
	return key
}

// path returns p as an absolute path: "~/" expands to the home directory, and
// a relative path resolves against dir. It returns "" for an empty p, or if
// "~/" can't be expanded.
func (r *resolver) path(label, key, p, dir string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if r.home == "" {
			r.addf("%s: %s %q: can't expand ~ because HOME isn't set", label, key, p)
			return ""
		}
		p = filepath.Join(r.home, p[1:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// checkSettings checks the values s sets, whether or not they end up used, so
// a config that works in one mode works in the other.
func (r *resolver) checkSettings(label string, s Settings) {
	if s.Schedule != "" {
		if _, err := schedule.Parse(s.Schedule); err != nil {
			r.addf("%s: schedule %q: %v", label, s.Schedule, err)
		}
	}
	if s.Retention != nil && *s.Retention < 0 {
		r.addf("%s: retention must be 0 or more, got %d", label, *s.Retention)
	}
	if s.Timeout != nil && *s.Timeout <= 0 {
		r.addf("%s: timeout must be more than 0, got %v", label, *s.Timeout)
	}
}

// checkKey checks that key, a resolved ssh_key, is a private key file that
// OpenSSH will use: a regular file it can read, which other users can't
// access if the current user owns it. OpenSSH refuses an own key that other
// users can access, but not one that another user, such as root, owns.
func (r *resolver) checkKey(label, key string) {
	if key == "" {
		return // "~/" couldn't be expanded, already reported
	}
	info, err := os.Stat(key)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.addf("%s: ssh_key %s doesn't exist", label, key)
	case err != nil:
		r.addf("%s: ssh_key: %v", label, err)
	case !info.Mode().IsRegular():
		r.addf("%s: ssh_key %s isn't a regular file", label, key)
	case info.Mode().Perm()&0o077 != 0 && ownedByMe(info):
		r.addf("%s: ssh_key %s has permissions %04o, and OpenSSH refuses a private key that other users can access: run chmod 600 %s",
			label, key, info.Mode().Perm(), key)
	default:
		f, err := os.Open(key)
		if err != nil {
			if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
				err = pathErr.Err
			}
			r.addf("%s: ssh_key %s can't be read: %v", label, key, err)
			return
		}
		_ = f.Close() // opened read-only: nothing to lose
	}
}

// label names a repository in problems: what, then its input if it has one,
// without credentials.
func label(what, input string) string {
	if input == "" {
		return what
	}
	return what + " (" + Redact(input) + ")"
}

// Redact hides credentials in a URL input, for logs and messages. For http,
// https, ftp and ftps it replaces the whole user part, since a token can be
// the user name on its own. For other schemes, such as ssh, it keeps the user
// name and replaces a password. Other inputs come back unchanged.
func Redact(input string) string {
	before, secret, after := splitSecret(input)
	if secret == "" {
		return input
	}
	return before + "xxxxx" + after
}

// CredentialReplacements returns old, new pairs for strings.NewReplacer that
// hide input's credentials in text that quotes it, or nil if it has none.
// Besides the whole input, they hide the credentials, and the password alone,
// wherever "@<host>" follows them, as in a URL quoted without its scheme or
// cut short. Anchored on the host, they can't rewrite ordinary text. A text
// cut inside the credentials still shows their end.
func CredentialReplacements(input string) []string {
	_, secret, _ := splitSecret(input)
	if secret == "" {
		return nil
	}
	user, _, host, _ := userinfo(input)
	pairs := []string{input, Redact(input), secret + "@" + host, "xxxxx@" + host}
	if _, password, ok := strings.Cut(user, ":"); ok && password != secret {
		pairs = append(pairs, password+"@"+host, "xxxxx@"+host)
	}
	return pairs
}

// splitSecret splits a URL input around its credentials: the whole user part
// for http, https, ftp and ftps, the password for other schemes.
func splitSecret(input string) (before, secret, after string) {
	user, start, _, ok := userinfo(input)
	if !ok {
		return input, "", ""
	}
	end := start + len(user)
	switch scheme, _, _ := strings.Cut(input, "://"); strings.ToLower(scheme) {
	case "http", "https", "ftp", "ftps":
	default:
		colon := strings.IndexByte(user, ':')
		if colon < 0 {
			return input, "", "" // a user name, not a secret
		}
		start += colon + 1
	}
	return input[:start], input[start:end], input[end:]
}

// userinfo returns the user part of a URL input, "user" or "user:password",
// where it starts, and the host after it. ok is false if there is none.
func userinfo(input string) (user string, start int, host string, ok bool) {
	if !isURL(input) {
		return "", 0, "", false
	}
	scheme, rest, _ := strings.Cut(input, "://")
	authority := rest
	if before, _, ok := strings.Cut(rest, "/"); ok {
		authority = before
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return "", 0, "", false
	}
	return authority[:at], len(scheme) + len("://"), authority[at+1:], true
}

// isLocal reports whether git reads input as a local path: it isn't a URL
// ("scheme://…") and isn't scp-like ("host:path", a colon before any slash).
// These are git's own rules (is_url and url_is_local_not_ssh in connect.c).
func isLocal(input string) bool {
	if isURL(input) {
		return false
	}
	colon := strings.IndexByte(input, ':')
	slash := strings.IndexByte(input, '/')
	return colon < 0 || (slash >= 0 && slash < colon)
}

// isURL reports whether s starts with "scheme://", where the scheme is a
// letter followed by letters, digits, "+", "-" or ".".
func isURL(s string) bool {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || scheme == "" || !strings.HasPrefix(rest, "//") {
		return false
	}
	for i, c := range scheme {
		letter := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		other := '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'
		if !letter && (i == 0 || !other) {
			return false
		}
	}
	return true
}
