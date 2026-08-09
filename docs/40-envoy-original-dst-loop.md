# Envoy ORIGINAL_DST Loop on Non-Linux Kernels

## 1. Problem

In mesh mode, envoy sidecars on **colima** (and potentially other non-Linux Docker backends on macOS) return `503 Service Unavailable` with "connection refused" for traffic that should reach the real application via `origin_cluster`. The same configuration works correctly on native Linux (including minikube on Linux).

## 2. Root Cause

### 2.1 Data path

```
user → TUN → traffic-manager → ClusterIP:9080
  → kube-proxy DNAT → pod_IP:9080
  → pod eth0
  → iptables PREROUTING: DNAT to envoy :15006
  → envoy inbound capture listener
  → origin_cluster (ORIGINAL_DST)
  → envoy connects to original dest: pod_IP:9080
```

### 2.2 The loop

When envoy's `origin_cluster` connects to `pod_IP:9080`:

| Kernel | Local-to-local routing | PREROUTING re-entry | Result |
|---|---|---|---|
| Native Linux | via **loopback** (`lo`) | No (loopback bypasses PREROUTING) | App responds normally |
| Colima VM (Lima) | via **physical interface** (`eth0`) | Yes — DNAT rule matches again | Envoy → DNAT → envoy → ... → timeout → 503 |

On Linux, the kernel recognizes that `pod_IP → pod_IP` is a local connection and routes it through the loopback interface. Loopback traffic does not traverse the PREROUTING chain, so the DNAT rule is not re-applied.

On colima's VM kernel, the same connection is routed through `eth0`. Traffic on `eth0` traverses the full netfilter PREROUTING chain, hits the DNAT rule again, and redirects back to envoy — creating an infinite loop.

### 2.3 Why this was not seen on master

The master branch uses **Docker Desktop** (`docker/setup-docker-action@v4`) whose linuxkit kernel routes local-to-local traffic through loopback, matching native Linux behavior. The refactor branch switched to **colima** for bind-mount flexibility, which exposed this kernel difference.

## 3. Fix

### 3.1 iptables rule

A single iptables rule inserted **before** the DNAT rule in the VPN sidecar startup script:

```bash
iptables -t nat -I PREROUTING 1 -p tcp -s $(hostname -i) -j ACCEPT
```

- `-t nat` — operates on the NAT table
- `-I PREROUTING 1` — inserts at position 1 (before the DNAT rule)
- `-p tcp` — only TCP (UDP does not use `origin_cluster`)
- `-s $(hostname -i)` — matches traffic whose source IP is the pod's own IP
- `-j ACCEPT` — accepts the packet without NAT, skipping subsequent PREROUTING rules

### 3.2 Resulting rule order

```
PREROUTING chain (nat table):
  1. ACCEPT  -p tcp -s <pod_IP>              ← NEW: bypass DNAT for own traffic
  2. DNAT    -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} --to :15006
```

### 3.3 Why it is safe

| Traffic | Source IP | Matches rule 1? | Matches rule 2? | Outcome |
|---|---|---|---|---|
| Envoy → app (origin_cluster) | pod_IP | **Yes → ACCEPT** | — | Reaches app directly |
| User → app (external) | TUN IP | No | Yes → DNAT | Redirected to envoy |
| Other pod → app | other pod_IP | No | Yes → DNAT | Redirected to envoy |
| Container curl localhost:9080 | 127.0.0.1 | No (TCP but wrong src) | No (`! -s 127.0.0.1`) | Reaches app directly |
| Container curl pod_IP:9080 (Linux) | pod_IP via loopback | — | — | Loopback bypasses PREROUTING entirely |

The rule only affects traffic whose **source IP** matches the pod's own IP — envoy's origin_cluster connections. All external traffic (users, other pods, traffic-manager) has a different source IP and continues to be DNAT'd to envoy. Container self-access (`curl localhost:9080`) is handled by the existing `! -s 127.0.0.1` exclusion.

This is consistent with how Istio handles the same scenario — sidecars should not intercept their own communication with the application container in the same pod.

## 4. Implementation

Modified in `pkg/inject/container.go`, the VPN sidecar startup script in `AddVPNAndEnvoyContainer`.

```go
Args: []string{fmt.Sprintf(`
echo 1 > /proc/sys/net/ipv4/ip_forward
...
# Let traffic from the pod's own IP bypass DNAT so envoy's ORIGINAL_DST
# connections to the app container do not loop back through PREROUTING.
iptables -t nat -I PREROUTING 1 -p tcp -s $(hostname -i) -j ACCEPT
iptables -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :%d
ip6tables -t nat -A PREROUTING -p tcp ! -s 0:0:0:0:0:0:0:1 ! -d ${CIDR6} -j DNAT --to :%d
kubevpn server --debug -l "tun:/tcp://...:10801?route=${CIDR4}"`, ...)},
```

## 5. Related

- [17-sidecar-injection.md](17-sidecar-injection.md) — sidecar injection strategies and iptables rules
- [16-envoy-controlplane.md](16-envoy-controlplane.md) — envoy xDS control plane, origin_cluster, and inbound capture listener
- [18-gvisor-network-stack.md](18-gvisor-network-stack.md) — gVisor TCP/UDP forwarding
