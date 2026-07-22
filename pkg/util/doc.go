// Package util is the shared utility package for KubeVPN. It holds cross-cutting
// helpers that don't belong to a single domain package: Kubernetes client/factory
// initialization (kube.go, kubeconfig.go), CIDR manipulation (cidr.go,
// cidr_detect.go), Docker/exec wrappers, file/port/name helpers, and version
// comparison.
//
// # Why this is one package (and not split further)
//
// util looks large (~6k lines) but is deliberately a single catch-all package.
// The cleanly separable, domain-bounded concerns already live in their own
// subpackages: netutil (network primitives), exitcode, progress, regctl, krew.
// The remaining top-level files are cross-cutting and tightly interlinked
// (version.go→FormatBanner, kube.go→kubeconfig.go's ToRESTConfig, cidr_detect.go
// →cidr.go). A full top-level split was assessed and rejected: it would churn
// 340+ call sites across 66 importing files for no functional gain, against the
// project rule that "inherent complexity is OK — don't split just to reduce line
// count" (CLAUDE.md refactoring rule 6).
//
// # When to graduate a file into a subpackage
//
// Extract a top-level file into its own subpackage (e.g. util/foo/) ONLY when it
// satisfies ALL of:
//   - Zero internal dependencies on other util top-level files (callers do not
//     reference sibling util functions such as FormatBanner or ToRESTConfig).
//   - A cohesive, nameable domain boundary (so the package name self-documents).
//   - Enough size/scope that a dedicated package earns its keep — a 70-line
//     name.go does not.
//
// As of this writing only name.go and exec_retry.go are internally decoupled,
// and neither meets the size threshold. New helpers added here should target a
// subpackage when they can; otherwise they stay top-level alongside their peers.
package util
