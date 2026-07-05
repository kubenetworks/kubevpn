# Client Self-Upgrade

## 1. Overview

`kubevpn upgrade` replaces the running `kubevpn` binary on disk with the latest GitHub release.
It resolves the latest version, compares it against the compiled-in `config.Version`, and — if
newer — quits both daemons, downloads and atomically installs the new binary (with rollback on
failure), then restarts the daemon. Privilege escalation is requested up front when the install
directory is not writable.

This is **distinct** from the `Upgrade` RPC described in
[14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md): that RPC negotiates the *client↔daemon
version match* at connect time; this doc covers swapping the *executable on disk*.

Entry point: `upgrade.Main` (`pkg/upgrade/upgrade.go`), wired by `CmdUpgrade`
(`cmd/kubevpn/cmds/upgrade.go`).

## 2. Flow

```
upgrade.Main(ctx, quit)                                pkg/upgrade/upgrade.go
  │
  ├── elevatePermission()         ── writable check on exe dir; re-exec via sudo/UAC if denied
  │
  ├── build http.Client           ── 30s timeout; OAuth Bearer if config.GitHubOAuthToken set
  │
  ├── needsUpgrade(client, config.Version)
  │      ├── util.GetManifest(GOOS, GOARCH)   ── GitHub releases/latest API → tag + asset URL
  │      │      └── fallback: plugins/stable.txt + constructed release URL
  │      └── semver compare (hashicorp/go-version): curr >= latest ⇒ no upgrade
  │
  ├── !needsUpgrade ⇒ print "Already up to date" and RETURN
  │
  ├── quit(ctx, true)  +  quit(ctx, false)   ── stop sudo daemon, then user daemon
  │
  ├── downloadAndInstall(client, url)         ── see §4 (atomic swap + rollback)
  │
  └── daemon.StartupDaemon(context.Background())  ── relaunch both daemons
```

## 3. Version Resolution (`needsUpgrade` / `util.GetManifest`)

- `GetManifest` tries the GitHub releases-latest API for both repo mirrors
  (`kubenetworks/kubevpn`, then `wencaiwulue/kubevpn`), unmarshals the release JSON, and picks the
  asset whose name contains both the OS and the arch. For non-Windows/Darwin it falls back to a
  `linux`+arch asset.
- If the API is unreachable, `needsUpgrade` falls back to the plain-text
  `plugins/stable.txt` and constructs the release zip URL by convention.
- Versions are compared with `github.com/hashicorp/go-version`; `curr.GreaterThanOrEqual(latest)`
  means "no upgrade needed". An unparseable version surfaces as an error.

## 4. Atomic Install with Rollback (`downloadAndInstall`)

The install is ordered so the user is **never left without a binary**:

1. Download the release zip to an OS temp file (`util.Download`, progress bar).
2. `util.UnzipKubeVPNIntoFile` extracts the `kubevpn` entry into a fresh temp file **in the same
   directory as the current executable** (so the final move is a same-filesystem rename, not a
   cross-device copy).
3. `os.Chmod(newFile, config.FileModeExecutable)`.
4. Reserve a unique `backupPath` next to the executable.
5. `util.Move(curBinary → backupPath)` — stash the current binary.
6. `util.Move(newFile → curBinary)` — install. **On failure, immediately
   `util.Move(backupPath → curBinary)` to restore**, then return the error.
7. On success, delete the backup.

All temp files are cleaned up via `defer os.Remove`.

## 5. Privilege Escalation (`elevatePermission`)

Before doing anything, `upgrade.Main` probes whether the executable's directory is writable by
creating and deleting a `.test` file. If that fails with `os.IsPermission`, it calls
`elevate.RunWithElevated()` to re-launch the whole command with elevated rights and `os.Exit(0)`s
the unprivileged process. See [34-privilege-escalation.md](34-privilege-escalation.md).

## 6. Error Classification

Failures are wrapped with sentinels from `pkg/config/errors.go` so the CLI exit code reflects the
cause (see [31-exit-codes.md](31-exit-codes.md)):

| Sentinel | When |
|---|---|
| `ErrUpgradeNetwork` | manifest/API fetch or download failed |
| `ErrUpgradeInstall` | unzip, chmod, backup, or install move failed |
| `ErrUpgradeUnsupportedPlatform` | no release asset matches this OS/arch |

## 7. Related Files

| File | Purpose |
|---|---|
| `pkg/upgrade/upgrade.go` | `Main`, `downloadAndInstall`, `elevatePermission`, `needsUpgrade` |
| `pkg/util/upgrade.go` | `GetManifest`, `Download`, `UnzipKubeVPNIntoFile`, mirror addresses |
| `pkg/util/util.go` | `Move` (same-dir rename helper) |
| `pkg/util/file.go` | `DownloadFileStream` (stable.txt fallback) |
| `cmd/kubevpn/cmds/upgrade.go` | `CmdUpgrade` command wiring |
| `pkg/config/errors.go` | `ErrUpgrade*` sentinels |

## 8. Related Docs

- [34-privilege-escalation.md](34-privilege-escalation.md) — how `elevatePermission` re-execs
- [35-daemon-bootstrap.md](35-daemon-bootstrap.md) — `StartupDaemon` relaunch after install
- [14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md) — the unrelated `Upgrade` version RPC
- [31-exit-codes.md](31-exit-codes.md) — `ErrUpgrade*` exit code mapping
