// Command gge is go-git-em's command: it backs up Git repositories. It keeps a
// mirror of each repository and writes Git bundle snapshots of it on a schedule.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	// Embedded time zone data, so TZ works on hosts and images without it.
	_ "time/tzdata"
)

// Build information, set by GoReleaser with -ldflags "-X main.version=…".
var (
	version string
	commit  string
	date    string
)

func main() {
	// Ctrl-C doesn't reach git, which runs in its own session or process group:
	// cancelling ctx is what stops it. Exiting before run returns would leave
	// git running, or on Windows kill it.
	ctx, stop := context.Background(), func() {}
	if sigs := stopSignals(); len(sigs) > 0 { // none would mean all
		ctx, stop = signal.NotifyContext(ctx, sigs...)
	}
	// A closed stderr, such as a tee that Ctrl-C ended, must not kill go-git-em
	// while git runs: log writes fail instead.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// stopSignals returns the signals that stop the backups, without SIGHUP or
// SIGINT if the process started with them ignored (nohup, background jobs).
func stopSignals() []os.Signal {
	var sigs []os.Signal
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	return sigs
}
