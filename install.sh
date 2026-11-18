#!/bin/sh
# Install the latest published macOS or Linux binary in a new local path.
set -eu

fail() {
    printf 'stealthbox installer: %s\n' "$*" >&2
    exit 1
}

download() {
    curl --disable --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --connect-timeout 15 "$@"
}

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail 'Use the matching binary from GitHub Releases. This script supports macOS and Linux.' ;;
esac
case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    amd64|x86_64) arch=amd64 ;;
    *) fail 'Supported architectures are ARM64 and AMD64.' ;;
esac
if command -v sha256sum >/dev/null 2>&1; then
    checksum() { sha256sum; }
elif command -v shasum >/dev/null 2>&1; then
    checksum() { shasum -a 256; }
else
    fail 'Install sha256sum or shasum to verify the release.'
fi
command -v curl >/dev/null 2>&1 || fail 'Install curl to download the release.'

bin_dir="$HOME/.local/bin"
target="$bin_dir/stealthbox"
if [ -e "$target" ] || [ -L "$target" ]; then
    fail "$target already exists. Use that installation or move it before retrying."
fi

releases=https://github.com/mcoder33/stealthbox/releases
release=$(download --head --output /dev/null --write-out '%{url_effective}' "$releases/latest") ||
    fail 'Could not find the latest release on GitHub.'
case "$release" in
    "$releases"/tag/v*) tag=${release##*/} ;;
    *) fail 'GitHub did not return a release tag.' ;;
esac
case "$tag" in
    *[!a-zA-Z0-9._-]*) fail 'GitHub returned an invalid release tag.' ;;
esac
assets="$releases/download/$tag"
asset="stealthbox-$os-$arch"

mkdir -p -- "$bin_dir"
staging=$(mktemp -d "$bin_dir/.stealthbox.XXXXXXXX")
trap 'rm -rf -- "$staging"' 0
trap 'exit 1' HUP INT TERM

printf 'Downloading Stealth Box %s (%s/%s)\n' "$tag" "$os" "$arch"
download "$assets/checksums.txt" > "$staging/checksums.txt"
expected=$(awk -v asset="$asset" '
    NF == 2 && $2 == asset && length($1) == 64 && $1 !~ /[^[:xdigit:]]/ {
        digest = tolower($1)
        matches++
    }
    END {
        if (matches != 1) exit 1
        print digest
    }
' "$staging/checksums.txt") || fail "No unique SHA-256 entry for $asset in $tag."
download "$assets/$asset" > "$staging/stealthbox"
actual=$(checksum < "$staging/stealthbox")
[ "${actual%% *}" = "$expected" ] || fail "SHA-256 mismatch for $asset."

chmod 755 "$staging/stealthbox"
# A hard link publishes the verified file atomically and refuses an occupied name.
ln -- "$staging/stealthbox" "$bin_dir/" || fail "Could not install $target."
printf 'Installed Stealth Box %s at %s\n' "$tag" "$target"
cat <<'NEXT'
In this terminal: export PATH="$HOME/.local/bin:$PATH"
Next: stealthbox --version, then stealthbox setup --host YOUR_SSH_ALIAS
NEXT
