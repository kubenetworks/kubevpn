# Security Policy

## Reporting a Vulnerability

**Please do NOT file a public GitHub issue for security vulnerabilities.**

Instead, report them privately using GitHub Security Advisories:

1. Go to the repository's Security tab: https://github.com/kubenetworks/kubevpn/security
2. Click "Report a vulnerability"
3. Fill out the form with as much detail as possible
4. Submit the report

We will respond to your report as soon as possible, and work with you to understand and address the issue.

## Supported Versions

We support the latest released version of KubeVPN for security updates.

## Security Considerations for Users

- KubeVPN modifies your local TUN device, routing table, and DNS settings
- Proxy mode injects sidecar containers that may require elevated privileges
- Always follow the cleanup order in the README to avoid leaving resources behind
- See [Security Documentation](https://kubevpn.dev/docs/security) for more details
