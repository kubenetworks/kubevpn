#!/usr/bin/env bash

RELEASE=${RELEASE:-$2}
PREVIOUS_RELEASE=${PREVIOUS_RELEASE:-$1}

# ref https://stackoverflow.com/questions/1441010/the-shortest-possible-output-from-git-log-containing-author-and-date
CHANGELOG=$(git log --no-merges --date=short --pretty=format:'- %h %an %ad %s' "${PREVIOUS_RELEASE}".."${RELEASE}")

cat <<EOF
# KubeVPN release ${RELEASE}

KubeVPN ${RELEASE} is available now ! 🎉

## Download KubeVPN for your platform

**Mac** (x86-64/Intel)

\`\`\`
curl -Lo kubevpn.zip https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_darwin_amd64.zip && unzip -d kubevpn kubevpn.zip
\`\`\`

**Mac** (AArch64/Apple M1 silicon)

\`\`\`
curl -Lo kubevpn.zip https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_darwin_arm64.zip && unzip -d kubevpn kubevpn.zip
\`\`\`

**Linux** (x86-64)

\`\`\`
curl -Lo kubevpn.zip https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_amd64.zip && unzip -d kubevpn kubevpn.zip
\`\`\`

**Linux** (AArch64)

\`\`\`
curl -Lo kubevpn.zip https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_arm64.zip && unzip -d kubevpn kubevpn.zip
\`\`\`

**Linux** (i386)

\`\`\`
curl -Lo kubevpn.zip https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_linux_386.zip && unzip -d kubevpn kubevpn.zip
\`\`\`

**Windows** (x86-64)

\`\`\`
curl -LO https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_amd64.zip
\`\`\`

**Windows** (AArch64)

\`\`\`
curl -LO https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_arm64.zip
\`\`\`

**Windows** (i386)

\`\`\`
curl -LO https://github.com/kubenetworks/kubevpn/releases/download/${RELEASE}/kubevpn_${RELEASE}_windows_386.zip
\`\`\`

## Checksums

SHA256 checksums available for compiled binaries.
Run \`shasum -a 256 -c checksums.txt\` to verify.

## Upgrading

Run \`kubevpn upgrade\` to upgrade from a previous version.

## Changelog

${CHANGELOG}
EOF
