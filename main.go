// Command go-git-em backs up Git repositories. It keeps a mirror of each
// repository and writes Git bundle snapshots of it on a schedule.
package main

import (
	"context"
	"os"

	// Embedded time zone data, so CRON_TZ works on hosts and images without it.
	_ "time/tzdata"
)

// Build information. GoReleaser sets these with -ldflags "-X main.version=…".
// When they're empty, versionString falls back to what the Go toolchain recorded.
var (
	version string
	commit  string
	date    string
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
