#!/bin/sh
# Builds dist/upsignal-agent_<version>_<arch>.deb from an already built Linux binary.
# Usage: packaging/build-deb.sh <amd64|arm64> <debian-version> <binary> <out-dir>  (see deb-version.sh)
# Needs dpkg-deb; `make deb` runs this in a Debian container when the host has none.
set -eu

ARCH=$1
VERSION=$2
BINARY=$3
OUT=$4
MAINTAINER=${DEB_MAINTAINER:-Upsignal <noreply@upsignal.cloud>}
HERE=$(cd "$(dirname "$0")" && pwd)

ROOT=$(mktemp -d)
# mktemp creates 0700; the package root must not carry that onto the target system.
chmod 0755 "$ROOT"
chmod 0755 "$ROOT"
trap 'rm -rf "$ROOT"' EXIT

install -D -m 0755 "$BINARY" "$ROOT/usr/bin/upsignal-agent"
install -D -m 0644 "$HERE/systemd/upsignal-agent.service" "$ROOT/usr/lib/systemd/system/upsignal-agent.service"
# postinst sets the owner to root:upsignal-agent once the user exists.
install -D -m 0640 "$HERE/config.yaml" "$ROOT/etc/upsignal-agent/config.yaml"
install -D -m 0644 "$HERE/../README.md" "$ROOT/usr/share/doc/upsignal-agent/README.md"

install -d "$ROOT/DEBIAN"
for f in postinst prerm postrm; do
	install -m 0755 "$HERE/deb/$f" "$ROOT/DEBIAN/$f"
done
echo /etc/upsignal-agent/config.yaml >"$ROOT/DEBIAN/conffiles"
cat >"$ROOT/DEBIAN/control" <<EOF
Package: upsignal-agent
Version: $VERSION
Architecture: $ARCH
Maintainer: $MAINTAINER
Installed-Size: $(du -sk "$ROOT/usr" | cut -f1)
Recommends: systemd
Section: admin
Priority: optional
Homepage: https://github.com/upsignal/agent
Description: Upsignal server monitoring agent
 Reports CPU, memory, disk, load and network health of this server to
 Upsignal. It opens no listening ports, runs no commands, and runs as an
 unprivileged system user.
EOF

mkdir -p "$OUT"
dpkg-deb --root-owner-group -Zxz --build "$ROOT" "$OUT/upsignal-agent_${VERSION}_${ARCH}.deb" >/dev/null
echo "$OUT/upsignal-agent_${VERSION}_${ARCH}.deb"
