#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT_DIR}"

required_go="$(sed -n 's/^go //p' go.mod)"
actual_go="$(go env GOVERSION)"
if [[ -z "${required_go}" ]]; then
  echo "failed to determine Go version from go.mod" >&2
  exit 1
fi
if [[ "${actual_go}" != "go${required_go}" ]]; then
  echo "Go version mismatch: required go${required_go}, got ${actual_go}" >&2
  exit 1
fi

if [[ "${IMCODEX_SKIP_TESTS:-0}" != "1" ]]; then
  echo "Running tests..."
  go test ./...
  go test -race ./...
fi

version="${1:-$(sed -n 's/^const appVersion = "\(.*\)"$/\1/p' version.go)}"
if [[ -z "${version}" ]]; then
  echo "failed to determine appVersion from version.go" >&2
  exit 1
fi

out_dir="${OUT_DIR:-build/release}"
release_prefix="imcodex-v${version}"
readonly version out_dir release_prefix

targets=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
)

checksum_cmd=()
verify_checksum_cmd=()
if command -v sha256sum >/dev/null 2>&1; then
  checksum_cmd=(sha256sum)
  verify_checksum_cmd=(sha256sum --check)
elif command -v shasum >/dev/null 2>&1; then
  checksum_cmd=(shasum -a 256)
  verify_checksum_cmd=(shasum -a 256 --check)
else
  echo "sha256 checksum tool not found" >&2
  exit 1
fi

rm -rf "${out_dir}"
mkdir -p "${out_dir}"

for target in "${targets[@]}"; do
  read -r goos goarch <<<"${target}"
  asset_name="${release_prefix}-${goos}-${goarch}"
  asset_path="${out_dir}/${asset_name}"

  rm -f "${asset_path}"

  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags="-s -w" -o "${asset_path}" .

  chmod 0755 "${asset_path}"
done

if find "${out_dir}" -type f \( -name '*.tar' -o -name '*.tar.*' -o -name '*.tgz' \) | grep -q .; then
  echo "release assets must be standalone binaries; tar archives are not allowed" >&2
  exit 1
fi

assets=()
for target in "${targets[@]}"; do
  read -r goos goarch <<<"${target}"
  asset_name="${release_prefix}-${goos}-${goarch}"
  if [[ ! -x "${out_dir}/${asset_name}" ]]; then
    echo "missing release binary: ${asset_name}" >&2
    exit 1
  fi
  assets+=("${asset_name}")
done

(
  cd "${out_dir}"
  "${checksum_cmd[@]}" "${assets[@]}" > "${release_prefix}-checksums.txt"
  "${verify_checksum_cmd[@]}" "${release_prefix}-checksums.txt"
)
