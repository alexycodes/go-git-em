// Command go-git-em backs up Git repositories. It keeps a mirror of each
// repository and writes Git bundle snapshots of it on a schedule.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
	// git runs in its own session, so Ctrl-C, or a closed terminal, doesn't
	// reach it: cancelling the context is what stops it, and lets it remove its
	// lock files. Signals stay caught until run returns: exiting sooner would
	// leave git running. That takes at most gitcmd.DefaultWaitDelay.
	ctx, stop := context.Background(), func() {}
	if sigs := stopSignals(); len(sigs) > 0 { // none would mean all
		ctx, stop = signal.NotifyContext(ctx, sigs...)
	}
	// For the same reason, a closed stderr, such as a tee that Ctrl-C ended,
	// must make log writes fail rather than kill go-git-em.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// stopSignals returns the signals that stop the backups, without SIGHUP or
// SIGINT if the process was started to ignore them, as nohup and background
// shells do: catching them would undo that. Go keeps only those two ignored.
func stopSignals() []os.Signal {
	var sigs []os.Signal
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	return sigs
}
