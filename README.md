<img src="assets/icon.svg" alt="" width="96" height="96">

# go-git-em

Back up Git repositories. go-git-em keeps a mirror of each repository and, when it changes, saves a snapshot that `git clone` can restore. Run it once, or as a daemon on a schedule.

## Install

```sh
go install github.com/alexycodes/go-git-em/cmd/gge@latest
```

Building needs Go 1.27.3 or later. Running needs `git`, and `ssh` for ssh URLs. Your git and ssh configuration applies.

## Quick start

```yaml
# backups.yaml
defaults:
  schedule: "0 3 * * *"   # every day at 03:00
  retention: 30           # keep the 30 latest snapshots
repositories:
  - input: https://github.com/example/project.git
    output: ./backups/project
  - input: git@github.com:example/other.git
    output: ./backups/other
    schedule: "@hourly"
```

```sh
gge -config backups.yaml -check   # check the config
gge -config backups.yaml -once    # back up everything now, then exit
gge -config backups.yaml          # run as a daemon, on the schedules
```

## Configuration

Set each key on a repository, or under `defaults` for all of them. Relative paths are relative to the config file.

| Key | Default | |
|---|---|---|
| `input` | required | Repository URL (`https://`, `ssh://`, `git@host:path`, `file://`) or local path |
| `output` | required | Directory for this repository's backups, one per repository |
| `schedule` | none | When the daemon backs it up: cron (`0 3 * * *`), `@hourly`, `@daily`, `@every 6h`… Prefix it with `CRON_TZ=Europe/Paris ` for a time zone. Required unless you use `-once`. |
| `retention` | keep all | How many snapshots to keep |
| `timeout` | `30m` | Longest a backup may run |
| `ssh_key` | ssh's own | Private key for ssh URLs |

Flags:

| Flag | Default | |
|---|---|---|
| `-config` | | Config file |
| `-input`, `-output` | | Back up one repository without a config file. `-schedule` and `-retention` go with them. |
| `-once` | | Back up everything once, then exit |
| `-parallel` | `4` | Backups at once |
| `-run-on-start` | `true` | Daemon: back up everything at startup |
| `-ssh-key` | | Default `ssh_key` |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `-log-format` | `text` | `text` or `json` |
| `-check` | | Check the config and print it, then exit |
| `-version` | | Print the version |

Every flag but `-check` and `-version` can also be set with an environment variable: `GOGITEM_` and the flag's name, such as `GOGITEM_LOG_LEVEL`.

## Backups

```
<output>/
  mirror.git/                      the mirror, updated on each run
  snapshots/
    20261101T030000.000Z.bundle    the repository at that time (UTC)
```

- A snapshot is written only when something changed. With a `retention`, the oldest ones are deleted.
- Branches deleted or force-pushed upstream stay in the older snapshots.
- Each snapshot is a full copy, so plan for about `retention` × the repository's size.

To restore, clone a snapshot:

```sh
git clone backups/project/snapshots/20261101T030000.000Z.bundle project
```

## Running it

- `-once` exits 1 if anything wasn't backed up. Use it from cron, a systemd timer or a Kubernetes CronJob.
- The daemon runs until it's stopped, then exits 0. A failed backup is logged and tried again when it's next due.
- Exit code 2 means a configuration error, or no git.
- Logs go to stderr, one line per backup.
- Ctrl-C or SIGTERM stops the backups in progress. The snapshots already written stay.
- If two go-git-em processes use the same output, the second skips it. On a network file system that can't lock, go-git-em warns and goes on.

The daemon doesn't start itself at boot: run it as a systemd service, a container with a restart policy, or a Kubernetes Deployment. In a container, run it under an init, such as `docker run --init`.

## SSH

- The host must already be in `known_hosts`: go-git-em never prompts.
- Your key file must be readable only by you (`chmod 600`). A key with a passphrase needs ssh-agent.

## Caveats

- **Git LFS** files aren't backed up, only their pointers. Restore with `GIT_LFS_SKIP_SMUDGE=1 git clone …`.
- **Pull request refs** (`refs/pull/*` on GitHub) are backed up too, so a busy repository gets a new snapshot on almost every run.
- **Not backed up:** submodules (add each one as its own repository), issues, wikis and other forge data.
- **A local repository owned by another user** is refused by git ("dubious ownership"). Run go-git-em as its owner, or add it to git's `safe.directory`.
- **A broken mirror** fails every run. Delete `<output>/mirror.git`, and the next run clones it again.
- **Credentials in a URL** are kept in `mirror.git/config`. Prefer ssh keys or a credential helper.
- **Don't `kill -9` it:** git would keep running, and the next run could clash with it.

## AI Disclaimer

This project is developed with the help of AI coding tools. Much of the code, tests and documentation was AI-generated. Review it before you rely on it for your backups.

## License

Licensed under the GNU General Public License v3.0 or later. See [LICENSE](LICENSE).
