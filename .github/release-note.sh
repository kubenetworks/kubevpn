#!/usr/bin/env bash

RELEASE=${RELEASE:-$2}
PREVIOUS_RELEASE=${PREVIOUS_RELEASE:-$1}

# ref https://stackoverflow.com/questions/1441010/the-shortest-possible-output-from-git-log-containing-author-and-date
CHANGELOG=$(git log --no-merges --date=short --pretty=format:'- %h %an %ad %s' "${PREVIOUS_RELEASE}".."${RELEASE}")

cat <<EOF
# KubeVPN release ${RELEASE}

KubeVPN ${RELEASE} is available now ! 🎉

## Download KubeVPN for your platform

| OS      | Architecture        | Download |
|---------|---------------------|----------|
| macOS   | Apple Silicon (M1+) | [kubevpn_${RELEASE}_darwin_arm64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_darwin_arm64.zip) |
| macOS   | Intel (x86_64)      | [kubevpn_${RELEASE}_darwin_amd64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_darwin_amd64.zip) |
| Linux   | amd64               | [kubevpn_${RELEASE}_linux_amd64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_amd64.zip) |
| Linux   | arm64               | [kubevpn_${RELEASE}_linux_arm64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_arm64.zip) |
| Linux   | 386                 | [kubevpn_${RELEASE}_linux_386.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_386.zip) |
| Windows | amd64               | [kubevpn_${RELEASE}_windows_amd64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_amd64.zip) |
| Windows | arm64               | [kubevpn_${RELEASE}_windows_arm64.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_arm64.zip) |
| Windows | 386                 | [kubevpn_${RELEASE}_windows_386.zip](https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_386.zip) |

Download the zip for your platform, unzip, and move \`kubevpn\`/\`kubevpn.exe\` to your PATH.

> Or run \`kubevpn upgrade\` to upgrade from a previous version.

## Checksums

SHA256 checksums available for compiled binaries.
Run \`shasum -a 256 -c checksums.txt\` to verify.

## Changelog

${CHANGELOG}
EOF
