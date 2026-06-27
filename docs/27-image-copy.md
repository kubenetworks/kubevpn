# Image Copy Design

## 1. Overview

The image copy feature (`kubevpn image copy`) copies container images between registries using [regclient](https://github.com/regclient/regclient). It only pulls layers that do not exist at the target, attempts to mount layers between repositories within the same registry, and within the same repository only sends the manifest with the new tag.

Key capability: **DSN-style inline credentials with automatic TLS detection**. Users embed registry credentials directly in the image reference, and TLS support is probed automatically — no manual `http://` or `--insecure` flags needed.

## 2. DSN Reference Format

```
[user[:password]@]registry[:port]/namespace/repo[:tag][@digest]
└──── credentials ─┘└──────────── standard image reference ──────────┘
```

| Example | Parsed As |
|---------|-----------|
| `ghcr.io/kubenetworks/kubevpn:latest` | No credentials, standard ref |
| `admin:secret@registry.example.com/repo:v1` | User `admin`, password `secret` |
| `admin@registry.example.com/repo:v1` | User `admin`, no password |
| `user:pass@registry.com/repo@sha256:abcd...` | Credentials + digest ref |
| `registry.com/repo@sha256:abcd...` | No credentials, digest ref |
| `nginx:latest` | Docker Hub short name, no credentials |

### Disambiguation with `@`

The `@` character is overloaded — it separates credentials in the DSN format and separates the digest in standard OCI references. The parser distinguishes them:

1. Find the first `@` in the string
2. If the left side contains `/` → it's `registry/repo`, not credentials (e.g. `registry.com/repo@sha256:abc`)
3. If the right side has no `/` and matches the digest pattern → standard digest ref, no credentials
4. Otherwise → left side is `user[:pass]`, right side is the image reference

```
user:pass@registry.com/repo@sha256:abc
│         │                 │
│         │                 └─ second @: digest (standard OCI)
│         └─ image reference starts here
└─ credentials (first @ is DSN separator, left has no "/")

registry.com/repo@sha256:abc
│                 │
│                 └─ only @: digest (left side has "/", not credentials)
└─ full image reference
```

## 3. Automatic TLS Detection

When inline credentials are provided, `probeRegistryTLS` probes the registry to determine TLS support. This eliminates the need for `http://` prefixes or `--insecure` flags.

```
probeRegistryTLS(registry)
    │
    ├─ TLS dial to host:443 (InsecureSkipVerify=true, 5s timeout)
    │   ├─ success → TLSInsecure (HTTPS works, skip CA verification)
    │   └─ failure ↓
    │
    ├─ HTTP GET http://host:80/v2/ (5s timeout)
    │   ├─ success → TLSDisabled (plain HTTP works)
    │   └─ failure ↓
    │
    └─ default → TLSEnabled (assume HTTPS with full verification)
```

| Probe Result | `config.TLSConf` | Meaning |
|---|---|---|
| TLS handshake succeeds | `TLSInsecure` | Registry supports HTTPS; skip CA verification to handle self-signed certs |
| TLS fails, HTTP `/v2/` succeeds | `TLSDisabled` | Registry is plain HTTP only |
| Both fail | `TLSEnabled` | Default to strict HTTPS (may fail at copy time if registry is truly unreachable) |

When no inline credentials are provided, the registry uses Docker's default credential store (`~/.docker/config.json`) and the standard TLS configuration — no probing occurs.

## 4. Authentication Flow

```
TransferImageWithRegctl(ctx, src, dst)
    │
    ├─ parseImageDSN(src) → (srcRef, user, pass)
    ├─ parseImageDSN(dst) → (dstRef, user, pass)
    │
    ├─ Base options (always applied):
    │   ├─ WithDockerCerts()    ← /etc/docker/certs.d
    │   └─ WithDockerCreds()    ← ~/.docker/config.json
    │
    ├─ If src has inline creds:
    │   └─ buildHostConfig(srcRef, user, pass)
    │       ├─ extractRegistry(srcRef) → registry hostname
    │       ├─ probeRegistryTLS(registry) → TLS mode
    │       └─ WithConfigHost(host{User, Pass, TLS})
    │
    ├─ If dst has inline creds:
    │   └─ buildHostConfig(dstRef, user, pass) (same as above)
    │
    └─ regclient.ImageCopy(src, dst) with progress display
```

Inline credentials take precedence over Docker credential store for the same registry. Registries without inline credentials still use Docker's credential helpers as before.

## 5. Docker Dependency

**image copy does not depend on Docker.** It works on machines where Docker is not installed.

Authentication has two independent paths, listed by priority:

| Priority | Auth Method | Source | Docker Dependency |
|---|---|---|---|
| 1 (high) | DSN inline credentials | `user:pass@registry.com/repo:tag` | None — injected directly into regclient via `WithConfigHost` |
| 2 (low) | Docker credential store | `~/.docker/config.json` + credential helpers | Requires `docker login` to generate the config file |

When both point to the same registry, inline credentials override Docker credentials.

### Behavior Without Docker

```
WithDockerCreds()
    → config.DockerLoad()
        → open ~/.docker/config.json
            → file not found (fs.ErrNotExist) → return empty []Host{}, no error
    → log a warn message, continue

WithDockerCerts()
    → read /etc/docker/certs.d
        → directory not found → no certs loaded, continue
```

Both Docker-related options are additive — they load what is available and silently skip when files are missing, never blocking the workflow. DSN inline credentials go through `WithConfigHost`, a completely independent path with no Docker coupling.

### Scenario Matrix

| Scenario | Works? | Notes |
|---|---|---|
| Docker installed, `docker login` done, no inline creds | Yes | Reads `~/.docker/config.json` automatically |
| Docker installed, `docker login` done, with inline creds | Yes | Inline creds override Docker creds |
| No Docker, with inline creds | Yes | Inline creds work independently |
| No Docker, no inline creds, public image | Yes | No authentication needed |
| No Docker, no inline creds, private image | No | No credential source, registry returns 401 |

## 6. Progress Display

Image copy includes a real-time progress display:

- **Per-blob progress bars** showing percentage, transferred/total size
- **Manifest counter** (finished/total)
- **Aggregate stats**: copied bytes, skipped bytes (already at target), queued bytes
- **Elapsed time**

The display uses `ascii.Lines` for in-place terminal updates at 250ms intervals.

## 7. Related Files

| File | Purpose |
|------|---------|
| `cmd/kubevpn/cmds/imagecopy.go` | CLI command definition (`kubevpn image copy`) |
| `pkg/util/regctl/regctl.go` | `TransferImageWithRegctl`, `parseImageDSN`, `probeRegistryTLS`, `buildHostConfig` |
| `pkg/util/regctl/image.go` | `ImageProgress` — progress callback and display |
| `pkg/util/regctl/ascii/` | Terminal progress bar and line management |
| `pkg/util/regctl/regctl_test.go` | Unit tests for DSN parsing, registry extraction, TLS probing |
