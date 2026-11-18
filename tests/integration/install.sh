#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' 0
mkdir "$scratch/bin"
export PATH="$scratch/bin:$PATH" HOME="$scratch/home"

cat > "$scratch/bin/uname" <<'MOCK'
#!/bin/sh
case "$1" in
    -s) printf '%s\n' "$TEST_OS" ;;
    -m) printf '%s\n' "$TEST_ARCH" ;;
    *) exit 1 ;;
esac
MOCK
cat > "$scratch/bin/curl" <<'MOCK'
#!/bin/sh
set -eu
for url do :; done
repo=https://github.com/mcoder33/stealthbox/releases
case "$url" in
    "$repo/latest") printf '%s\n' "$repo/tag/v1.2.3" ;;
    "$repo/download/v1.2.3/checksums.txt") printf '%s  %s\n' "$TEST_DIGEST" "$TEST_ASSET" ;;
    "$repo/download/v1.2.3/$TEST_ASSET") printf '%s\n' 'test binary' ;;
    *) printf 'Unexpected URL: %s\n' "$url" >&2; exit 1 ;;
esac
MOCK
chmod +x "$scratch/bin/uname" "$scratch/bin/curl"

valid_digest=1da6188a2ee4d0fc5f3c431260b0931dbd62ef8e9581e1492cf3548bab06d7ac
export TEST_OS=Darwin TEST_ARCH=arm64 TEST_ASSET=stealthbox-darwin-arm64 TEST_DIGEST="$valid_digest"
sh "$root/install.sh" > "$scratch/install.log"
target="$HOME/.local/bin/stealthbox"
test -x "$target"
test "$(cat "$target")" = 'test binary'

printf '%s\n' 'existing binary' > "$target"
if sh "$root/install.sh" > "$scratch/reinstall.log" 2>&1; then
    echo 'Installer accepted an existing target' >&2
    exit 1
fi
test "$(cat "$target")" = 'existing binary'
rm -- "$target"
ln -s "$HOME/absent" "$target"
if sh "$root/install.sh" > "$scratch/symlink.log" 2>&1; then
    echo 'Installer accepted a dangling symlink' >&2
    exit 1
fi
test -L "$target"
test ! -e "$HOME/absent"

export HOME="$scratch/linux" TEST_OS=Linux TEST_ARCH=x86_64 TEST_ASSET=stealthbox-linux-amd64
export TEST_DIGEST=0000000000000000000000000000000000000000000000000000000000000000
if sh "$root/install.sh" > "$scratch/checksum.log" 2>&1; then
    echo 'Installer accepted an invalid checksum' >&2
    exit 1
fi
test ! -e "$HOME/.local/bin/stealthbox"
grep -qi 'SHA-256' "$scratch/checksum.log"
export TEST_DIGEST="$valid_digest"
sh "$root/install.sh" > "$scratch/linux.log"
test -x "$HOME/.local/bin/stealthbox"

export HOME="$scratch/unsupported" TEST_ARCH=riscv64
if sh "$root/install.sh" > "$scratch/arch.log" 2>&1; then
    echo 'Installer accepted an unsupported architecture' >&2
    exit 1
fi
grep -q 'Supported architectures' "$scratch/arch.log"
test ! -e "$HOME"

printf '%s\n' 'Installer tests passed'
