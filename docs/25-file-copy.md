# File Copy Design

## 1. Overview

The cp package (`pkg/cp`) provides file copy between local filesystem and Kubernetes pods, similar to `kubectl cp` but with one key difference: **symlinks are dereferenced and their target data is copied** (`tar cfh` / `tar -xmfh`), whereas `kubectl cp` preserves symlinks as links. This ensures the local copy contains real file content rather than broken symlinks pointing to paths that only exist inside the container.

It uses tar streaming over `kubectl exec` for transfer, with retry support for large files.

## 2. Architecture

```
Copy to Pod:                          Copy from Pod:
  local file                            pod container
  → makeTar() → tar stream              → kubectl exec tar cfh -
  → kubectl exec tar -xmfh -            → TarPipe (retry-aware reader)
  → pod container                        → untarAll() → local files
```

## 3. File Spec Format

`[[namespace/]pod:]file/path`

| Example | Parsed As |
|---------|-----------|
| `/tmp/data` | Local file |
| `my-pod:/app/data` | Remote file in pod `my-pod` |
| `default/my-pod:/app/data` | Remote file in namespace `default`, pod `my-pod` |

Windows drive letters (e.g., `C:\Users\...`) are handled specially — `C:` is not treated as a pod name.

## 4. Copy to Pod (`copyToPod`)

1. Check if destination is a directory (via `kubectl exec test -d`)
2. If directory, append source base name to destination path
3. Create tar stream from local files via `makeTar()` / `recursiveTar()`
4. Pipe tar stream to `kubectl exec tar -xmfh -` in the pod
5. Supports `--no-same-permissions` / `--no-same-owner` flags

`recursiveTar()` handles:
- Regular files — tar header + file content
- Directories — recursive descent
- Symlinks — tar header with link target
- Glob patterns — via `filepath.Glob`

## 5. Copy from Pod (`copyFromPod`)

1. Create `TarPipe` — an `io.Reader` wrapping `kubectl exec tar cfh -`
2. `untarAll()` extracts the tar stream to the local filesystem
3. Validates path prefix to prevent tar path traversal attacks
4. Handles symlinks by copying the linked file content

### Retry Mechanism (`TarPipe`)

For large files, the exec connection may drop. `TarPipe` tracks bytes read and reconnects with `tail -c+{offset}`:

```
First attempt:  tar cfh - /app/data
Retry (at N):   sh -c "tar cfh - /app/data | tail -c+N"
```

Controlled by `MaxTries` (0 = no retry, -1 = unlimited).

## 6. Path Types

Two path types enforce correct behavior:
- `localPath` — local filesystem paths (OS-specific separators)
- `remotePath` — pod filesystem paths (always `/` separator)

Both implement `filePath` interface with `Dir()`, `Base()`, `Clean()`, `Join()`, `Glob()`, `String()` methods.

## 7. Related Files

| File | Purpose |
|------|---------|
| `pkg/cp/cp.go` | CopyOptions, Run, copyToPod, copyFromPod, TarPipe |
| `pkg/cp/filespec.go` | fileSpec, localPath, remotePath path types |
| `pkg/cp/untar.go` | Path validation helpers |
