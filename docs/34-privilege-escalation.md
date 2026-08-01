# Privilege Escalation

## 1. Overview

The root daemon owns the data plane (TUN device, routing tables, DNS), which requires
root/administrator privileges, while the CLI and user daemon run unprivileged. The
`pkg/daemon/elevate` package is the small, platform-split layer that (a) reports whether the
current process is already privileged and (b) launches a process — usually `kubevpn daemon
--sudo` or a self-upgrade re-exec — with elevated rights.

It is deliberately tiny and dependency-free (only `golang.org/x/sys` + `plog`), because it is
called very early in process startup, before any client or kube config exists.

## 2. API Surface

| Function | Platform | Purpose |
|---|---|---|
| `IsAdmin() bool` | both | Is the current process root/admin? |
| `RunCmd(exe, args)` | both | Launch a detached child **without** elevation |
| `RunCmdWithElevated(exe, args)` | both | Launch a detached child **with** elevation |
| `RunWithElevated()` | both | Re-exec **the current command** (`os.Args`) elevated, then wait |
| `RunWithElevatedInnerExec()` | windows | Alternate elevation via PowerShell `Start -Verb Runas` |
| `Kill(cmd)` | windows | `TASKKILL /T /F /PID` a process tree |

`EnvDisableSyncthingLog` (`config.go`, value `LOGGER_DISCARD`) is exported and set on every
elevated/detached launch — Syncthing cannot redirect its logger, so this env tells it to discard
output instead of polluting stdout.

## 3. Detecting Privilege (`IsAdmin`)

- **Unix** (`elevatecheck_others.go`): `os.Getuid() == 0`.
- **Windows** (`elevatecheck_windows.go`): attempts to `os.Open("\\\\.\\PHYSICALDRIVE0")` —
  opening the raw physical drive succeeds only for administrators, so a successful open (then
  close) means admin. There is no UID concept on Windows.

## 4. Launching Children (`RunCmd` / `RunCmdWithElevated`)

### Unix (`elevate_others.go`)

- `RunCmdWithElevated`: `sudo --preserve-env=HOME --background <exe> <args...>`. `--preserve-env=HOME`
  keeps `$HOME` so the elevated process finds `~/.kube` / `~/.kubevpn`; `--background` detaches.
- `RunCmd`: runs `<exe>` directly with `SysProcAttr{Setpgid: true}` so the child gets its own
  process group and survives the parent.
- Both wire stdio through, append `LOGGER_DISCARD=1`, `cmd.Start()`, then `cmd.Process.Release()`
  to fully detach (no zombie, no parent wait).

### Windows (`elevate_windows.go`)

- `RunCmdWithElevated`: `windows.ShellExecute(0, "runas", exe, args, cwd, SW_NORMAL)` — the
  `runas` verb triggers the UAC consent prompt.
- `RunCmd`: same call with verb `open` (no elevation).
- Args are space-joined into a single string (Windows `ShellExecute` takes one arg string).

## 5. Re-Exec Current Command (`RunWithElevated`)

Used by **self-upgrade** (`pkg/upgrade/upgrade.go` → `elevatePermission`) when the install
directory is not writable.

- **Unix**: `sudo --preserve-env=HOME <os.Args...>` run **synchronously** (`cmd.Run`, not detached
  — the caller `os.Exit(0)`s afterward). A goroutine `signal.Notify`s on
  `SIGHUP/SIGINT/SIGTERM/SIGQUIT` and swallows the first one: this **mutes a single Ctrl-C** in the
  parent so the elevated inner command handles it and its output is not cut off mid-flush.
  `SIGKILL`/`SIGSTOP` are uncatchable and intentionally not listed.
- **Windows**: `ShellExecute("runas", exe, os.Args[1:], cwd, SW_NORMAL)`.
  `RunWithElevatedInnerExec` is an alternative that builds a PowerShell
  `Powershell Start "<exe args>" -Verb Runas` command line and runs it via `windows.CreateProcess`,
  then waits on the spawned PID (kept for the "still can't use env KUBECONFIG" edge case).

## 6. Call Sites

| Caller | Call | Why |
|---|---|---|
| `daemon/client.go` `runDaemon` (sudo, not admin) | `RunCmdWithElevated(exe, ["daemon","--sudo"])` | start the root daemon with elevation |
| `daemon/client.go` `runDaemon` (sudo, already admin) | `RunCmd(exe, ["daemon","--sudo"])` | already root, no prompt needed |
| `daemon/client.go` `runDaemon` (user) | `RunCmd(exe, ["daemon"])` | unprivileged user daemon |
| `upgrade/upgrade.go` `elevatePermission` | `RunWithElevated()` | re-exec the upgrade with write access |

After launching the daemon, `runDaemon` polls the PID file (50ms ticks) until it appears, then
re-acquires the gRPC client — see [35-daemon-bootstrap.md](35-daemon-bootstrap.md).

## 7. Related Files

| File | Purpose |
|---|---|
| `pkg/daemon/elevate/elevate_others.go` | Unix `RunCmd` / `RunCmdWithElevated` |
| `pkg/daemon/elevate/elevate_windows.go` | Windows `RunCmd` / `RunCmdWithElevated` / `Kill` |
| `pkg/daemon/elevate/elevatecheck_others.go` | Unix `IsAdmin` / `RunWithElevated` |
| `pkg/daemon/elevate/elevatecheck_windows.go` | Windows `IsAdmin` / `RunWithElevated` / `RunWithElevatedInnerExec` |
| `pkg/daemon/elevate/config.go` | `EnvDisableSyncthingLog` constant |

## 8. Related Docs

- [02-dual-daemon.md](02-dual-daemon.md) — why a privileged root daemon exists
- [35-daemon-bootstrap.md](35-daemon-bootstrap.md) — `runDaemon` / `StartupDaemon` lifecycle
- [33-client-upgrade.md](33-client-upgrade.md) — `elevatePermission` re-exec path
