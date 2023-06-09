#!/usr/bin/env bash

RELEASE=${RELEASE:-$2}
PREVIOUS_RELEASE=${PREVIOUS_RELEASE:-$1}

# ref https://stackoverflow.com/questions/1441010/the-shortest-possible-output-from-git-log-containing-author-and-date
CHANGELOG=$(git log --no-merges --date=short --pretty=format:'- %h %an %ad %s' "${PREVIOUS_RELEASE}".."${RELEASE}")

cat <<EOF
## ${RELEASE}
KubeVPN ${RELEASE} is available now ! 🎉
- fix known bugs 🛠
## Installation and Upgrading
wget -LO "https://github.com/KubeNetworks/kubevpn/releases/download/$(curl -L -s https://raw.githubusercontent.com/KubeNetworks/kubevpn/master/plugins/stable.txt)/kubevpn_$(curl -L -s https://raw.githubusercontent.com/KubeNetworks/kubevpn/master/plugins/stable.txt)_darwin_amd64.zip"
## Changelog
${CHANGELOG}
EOF
