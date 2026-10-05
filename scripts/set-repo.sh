#!/bin/sh
# One-time setup before publishing: point the module path and all docs/installer
# URLs at your repository.
#   scripts/set-repo.sh github.com/acme/routeperf
set -eu
NEW="${1:?usage: scripts/set-repo.sh github.com/<owner>/<repo>}"
case "$NEW" in github.com/*/*) ;; *) echo "expected github.com/<owner>/<repo>" >&2; exit 1;; esac
OWNER_REPO="${NEW#github.com/}"
OLD=$(go list -m)
go mod edit -module "$NEW"
find . -name '*.go' -not -path './dist/*' | xargs perl -pi -e "s#\"\Q$OLD\E/#\"$NEW/#g"
perl -pi -e "s#OWNER/routeperf#$OWNER_REPO#g" README.md install.sh .goreleaser.yaml
go build ./... && echo "module is now $NEW; docs and installer point at $OWNER_REPO"
