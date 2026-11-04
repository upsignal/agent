#!/bin/sh
# Installs a pinned upsignal-agent release on Ubuntu 22.04+ or Debian 12+, then enrolls it.
#
#   curl -fsSLO https://github.com/upsignal/agent/releases/download/v<version>/install.sh
#   less install.sh                 # read it first; never pipe a remote script into a root shell
#   sudo sh install.sh
#
# Options:
#   --version <version>   release to install, e.g. 1.2.0 (defaults to the release this script came from)
#   --api-url <url>       set api.base_url, for self-hosted Upsignal (default https://api.upsignal.cloud)
#   --no-enroll           only install; run `sudo upsignal-agent enroll` later
#   --base-url <url>      download from this URL instead of GitHub releases (mirrors, testing)
#   --require-signature   fail unless cosign is installed to verify the release signature
#
# The .deb is always checked against the release's SHA256SUMS. If cosign is installed, SHA256SUMS itself is
# verified first: it must be signed by this repository's release workflow for exactly this version tag.
set -eu

VERSION="@VERSION@"
API_URL=
ENROLL=1
BASE_URL=
REQUIRE_SIGNATURE=0
REPO=upsignal/agent

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case $1 in
	--version) [ $# -ge 2 ] || fail "--version needs a value"; VERSION=$2; shift 2 ;;
	--api-url) [ $# -ge 2 ] || fail "--api-url needs a value"; API_URL=$2; shift 2 ;;
	--base-url) [ $# -ge 2 ] || fail "--base-url needs a value"; BASE_URL=$2; shift 2 ;;
	--no-enroll) ENROLL=0; shift ;;
	--require-signature) REQUIRE_SIGNATURE=1; shift ;;
	-h | --help) sed -n '2,18p' "$0"; exit 0 ;;
	*) fail "unknown option $1 (see --help)" ;;
	esac
done

# Releases replace the placeholder above; the pattern here is split so that substitution leaves it alone.
case $VERSION in "@"*"@") fail "this copy of install.sh is not from a release; pass --version" ;; esac
VERSION=${VERSION#v}
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || fail "invalid version $VERSION"
# Release tag v1.3.0-rc1 ships upsignal-agent_1.3.0~rc1_<arch>.deb (see packaging/deb-version.sh).
DEB_VERSION=$(printf '%s' "$VERSION" | sed 's/-/~/')
[ "$(id -u)" -eq 0 ] || fail "run as root: sudo sh install.sh"
case $API_URL in
"" | https://*) ;;
*) fail "--api-url must start with https://" ;;
esac
case $API_URL in
*[!A-Za-z0-9.:/_-]*) fail "--api-url contains unsupported characters" ;;
esac

# Supported systems only: the agent is tested on these and relies on their systemd features.
[ -r /etc/os-release ] || fail "cannot read /etc/os-release"
# Read os-release in a subshell: it defines VERSION, which would clobber ours.
# shellcheck source=/dev/null
os() { (. /etc/os-release && eval "printf '%s' \"\${$1:-}\""); }
OS_ID=$(os ID)
OS_VERSION=$(os VERSION_ID)
major=${OS_VERSION%%.*}
case $OS_ID in
ubuntu) [ "${major:-0}" -ge 22 ] || fail "Ubuntu ${OS_VERSION:-?} is not supported (need 22.04 or newer)" ;;
debian) [ "${major:-0}" -ge 12 ] || fail "Debian ${OS_VERSION:-?} is not supported (need 12 or newer)" ;;
*) fail "$(os PRETTY_NAME) is not supported (Ubuntu 22.04+ or Debian 12+)" ;;
esac
ARCH=$(dpkg --print-architecture)
case $ARCH in
amd64 | arm64) ;;
*) fail "architecture $ARCH is not supported (amd64 or arm64)" ;;
esac
[ -d /run/systemd/system ] || fail "systemd is not running on this system"

if command -v curl >/dev/null; then
	fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; }
elif command -v wget >/dev/null; then
	fetch() { wget -q --https-only -O "$2" "$1"; }
else
	fail "curl or wget is required"
fi
BASE_URL=${BASE_URL:-https://github.com/$REPO/releases/download/v$VERSION}
case $BASE_URL in
https://*) ;;
*) fail "--base-url must start with https://" ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
DEB=upsignal-agent_${DEB_VERSION}_${ARCH}.deb
echo "Downloading upsignal-agent $VERSION ($ARCH)..."
fetch "$BASE_URL/$DEB" "$TMP/$DEB" || fail "download failed: $BASE_URL/$DEB"
fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS" || fail "download failed: $BASE_URL/SHA256SUMS"

if command -v cosign >/dev/null; then
	fetch "$BASE_URL/SHA256SUMS.sigstore.json" "$TMP/SHA256SUMS.sigstore.json" ||
		fail "download failed: $BASE_URL/SHA256SUMS.sigstore.json"
	cosign verify-blob "$TMP/SHA256SUMS" --bundle "$TMP/SHA256SUMS.sigstore.json" \
		--certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/v$VERSION" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>&1 ||
		fail "SHA256SUMS is not signed by the $REPO release workflow for v$VERSION; not installing"
	echo "Release signature verified."
elif [ "$REQUIRE_SIGNATURE" -eq 1 ]; then
	fail "--require-signature needs cosign (https://docs.sigstore.dev/cosign/system_config/installation/)"
else
	echo "Note: cosign is not installed, so only the checksum is verified, not the release signature."
fi
# Exactly one line for this file, and it must match; anything else aborts before installing.
# Exact string match on the file name: versions contain characters that are special in regexes.
awk -v f="$DEB" 'NF == 2 && $2 == f && $1 ~ /^[0-9a-f]+$/ && length($1) == 64' "$TMP/SHA256SUMS" >"$TMP/expected"
[ -s "$TMP/expected" ] || fail "no checksum for $DEB in SHA256SUMS"
[ "$(wc -l <"$TMP/expected")" -eq 1 ] || fail "ambiguous checksum for $DEB"
(cd "$TMP" && sha256sum --check --quiet expected) || fail "checksum mismatch for $DEB; not installing"
echo "Checksum verified."

DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$TMP/$DEB" >/dev/null
echo "Installed $(upsignal-agent version)"

if [ -n "$API_URL" ]; then
	sed -i "s|^\(  base_url:\).*|\1 \"$API_URL\"|" /etc/upsignal-agent/config.yaml
	echo "api.base_url set to $API_URL"
fi

if [ -e /var/lib/upsignal-agent/agent-credential ]; then
	# Upgrade: the package already restarted the service with the existing credential.
	systemctl enable --now upsignal-agent.service >/dev/null 2>&1 || true
	echo "Already enrolled; upsignal-agent is running."
	exit 0
fi
if [ "$ENROLL" -eq 0 ] || ! (: </dev/tty) 2>/dev/null; then
	echo "Next: sudo upsignal-agent enroll && sudo systemctl enable --now upsignal-agent"
	exit 0
fi

echo "Paste the enrollment token from Upsignal (input is hidden)."
upsignal-agent enroll </dev/tty || fail "enrollment failed; retry with: sudo upsignal-agent enroll"
systemctl enable --now upsignal-agent.service
echo "upsignal-agent is running. Check it with: sudo upsignal-agent status"
