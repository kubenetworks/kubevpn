# Config & Alias

## 1. Overview

`kubevpn alias` lets users name a kubevpn invocation (with its flags) in a YAML config file and
run it by short name — like an SSH `~/.ssh/config` alias. It also supports a **dependency chain**
(`Needs`): if cluster A is only reachable through cluster B, `alias A` first runs B, then A.

A companion feature, the `--kubeconfig-json` flag, lets a full kubeconfig be passed *inline* as
JSON (no file on disk); kubevpn materializes it to a temp file transparently. This is especially
useful inside alias configs so a self-contained alias needs no separate kubeconfig path.

Entry point: `CmdAlias` (`cmd/kubevpn/cmds/alias.go`). The CLI itself is frozen; this doc describes
the existing behavior.

## 2. Config File

Source resolution order (`ParseAndGet`):

1. `--kubevpnconfig` / `-f` flag (or the `KUBEVPNCONFIG` env var) — a local path.
2. `--remote` / `-r` flag — a URL fetched via `util.DownloadFileStream`.
3. Default `~/.kubevpn/config.yaml` (`config.GetConfigFile()`).

The file is **multi-document YAML** (`---`-separated), each document a `Config`:

```yaml
Name: dev
Description: This is dev k8s environment
Needs: jumper
Flags:
  - connect
  - --kubeconfig=~/.kube/config
  - --namespace=default
---
Name: jumper
Description: This is jumper k8s environment
Flags:
  - connect
  - --kubeconfig=~/.kube/jumper_config
  - --namespace=test
```

```go
type Config struct {
    Name        string   `yaml:"Name"`
    Description string   `yaml:"Description"`
    Needs       string   `yaml:"Needs,omitempty"`
    Flags       []string `yaml:"Flags,omitempty"`
}
```

`Flags` is the full argv kubevpn will be re-invoked with (first element is the subcommand, e.g.
`connect`).

## 3. Dependency Resolution (`GetConfigs`)

`GetConfigs` walks the `Needs` chain to produce an **ordered** list, dependency-first:

```
GetConfigs(configs, name)
  m = {Name → Config}
  seen = {}
  loop:
    if name ∈ seen ⇒ "loop jump detected: a -> b -> a"   ── cycle guard
    cfg = m[name]; if absent ⇒ return what we have
    result = [cfg] ++ result          ── prepend so deps run first
    seen += name
    name = cfg.Needs; if "" ⇒ return result
```

- The result is prepended (`[]Config{cfg}` ahead of `result`), so for `dev → jumper` the returned
  order is `[jumper, dev]` — the dependency connects first.
- Cycles are detected via a `seen` set (`sets.New[string]`); a back-edge returns a
  `loop jump detected: …` error with the path.
- An unknown alias name yields a helpful error listing the available names.

## 4. Execution (`CmdAlias.RunE`)

For each resolved `Config` in order:

1. `ParseArgs(cmd, &conf)` — rewrites `Flags` to handle any inline `--kubeconfig-json` (§5).
2. Build `exec.Command(self, conf.Flags...)` where `self = os.Executable()` — i.e. **kubevpn
   re-execs itself** as a child with the alias's flags, wiring through stdio.
3. Print `Name` / `Description` / `Command`, then `c.Run()`.
4. Any child error aborts the chain.

So `alias` is a thin orchestrator: it does not embed connect logic, it just shells out to
`kubevpn <flags>` in the right order.

## 5. Inline Kubeconfig (`--kubeconfig-json`)

Two code paths materialize an inline JSON kubeconfig to a temp file via
`util.ConvertToTempKubeconfigFile`:

- **Normal commands** (`cmd/kubevpn/cmds/root.go`): the `--kubeconfig-json` persistent flag is
  parsed in the root `PersistentPreRun`; its JSON is written to a temp file and the standard
  `--kubeconfig` is pointed at it.
- **Alias** (`ParseArgs` in `status.go`): scans `conf.Flags` for a `--kubeconfig-json=…`, removes
  it from the slice, writes the JSON to `~/.kubevpn/temp/<AliasName>` (`config.GetTempPath()` +
  alias name), and appends `--kubeconfig=<that file>`. Naming the temp file by alias keeps each
  alias's materialized kubeconfig stable and distinct.

This is what makes an `all-in-one` alias possible — the kubeconfig travels inside the alias config
itself rather than referencing an external path.

## 6. Related Files

| File | Purpose |
|---|---|
| `cmd/kubevpn/cmds/alias.go` | `CmdAlias`, `ParseAndGet`, `ParseConfig`, `GetConfigs`, `Config` type |
| `cmd/kubevpn/cmds/status.go` | `ParseArgs`, `parseKubeconfigJson` (inline kubeconfig in alias flags) |
| `cmd/kubevpn/cmds/root.go` | `--kubeconfig-json` persistent flag + temp-file conversion |
| `pkg/util/kubeconfig.go` | `ConvertToTempKubeconfigFile` |
| `pkg/util/file.go` | `DownloadFileStream` (remote config) |
| `pkg/config` | `GetConfigFile`, `GetTempPath` |

## 7. Related Docs

- [14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md) — the subcommands an alias ultimately drives
- [15-ssh-architecture.md](15-ssh-architecture.md) — `Needs` is commonly used for SSH-jump-reachable clusters
