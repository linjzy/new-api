#!/usr/bin/env bash
set -Eeuo pipefail
destination="${1:?destination directory required}"
architecture="${2:-$(uname -m)}"
case "$architecture" in
  amd64|x86_64)
    target=x86_64-unknown-linux-musl
    digest=e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2
    ;;
  arm64|aarch64)
    target=aarch64-unknown-linux-musl
    digest=4c6b1c17c1c5fd0d4fb2951b7481867b95ea732b1feab269c98588b15db16253
    ;;
  *) echo "unsupported Codex CLI architecture: $architecture" >&2; exit 1 ;;
esac
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
wget -q -T 60 "https://github.com/openai/codex/releases/download/rust-v0.157.1/codex-${target}.tar.gz" -O "$scratch/codex.tar.gz"
echo "$digest  $scratch/codex.tar.gz" | sha256sum -c -
tar -xzf "$scratch/codex.tar.gz" -C "$scratch"
mkdir -p "$destination"
install -m 755 "$scratch/codex-$target" "$destination/codex"
