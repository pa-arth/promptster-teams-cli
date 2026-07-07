#!/usr/bin/env bash
# promptster-teams installer
# Usage: curl -fsSL https://raw.githubusercontent.com/pa-arth/promptster-teams-cli/main/install.sh | sh
# Or:    PROMPTSTER_TEAMS_VERSION=0.1.0 curl -fsSL <url> | sh
set -euo pipefail

REPO="pa-arth/promptster-teams-cli"
VERSION="${PROMPTSTER_TEAMS_VERSION:-latest}"
BINARY="promptster-teams"

ok()   { printf '  \033[32m✓\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

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
trap 'rm -f "${TMP}" "${SUMS}"' EXIT

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

# Verify the download against the published checksum BEFORE we ever chmod +x
# and run it. A mismatch means the artifact was tampered with or corrupted.
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
printf '  export PROMPTSTER_TEAMS_API_URL=...\n'
printf '  export PROMPTSTER_TEAMS_TOKEN=...\n'
printf '  promptster-teams doctor\n\n'
