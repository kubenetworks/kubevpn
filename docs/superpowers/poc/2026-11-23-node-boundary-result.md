# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45134423834 (timeout on port 20002) |
| hostNetwork | false |
| TCP header match | NOT_RUN (timeout waiting for port 20002) |
| TCP origin forwarding | NOT_RUN (timeout waiting for port 20002) |
| Second workload isolation | NOT_RUN (timeout waiting for port 20002) |
| UDP path | NOT_RUN (timeout waiting for port 20002) |
| Workload UID/generation unchanged | NOT_RUN (timeout waiting for port 20002) |
| Decision | NO_GO (timeout waiting for Envoy listeners on 20001/20002) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
  - 45129878854: ImagePullBackOff (imagePullPolicy=Always for :latest tried Docker Hub)
  - 45131859595: Envoy startup failure (missing typed_config for router filter)
  - 45133203004: Envoy config parse failure (duplicate typed_config for router filter)
  - 45133052768: wait_listener timeout bug
  - 45134279801: cancelled
  - 45134165759: cancelled
  - 45134423834: timeout on port 20002 (both 20001 and 20002 timed out)
- Run 45134423834 shows both ports 20001 and 20002 timed out:
  - Wait_listener function now correctly uses `exec 3<>/dev/tcp/127.0.0.1/$port` (just open/close socket)
  - This indicates Envoy itself is not starting successfully or listeners aren't binding
  - Need to check Envoy logs from preserved namespace for diagnosis
- Decision: NO_GO (timeout waiting for Envoy listeners; further investigation needed)
