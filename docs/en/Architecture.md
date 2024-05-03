## Architecture
### Connect mode
create a tunnel with port-forward, add route to virtual interface, like tun0, forward traffic though tunnel to remote traffic manager.  
![connect-mode](/docs/en/images/connect-mode.drawio.svg)

### Reverse mode
base on connect mode, inject a container to controller, use iptables to block all inbound traffic and forward to local though tunnel.

```text
┌──────────┐    ┌─────────┌──────────┐    ┌──────────┐
│ ServiceA ├───►│ sidecar │ ServiceB │ ┌─►│ ServiceC │
└──────────┘    └────┌────┘──────────┘ │  └──────────┘
                     │                 │
                     │                 │                     cloud
 ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘─ ─ ─ ─ ─ ─ ─ ─ ─┘ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─
                     │                 │                     local
                 ┌───┘──────┐          │
                 │ ServiceB'├──────────┘
                 └──────────┘
```

### Mesh mode
base on reverse mode, using envoy as proxy, if headers have special key-value pair, it will route to local machine, if not, use origin service.
```text
┌──────────┐    ┌─────────┌────────────┐     ┌──────────┐
│ ServiceA ├───►│ sidecar ├─► ServiceB │─►┌─►│ ServiceC │
└──────────┘    └────┌────┘────────────┘  │  └──────────┘
                     │                    │                     cloud
─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─┘─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─
                     │                    │                     local
                header: a=1               │
                 ┌───┘──────┐             │
                 │ ServiceB'├─────────────┘
                 └──────────┘
```