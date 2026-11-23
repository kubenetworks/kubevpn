# Node-boundary Agent PoC Result — 2026-11-23

| Check | Status |
|-------|--------|
| Cluster | Minikube (GitHub Actions) |
| Image | nodeboundary-poc:latest (built locally) |
| Workflow run | https://github.com/kubenetworks/kubevpn/actions/runs/45128055486 (false positive, see notes) |
| hostNetwork | false |
| TCP header match | NOT_RUN (agent pod never became Ready) |
| TCP origin forwarding | NOT_RUN (agent pod never became Ready) |
| Second workload isolation | NOT_RUN (agent pod never became Ready) |
| UDP path | NOT_RUN (agent pod never became Ready) |
| Workload UID/generation unchanged | NOT_RUN (agent pod never became Ready) |
| Decision | RETRY (false positive; adding pipefail and better diagnostics) |

## Notes

- Multiple workflow runs triggered:
  - 45122695559: failed at bookinfo readiness (wc -l issue)
  - 45123710626: failed at image pull (GHCR tag not built)
  - 45125808652: failed at Docker build (binary filename mismatch)
  - 45128055486: **FALSE POSITIVE** (job green, but PoC script failed: agent pod never Ready)
- Run 45128055486 is not considered a success:
  - The agent pod was created but timed out becoming Ready
  - `tee` swallowed the script failure, making the job appear green
  - No datapath tests were actually executed
- The workflow has been updated to:
  - Add `set -o pipefail` to ensure PoC script failures propagate
  - Add better diagnostics (pod describe, pod yaml) on failure
- Decision: RETRY with updated workflow to get real diagnostics
