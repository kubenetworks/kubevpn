# Node-Boundary Agent PoC Result — 2026-11-23

## Decision: **go**

The node-boundary datapath is viable on GitHub Actions minikube without `hostNetwork`. Stage 1 may proceed with a separate implementation plan.

## Successful evidence

- **Run:** https://github.com/kubenetworks/kubevpn/actions/runs/45146819762
- **Cluster:** GitHub Actions minikube (local `/data/.kube/config` and its ACK context were unavailable on the development machine)
- **Image:** `nodeboundary-poc:latest`, built from the checked-out commit on the runner
- **hostNetwork:** `false`
- **Workload mutation:** reviews pod UID and Deployment generation unchanged

Actual artifact output:

```text
[node-boundary-poc] PASS header match goes to authors: {"status":"Authors is healthy"}
[node-boundary-poc] PASS no header goes to original reviews: {"status": "Reviews is healthy"}
[node-boundary-poc] PASS second workload header match: {"status":"Authors is healthy"}
[node-boundary-poc] PASS second workload origin: {"status":"Details is healthy"}
[node-boundary-poc] direct UDP reply: <poc-udp-direct>
[node-boundary-poc] proxied UDP reply: <poc-udp-via-agent>
[node-boundary-poc] PASS UDP original destination path: poc-udp-via-agent
[node-boundary-poc] PASS no workload restart, no generation change, hostNetwork=false
NODE_BOUNDARY_POC_OK
```

## Architecture facts established by the PoC

1. Host-side exact-podIP DNAT to the agent Pod IP works for TCP header matching and origin forwarding.
2. **The rule must exclude `src=<agentPodIP>`** or the agent's origin-forwarding request is captured again and returns 503. This requirement is now recorded in the spec.
3. Multiple Envoys in one agent pod need unique listener/admin ports and unique `--base-id` values.
4. `SYS_ADMIN + NET_ADMIN` alone could not open `/host/proc/1/ns/net` in this environment; the successful run used `privileged: true`, matching the existing node-agent privilege level. hostNetwork remains unnecessary.
5. UDP works independently of the TCP checks; direct backend and proxied UDP replies were both verified.

## Earlier harness failures (not counted as datapath evidence)

The PoC workflow initially failed on harness issues: a readiness count using `wc -l`, an unavailable GHCR tag, a local binary filename mismatch, `tee` swallowing a failure, `imagePullPolicy=Always`, missing/duplicate Envoy `typed_config`, a `cat`-based readiness check, missing unique Envoy base IDs, host DNAT recapture, and a socat server argument passed as one string. These were corrected without changing production attach-mode code. One run (`45128055486`) was a false positive and is explicitly excluded.

The final green run used the corrected harness and its artifact is the evidence for the go decision above.
