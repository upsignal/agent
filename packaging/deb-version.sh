#!/bin/sh
# Converts a `git describe` version into a Debian package version, which must start with a digit and sort
# upgrades correctly:
#   v1.2.0            -> 1.2.0
#   v1.2.0-3-gabc123  -> 1.2.0+3.gabc123   (commits after a tag sort after it)
#   v1.3.0-rc1        -> 1.3.0~rc1         (pre-releases sort before the release)
#   abc123-dirty      -> 0.0.0+gitabc123.dirty
set -eu
v=$1
v=${v#v}
case $v in
[0-9]*.*) ;;
# No tag yet: `git describe --always` printed a commit hash, which can also start with a digit.
*) v=0.0.0+git$1 ;;
esac
printf '%s\n' "$v" | sed -E 's/-dirty$/.dirty/; s/-([0-9]+)-g([0-9a-f]+)/+\1.g\2/; s/-([A-Za-z])/~\1/; s/-/./g'
