#!/usr/bin/env bash
# Prints the next release tag and the previous one, as "NEXT PREVIOUS", for
# the commit at HEAD. PREVIOUS is empty for a repository's first release.
#
# The next release bumps the patch number of the highest vX.Y.Z tag. A
# "[minor]" or "[major]" anywhere in HEAD's commit message (GitHub puts the
# pull request's title there when it merges) bumps that part instead.
# Pre-release tags (v1.0.0-rc1) and malformed ones (v.0.2.0) are ignored.
#
# Prints nothing when HEAD already carries a vX.Y.Z tag, so re-running a
# release does not make a second one.
set -euo pipefail

release_tags() { git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; }

if git tag --points-at HEAD | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "HEAD is already released as $(git tag --points-at HEAD | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)" >&2
  exit 0
fi

previous=$(release_tags | sort -V | tail -1)
if [[ -z "$previous" ]]; then
  major=0 minor=0 patch=0
else
  IFS=. read -r major minor patch <<< "${previous#v}"
fi

message=$(git log -1 --format=%B HEAD)
if [[ "$message" == *"[major]"* ]]; then
  major=$((major + 1)) minor=0 patch=0
elif [[ "$message" == *"[minor]"* ]]; then
  minor=$((minor + 1)) patch=0
else
  patch=$((patch + 1))
fi

echo "v${major}.${minor}.${patch} ${previous}"
