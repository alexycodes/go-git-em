<img src="assets/icon.svg" alt="" width="96" height="96">

# go-git-em

Back up Git repositories. go-git-em keeps a mirror of each repository and, when it changes, saves a snapshot that `git clone` can restore. Run it once, or as a daemon on a schedule.

## Install

```sh
go install github.com/alexycodes/go-git-em/cmd/gge@latest
```

Building needs Go 1.27.3 or later. Running needs `git` 2.29 or later, and `ssh` for ssh URLs. Your git and ssh configuration applies.

## Quick start

```yaml
# go-git-em.yaml
defaults:
  schedule: "0 3 * * *"   # every day at 03:00
  retention: 30           # keep the 30 latest snapshots
repositories:
  - input: https://github.com/example/my-project.git
    output: ./backups/my-project
  - input: git@github.com:example/other.git
    output: ./backups/other
    schedule: "0 * * * *" # every hour
```

In the same directory:

```sh
gge validate       # validate the config
gge backup         # back up everything now, then exit
gge cron           # back up on the schedules, until stopped
gge check          # check the backups made so far
```

## Configuration

Set each key on a repository, or under `defaults` for all of them. Relative paths are relative to the config file. On Windows, don't put a path with backslashes in double quotes, where YAML reads `\` as an escape: leave it unquoted, or use forward slashes.

| Key | Default | |
|---|---|---|
| `input` | required | Repository URL (`https://`, `ssh://`, `git@host:path`, `file://`) or local path |
| `output` | required | Directory for this repository's backups, one per repository |
| `schedule` | none | When `gge cron` backs it up: the 5 cron fields, in quotes, such as `"0 3 * * *"` (every day at 03:00) or `"*/15 * * * *"`. In the local time zone, which `TZ` sets on Linux and macOS, such as `TZ=Europe/Paris`. Only `gge cron` needs it. |
| `retention` | keep all | How many snapshots to keep |
| `timeout` | `30m` | Longest a backup may run, its checks included. `gge check` gives each snapshot, and the mirror, as long. |
| `ssh_key` | ssh's own | Private key for ssh URLs |
| `test_restore` | `true` | Restore each new snapshot as a test before keeping it. Takes about as long as a clone, within `timeout`, and as much free space as the snapshot while it runs. |

Commands:

| Command | |
|---|---|
| `gge backup` | Back up everything once, then exit |
| `gge cron` | Back up each repository on its schedule, until stopped |
| `gge check` | Check the backups made so far: every snapshot's checksum, and the mirror's data. `--full` also restores every snapshot as a test. Deletes no backup. |
| `gge validate` | Validate the config and git, print the repositories to back up, then exit. A repository without a schedule gets a warning. |
| `gge version` | Print the version |

Flags go after the command, and `gge <command> -h` lists them:

| Flag | Default | `backup` | `cron` | `check` | `validate` | |
|---|---|:-:|:-:|:-:|:-:|---|
| `--config` | `go-git-em.yaml` | ✓ | ✓ | ✓ | ✓ | Config file. By default, `go-git-em.yaml` in the current directory, unless `--input` is given. |
| `--input`, `--output` | | ✓ | ✓ | ✓ | ✓ | One repository without a config file |
| `--schedule` | | | ✓ | | ✓ | The `--input` repository's `schedule` |
| `--retention` | | ✓ | ✓ | | ✓ | The `--input` repository's `retention` |
| `--ssh-key` | | ✓ | ✓ | | ✓ | Default `ssh_key` |
| `--parallel` | `4` | ✓ | ✓ | ✓ | | Repositories at once |
| `--run-on-start` | `true` | | ✓ | | | Back up everything at startup |
| `--full` | | | | ✓ | | Also restore every snapshot as a test |
| `--log-level` | `info` | ✓ | ✓ | ✓ | ✓ | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | ✓ | ✓ | ✓ | ✓ | `text` or `json` |

Every flag can also be set with an environment variable: `GOGITEM_` and the flag's name, such as `GOGITEM_LOG_LEVEL`. A command ignores the variables of flags it doesn't take.

## Backups

```
<output>/
  mirror.git/                      the mirror, updated on each run
  snapshots/
    20261101T030000.000Z.bundle    the repository at that time (UTC)
```

- A snapshot is written only when something changed. With a `retention`, the oldest ones are deleted.
- A new snapshot is kept only if a test restore of it succeeds, unless `test_restore` is `false`. A run that finds no change checks the newest snapshot's checksum, and takes a new one if it's damaged.
- Branches deleted or force-pushed upstream stay in the older snapshots.
- Each snapshot is a full copy, so plan for about `retention` × the repository's size.
- Run `gge check` now and then, such as weekly, to check the older snapshots and the mirror too. Add `--full` to also find damage that a checksum can't, in snapshots taken with `test_restore: false`. A backup due meanwhile skips the output being checked.

To restore, clone a snapshot:

```sh
git clone backups/my-project/snapshots/20261101T030000.000Z.bundle my-project
```

## Running it

- `gge backup` exits 1 if anything wasn't backed up. Use it from your system's cron, a systemd timer or a Kubernetes CronJob.
- `gge check` exits 1 if anything is damaged, or wasn't checked: unreadable, missing, interrupted, or locked by another go-git-em.
- `gge cron` runs until it's stopped, then exits 0. A failed backup is logged and tried again when it's next due.
- Exit code 2 means a usage or configuration error, or git is missing or too old.
- Logs go to stderr, one line per backup or check.
- Ctrl-C or SIGTERM (on Windows, closing the console too) stops the backups in progress. The snapshots already written stay.
- If two go-git-em processes use the same output, the second skips it. On a network file system that can't lock, go-git-em warns and goes on.

`gge cron` doesn't start itself at boot: run it as a systemd service, a container with a restart policy, or a Kubernetes Deployment. In a container, run it under an init, such as `docker run --init`.

## SSH

- The host must already be in `known_hosts`: go-git-em never prompts.
- Your key file must be readable only by you (`chmod 600`; on Windows, OpenSSH checks this itself). A key with a passphrase needs ssh-agent.

## Caveats

- **Git LFS** files aren't backed up, only their pointers. Restore with `GIT_LFS_SKIP_SMUDGE=1 git clone …`.
- **Pull request refs** (`refs/pull/*` on GitHub) are backed up too, so a busy repository gets a new snapshot on almost every run.
- **Not backed up:** submodules (add each one as its own repository), issues, wikis and other project data.
- **A local repository owned by another user** is refused by git ("dubious ownership"). Run go-git-em as its owner, or add it to git's `safe.directory`.
- **A broken mirror** fails every run. Damaged data in it fails the runs that write a snapshot, and the error usually says so. With `test_restore: false`, some damage reaches the snapshots unnoticed. Either way, delete `<output>/mirror.git`, and the next run clones it again.
- **Credentials in a URL** are kept in `mirror.git/config`. Prefer ssh keys or a credential helper.
- **Don't `kill -9` it:** git would keep running, and the next run could clash with it. On Windows, git is killed with it.

## AI Disclaimer

This project is developed with the help of AI coding tools. Much of the code, tests and documentation was AI-generated. Review it before you rely on it for your backups.

## License

Licensed under the GNU General Public License v3.0 or later. See [LICENSE](LICENSE).
