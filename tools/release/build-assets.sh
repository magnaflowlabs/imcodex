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
  stage_dir="${out_dir}/${release_prefix}-${goos}-${goarch}"
  archive_name="${release_prefix}-${goos}-${goarch}.tar.gz"

  rm -rf "${stage_dir}"
  mkdir -p "${stage_dir}"

  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags="-s -w" -o "${stage_dir}/imcodex" .

  cp LICENSE README.md config.example.yaml "${stage_dir}/"
  tar -C "${stage_dir}" -czf "${out_dir}/${archive_name}" .
  rm -rf "${stage_dir}"
done

(
  cd "${out_dir}"
  "${checksum_cmd[@]}" ./*.tar.gz > "${release_prefix}-checksums.txt"
  "${verify_checksum_cmd[@]}" "${release_prefix}-checksums.txt"
)
