#!/usr/bin/env bash
# promptster-teams installer
# Usage: curl -fsSL https://raw.githubusercontent.com/pa-arth/promptster-teams-cli/main/install.sh | sh
# Or:    PROMPTSTER_TEAMS_VERSION=0.1.0 curl -fsSL <url> | sh
#
# Before installing, this verifies the minisign SIGNATURE over SHA256SUMS using
# the release public key embedded below — the same trust root the CLI's own
# auto-updater enforces — then checks the downloaded binary against those signed
# checksums. It uses `minisign` if present, else OpenSSL 3.x. If neither is
# available it refuses to install; set PROMPTSTER_TEAMS_SKIP_SIGNATURE=1 to fall
# back to checksum-only verification (not recommended).
set -euo pipefail

REPO="pa-arth/promptster-teams-cli"
VERSION="${PROMPTSTER_TEAMS_VERSION:-latest}"
BINARY="promptster-teams"

ok()   { printf '  \033[32m✓\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# --- Release trust root -------------------------------------------------------
# This is the minisign PUBLIC key whose secret half signs SHA256SUMS in the
# release pipeline (.github/workflows/release.yml). The SAME key is embedded in
# the CLI binary (internal/selfupdate/verify.go) and committed as minisign.pub.
# Verifying SHA256SUMS against it here gives `curl | sh` the SAME trust root as
# the auto-updater: an attacker who can rewrite the GitHub release assets (both
# the binary AND SHA256SUMS) still cannot forge this signature without the
# secret key, so the checksum we then trust is genuinely ours.
# Key id: F977AA1063A19E8F.
MINISIGN_PUBKEY="RWSPnqFjEKp3+YGItrZaM+Ks6clhwDqFJBSDO/rMU1/KTm7xuijKxmO2"

# The Ed25519 half of MINISIGN_PUBKEY as an X.509 SPKI PEM, for the OpenSSL
# fallback used when `minisign` is not installed. Derived from the key above;
# CI (ci.yml) asserts the two agree so this cannot silently drift on rotation.
SIGN_PUBKEY_PEM="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAgYi2tloz4qzpyWHAOoUkFIM7+sxTX8pObvG6KMrGY7Y=
-----END PUBLIC KEY-----"

# find_capable_openssl: echo the path to an OpenSSL that can verify the release
# signature, or return 1. Requires BLAKE2b-512 and `pkeyutl -rawin`, which is
# OpenSSL 3.0+ only — macOS ships LibreSSL as /usr/bin/openssl (neither), so we
# also probe the common Homebrew prefixes.
find_capable_openssl() {
  for _cand in openssl \
      /opt/homebrew/opt/openssl@3/bin/openssl \
      /opt/homebrew/opt/openssl/bin/openssl \
      /usr/local/opt/openssl@3/bin/openssl \
      /usr/local/opt/openssl/bin/openssl; do
    command -v "${_cand}" >/dev/null 2>&1 || continue
    printf '' | "${_cand}" dgst -blake2b512 >/dev/null 2>&1 || continue
    case "$("${_cand}" pkeyutl -help 2>&1 || true)" in
      *rawin*) echo "${_cand}"; return 0 ;;
    esac
  done
  return 1
}

# verify_with_openssl <openssl> <sums> <sig>: verify the prehashed minisign
# signature (Ed25519 over BLAKE2b-512 of the file) without the minisign binary.
# Returns 0 iff the signature is cryptographically valid under our key.
verify_with_openssl() {
  _ossl="$1"; _sums="$2"; _sig="$3"
  _td="$(mktemp -d)" || return 1
  # A .minisig's line 2 is base64(alg[2] || keyid[8] || ed25519_sig[64]).
  _line="$(sed -n '2p' "${_sig}")"
  [ -n "${_line}" ] || { rm -rf "${_td}"; return 1; }
  printf '%s' "${_line}" | "${_ossl}" base64 -d -A > "${_td}/blob" 2>/dev/null || { rm -rf "${_td}"; return 1; }
  # Must be the prehashed algorithm 'ED'; our pipeline always signs prehashed.
  if [ "$(dd if="${_td}/blob" bs=1 count=2 2>/dev/null)" != "ED" ]; then
    rm -rf "${_td}"; return 1
  fi
  # keyid (bytes 3..10) must match our embedded key's keyid — reject a valid
  # signature made by a DIFFERENT minisign key.
  dd if="${_td}/blob" bs=1 skip=2 count=8 2>/dev/null > "${_td}/sig_keyid"
  printf '%s' "${MINISIGN_PUBKEY}" | "${_ossl}" base64 -d -A 2>/dev/null | dd bs=1 skip=2 count=8 2>/dev/null > "${_td}/pub_keyid"
  cmp -s "${_td}/sig_keyid" "${_td}/pub_keyid" || { rm -rf "${_td}"; return 1; }
  # 64-byte raw signature = last 64 bytes; Ed25519 message = BLAKE2b-512(sums).
  tail -c 64 "${_td}/blob" > "${_td}/sig.raw"
  "${_ossl}" dgst -blake2b512 -binary -out "${_td}/h.bin" "${_sums}" 2>/dev/null || { rm -rf "${_td}"; return 1; }
  printf '%s\n' "${SIGN_PUBKEY_PEM}" > "${_td}/ed_pub.pem"
  if "${_ossl}" pkeyutl -verify -pubin -inkey "${_td}/ed_pub.pem" -rawin -in "${_td}/h.bin" -sigfile "${_td}/sig.raw" >/dev/null 2>&1; then
    rm -rf "${_td}"; return 0
  fi
  rm -rf "${_td}"; return 1
}

# verify_sums_signature <sums> <sig>: prove SHA256SUMS was signed by our release
# key BEFORE any checksum inside it is trusted. die()s on a real verification
# failure (tamper). If NO verifier is available it die()s too — unless the
# operator set PROMPTSTER_TEAMS_SKIP_SIGNATURE=1, which downgrades ONLY the
# missing-verifier case to a warning. A genuine mismatch is never overridable.
verify_sums_signature() {
  _sums="$1"; _sig="$2"

  # Preferred: minisign. Verifies the file signature AND the trusted-comment
  # global signature, exactly like the auto-updater's go-minisign path.
  if command -v minisign >/dev/null 2>&1; then
    if minisign -V -P "${MINISIGN_PUBKEY}" -m "${_sums}" -x "${_sig}" >/dev/null 2>&1; then
      ok "release signature verified (minisign)"
      return 0
    fi
    die "release signature did NOT verify — SHA256SUMS is not signed by the
  promptster-teams release key. Refusing to install a possibly tampered binary."
  fi

  # Fallback: OpenSSL 3.x can do it directly.
  _ossl="$(find_capable_openssl)" || _ossl=""
  if [ -n "${_ossl}" ]; then
    if verify_with_openssl "${_ossl}" "${_sums}" "${_sig}"; then
      ok "release signature verified (openssl)"
      return 0
    fi
    die "release signature did NOT verify — SHA256SUMS is not signed by the
  promptster-teams release key. Refusing to install a possibly tampered binary."
  fi

  # No verifier available.
  if [ "${PROMPTSTER_TEAMS_SKIP_SIGNATURE:-0}" = "1" ]; then
    warn "no signature verifier (minisign / OpenSSL 3.x) found and"
    warn "PROMPTSTER_TEAMS_SKIP_SIGNATURE=1 is set — proceeding with CHECKSUM-ONLY"
    warn "verification: the download is checked against SHA256SUMS, but SHA256SUMS"
    warn "itself is NOT cryptographically verified. Not recommended."
    return 0
  fi
  die "cannot verify the release signature: no signature verifier available.
  Install one and re-run:
    macOS:          brew install minisign
    Debian/Ubuntu:  sudo apt-get install -y minisign
    Fedora:         sudo dnf install -y minisign
    Arch:           sudo pacman -S minisign
  (OpenSSL 3.x also works and ships on most Linux systems.)
  To install with checksum-only verification instead, re-run with
  PROMPTSTER_TEAMS_SKIP_SIGNATURE=1 — but that trusts whoever served the download."
}
# ------------------------------------------------------------------------------

printf '\033[1m[1/4]\033[0m Detecting platform...\n'
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
RAW_ARCH="$(uname -m)"

case "${OS}" in
  linux|darwin) ;;
  *) die "unsupported OS: ${OS}" ;;
esac

case "${RAW_ARCH}" in
  x86_64) ARCH="x64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "unsupported architecture: ${RAW_ARCH}" ;;
esac

ASSET="${BINARY}-${OS}-${ARCH}"
ok "${OS}/${ARCH}"

printf '\033[1m[2/4]\033[0m Downloading CLI...\n'
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || die "curl or wget is required"

TMP="$(mktemp)"
SUMS="$(mktemp)"
SIG="$(mktemp)"
trap 'rm -f "${TMP}" "${SUMS}" "${SIG}"' EXIT

if [ "${VERSION}" = "latest" ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/v${VERSION}"
fi

# fetch <url> <dest>
fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --progress-bar "$1" -o "$2"
  else
    wget -q --show-progress "$1" -O "$2"
  fi
}

fetch "${BASE}/${ASSET}" "${TMP}"
fetch "${BASE}/SHA256SUMS" "${SUMS}" || die "could not download SHA256SUMS — refusing to install unverified binary"
fetch "${BASE}/SHA256SUMS.minisig" "${SIG}" || die "could not download SHA256SUMS.minisig — refusing to install without a verifiable signature"

# Verify the SIGNATURE over SHA256SUMS first: this is the trust root, the same
# one the auto-updater enforces. Only once SHA256SUMS is proven authentic do we
# trust the checksum it holds for the binary below.
verify_sums_signature "${SUMS}" "${SIG}"

# Verify the download against the (now trusted) published checksum BEFORE we
# ever chmod +x and run it. A mismatch means the artifact was tampered or
# corrupted.
EXPECTED="$(grep " ${ASSET}\$" "${SUMS}" | awk '{print $1}')"
[ -n "${EXPECTED}" ] || die "no checksum for ${ASSET} in SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMP}" | awk '{print $1}')"
else
  die "need sha256sum or shasum to verify the download"
fi

if [ "${EXPECTED}" != "${ACTUAL}" ]; then
  die "checksum mismatch for ${ASSET}
  expected ${EXPECTED}
  actual   ${ACTUAL}
Refusing to install a binary that does not match the published checksum."
fi
ok "checksum verified"

printf '\033[1m[3/4]\033[0m Installing...\n'
INSTALL_DIR="${HOME}/.promptster-teams/bin"
mkdir -p "${INSTALL_DIR}"
DEST="${INSTALL_DIR}/${BINARY}"
mv "${TMP}" "${DEST}"
chmod +x "${DEST}"
ok "installed to ${DEST}"

printf '\033[1m[4/4]\033[0m Configuring PATH...\n'
PATH_ENTRY='export PATH="${HOME}/.promptster-teams/bin:${PATH}"'
PATH_COMMENT='# Added by promptster-teams installer'

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*)
    ok "already in PATH"
    ;;
  *)
    ADDED=0
    for RC_FILE in "${HOME}/.zshrc" "${HOME}/.bashrc"; do
      if [ -f "${RC_FILE}" ]; then
        if grep -q '\.promptster-teams/bin' "${RC_FILE}" 2>/dev/null; then
          ADDED=1
          continue
        fi
        printf '\n%s\n%s\n' "${PATH_COMMENT}" "${PATH_ENTRY}" >> "${RC_FILE}"
        ok "added PATH to ${RC_FILE}"
        ADDED=1
      fi
    done
    if [ "${ADDED}" -eq 0 ]; then
      printf '\n%s\n%s\n' "${PATH_COMMENT}" "${PATH_ENTRY}" >> "${HOME}/.bashrc"
      ok "created ${HOME}/.bashrc with PATH entry"
    fi
    warn "restart your shell or: source ~/.zshrc  # or ~/.bashrc"
    ;;
esac

printf '\n\033[1mpromptster-teams installed!\033[0m\n'
printf 'Get started:\n'
printf '  promptster-teams login             # paste your PSE-XXXX-XXXX key — capture starts automatically\n'
printf '  promptster-teams autostart enable  # keep capturing across reboots (starts at login)\n'
printf '  promptster-teams status            # confirm it'\''s running\n\n'
