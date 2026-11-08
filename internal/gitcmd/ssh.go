package gitcmd

import "strings"

// sshCommand returns the GIT_SSH_COMMAND that makes base use key, and only key.
// git runs the command through sh, so the key path is shell-quoted. OpenSSH
// keeps the first value it gets for an option, so options already in base win.
func sshCommand(base, key string) string {
	return base + " -i " + shellQuote(key) + " -o IdentitiesOnly=yes"
}

// shellQuote quotes s as a single sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
