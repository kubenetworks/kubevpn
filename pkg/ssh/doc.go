// Package ssh provides SSH client functionality for kubevpn.
//
// Features:
//   - Multi-hop jump host chains (ProxyJump alias recursion with cycle detection)
//   - GSSAPI/Kerberos authentication (keytab, password, ccache)
//   - Port forwarding and reverse tunnels
//   - Remote command execution
//   - Kubeconfig tunneling through SSH to reach private clusters
package ssh
