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

## Changelog
${CHANGELOG}
EOF
