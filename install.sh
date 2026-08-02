#!/bin/sh
# nuzur-cli installer — https://nuzur.com/cli
#
# Usage (macOS & Linux, including WSL):
#
#   curl -fsSL https://nuzur.com/install.sh | sh
#   curl -fsSL https://nuzur.com/install.sh | NUZUR_VERSION=v1.6.1 sh
#   curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=$HOME/bin sh
#
# The environment goes on the SH side of the pipe, not the curl side: it is the
# shell running this script that reads it, and `NUZUR_VERSION=... curl ... | sh`
# sets it on the wrong process — a mistake that silently installs the latest
# release instead of the pin that was asked for.
#
# Native Windows is not supported here; use Scoop (see nuzur.com/cli) or run this
# inside WSL, which is an ordinary Linux and takes the Linux path below.
#
# Design notes, because both constraints get "fixed" by people who did not know:
#
#   - POSIX `#!/bin/sh` on purpose. The advertised one-liner is `| sh`, and on
#     Debian/Ubuntu that is dash, on Alpine and most containers it is busybox ash.
#     A bash-ism here does not degrade, it fails on the majority of the machines
#     this exists for. No `local`, no `[[`, no arrays, no `$'...'`, no pipefail.
#   - `set -eu` without pipefail (there is no POSIX pipefail), so every network
#     step is checked explicitly rather than trusted to a pipeline's status.
#   - curl only. Adding a wget branch doubles the flag surface of every download
#     and every failure message for machines that, in practice, have curl.
#   - Every failure names the exact URL it tried. A 404 whose message does not say
#     which file cannot be checked by the person reading it.
#   - No automatic sudo. A script piped from the internet does not get to decide
#     to become root; it picks a destination it can already write, and says
#     exactly what to re-run if you want a system-wide one.

set -eu

# ── helpers ─────────────────────────────────────────────────────────────────────
nuzur_say() {
	printf '[nuzur-install] %s\n' "$*"
}

nuzur_fail() {
	printf '[nuzur-install] %s\n' "$*" >&2
	exit 1
}
# ── end helpers ─────────────────────────────────────────────────────────────────

# ── 1. prerequisites ────────────────────────────────────────────────────────────
# Checked up front so a missing tool is a first-line message rather than a
# half-finished install. sha256 is resolved later (§5) because it has two
# acceptable spellings.
command -v curl >/dev/null 2>&1 || nuzur_fail "curl is required and was not found. Install curl, or download the release archive by hand from https://nuzur.com/cli"
command -v tar >/dev/null 2>&1 || nuzur_fail "tar is required and was not found. Install tar, or download the release archive by hand from https://nuzur.com/cli"

# ── 2. operating system ─────────────────────────────────────────────────────────
# `uname -s` already prints exactly what goreleaser's `{{ title .Os }}` renders
# for the two supported platforms — Linux and Darwin — so the value passes
# straight into the asset name with no mapping table to drift.
NUZUR_OS="$(uname -s)"
case "$NUZUR_OS" in
Linux | Darwin) ;;
*)
	# MINGW64_NT-*/MSYS_NT-*/CYGWIN_NT-* land here: Git Bash is a POSIX shell on a
	# machine whose releases are .zip, and there is no nuzur-cli_Windows tarball to
	# install. This refusal is deliberate and tested — see
	# TestInstallScriptRejectsUnsupportedOS.
	nuzur_fail "unsupported operating system: $NUZUR_OS
On Windows, install with Scoop:
  scoop bucket add nuzur https://github.com/nuzur/scoop-bucket
  scoop install nuzur-cli
or run this installer inside WSL. All install options: https://nuzur.com/cli"
	;;
esac

# ── 3. architecture ─────────────────────────────────────────────────────────────
# The same three-way map the deploy bootstrap uses, for the same reason: `uname
# -m` and goreleaser disagree on the spelling of every one of them.
NUZUR_ARCH="$(uname -m)"
case "$NUZUR_ARCH" in
x86_64 | amd64) NUZUR_ARCH=x86_64 ;;
aarch64 | arm64) NUZUR_ARCH=arm64 ;;
i386 | i686) NUZUR_ARCH=i386 ;;
*) nuzur_fail "unsupported architecture: $NUZUR_ARCH (supported: x86_64, arm64, i386). All install options: https://nuzur.com/cli" ;;
esac

# There is no 32-bit macOS build — Apple dropped 32-bit execution entirely — so
# this combination has no asset and would otherwise 404 three steps later with a
# URL nobody can fix.
if [ "$NUZUR_OS" = "Darwin" ] && [ "$NUZUR_ARCH" = "i386" ]; then
	nuzur_fail "there is no 32-bit macOS build of nuzur-cli (Darwin/i386). All install options: https://nuzur.com/cli"
fi

# ── 4. version ──────────────────────────────────────────────────────────────────
# NUZUR_VERSION is stored BARE (no leading v) because the release carries the
# version in two forms: the tag segment has the v, goreleaser's checksums filename
# does not. Normalising once here is what lets both URLs below be written the way
# deploy.CLIReleaseAssetURL / deploy.CLIReleaseChecksumsURL compose them.
NUZUR_VERSION="${NUZUR_VERSION:-}"
NUZUR_API_URL="https://api.github.com/repos/nuzur/nuzur-cli/releases/latest"
if [ -n "$NUZUR_VERSION" ]; then
	NUZUR_VERSION="${NUZUR_VERSION#v}"
	nuzur_say "installing pinned version v${NUZUR_VERSION}"
else
	# The API, not `releases/latest/download/...`. That redirect resolves to
	# whatever Release exists at that instant, and a GitHub Release exists from the
	# moment it is created — seconds before goreleaser finishes uploading its
	# assets. Asking the API for tag_name and then fetching that tag's assets keeps
	# the resolve and the download talking about the same release.
	#
	# sed rather than jq: jq is not installed on a default machine, and this is one
	# field of a document GitHub has been serving in the same shape for a decade.
	NUZUR_API_JSON="$(curl -fsSL --retry 3 "$NUZUR_API_URL" 2>/dev/null || true)"
	NUZUR_VERSION="$(printf '%s\n' "$NUZUR_API_JSON" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
	NUZUR_VERSION="${NUZUR_VERSION#v}"
	if [ -z "$NUZUR_VERSION" ]; then
		nuzur_fail "could not resolve the latest nuzur-cli release
tried: $NUZUR_API_URL
Install a specific version instead — for example:
  curl -fsSL https://nuzur.com/install.sh | NUZUR_VERSION=v1.6.1 sh
Released versions are listed at https://github.com/nuzur/nuzur-cli/releases"
	fi
	nuzur_say "latest release is v${NUZUR_VERSION}"
fi

# ── dest resolution ─────────────────────────────────────────────────────────────
# Where the binary goes, decided from the environment alone and printed on stdout
# so the decision is testable without installing anything.
#
# The order is the whole policy, and it exists because this script must never
# become root on its own:
#
#   1. NUZUR_INSTALL_DIR — an explicit answer, honoured even if it is not on PATH.
#      Unwritable is a hard failure naming the sudo re-run, never a silent
#      fallback to somewhere the caller did not ask for.
#   2. ~/.local/bin IF it is already on PATH — the modern per-user bin dir. Taking
#      it before /usr/local/bin means a normal user's install needs no privileges
#      and no PATH advice.
#   3. /usr/local/bin (NUZUR_SYSTEM_BIN, overridable so tests need no root) if it
#      is writable — true for Homebrew-style macOS setups and for root.
#   4. ~/.local/bin regardless, created — plus the PATH line to add. Better a
#      working binary the user must expose than a refusal.
nuzur_choose_dest() {
	nuzur_cd_system="${NUZUR_SYSTEM_BIN:-/usr/local/bin}"
	nuzur_cd_home="${HOME:-}"
	nuzur_cd_explicit="${NUZUR_INSTALL_DIR:-}"

	if [ -n "$nuzur_cd_explicit" ]; then
		if [ ! -d "$nuzur_cd_explicit" ]; then
			mkdir -p "$nuzur_cd_explicit" 2>/dev/null || nuzur_fail "cannot create NUZUR_INSTALL_DIR $nuzur_cd_explicit
To install there with elevated privileges, re-run:
  curl -fsSL https://nuzur.com/install.sh | sudo NUZUR_INSTALL_DIR=$nuzur_cd_explicit sh"
		fi
		if [ ! -w "$nuzur_cd_explicit" ]; then
			nuzur_fail "NUZUR_INSTALL_DIR $nuzur_cd_explicit is not writable by $(id -un 2>/dev/null || echo "this user")
This installer never elevates on its own. To install there anyway, re-run:
  curl -fsSL https://nuzur.com/install.sh | sudo NUZUR_INSTALL_DIR=$nuzur_cd_explicit sh
Or pick a directory you own, for example:
  curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=\$HOME/.local/bin sh"
		fi
		printf '%s\n' "$nuzur_cd_explicit"
		return 0
	fi

	if [ -n "$nuzur_cd_home" ]; then
		case ":${PATH}:" in
		*":${nuzur_cd_home}/.local/bin:"*)
			mkdir -p "$nuzur_cd_home/.local/bin" 2>/dev/null || true
			if [ -w "$nuzur_cd_home/.local/bin" ]; then
				printf '%s\n' "$nuzur_cd_home/.local/bin"
				return 0
			fi
			;;
		esac
	fi

	if [ -d "$nuzur_cd_system" ] && [ -w "$nuzur_cd_system" ]; then
		printf '%s\n' "$nuzur_cd_system"
		return 0
	fi

	if [ -n "$nuzur_cd_home" ]; then
		mkdir -p "$nuzur_cd_home/.local/bin" 2>/dev/null || nuzur_fail "cannot create $nuzur_cd_home/.local/bin, and $nuzur_cd_system is not writable.
Set NUZUR_INSTALL_DIR to a directory you can write:
  curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=/path/to/bin sh"
		printf '%s\n' "$nuzur_cd_home/.local/bin"
		return 0
	fi

	nuzur_fail "no writable install directory: HOME is unset and $nuzur_cd_system is not writable.
Set NUZUR_INSTALL_DIR to a directory you can write:
  curl -fsSL https://nuzur.com/install.sh | NUZUR_INSTALL_DIR=/path/to/bin sh"
}
# ── end dest resolution ─────────────────────────────────────────────────────────

NUZUR_DEST="$(nuzur_choose_dest)"

# ── 5. download and verify ──────────────────────────────────────────────────────
NUZUR_TMP="$(mktemp -d 2>/dev/null || mktemp -d -t nuzur-install)"
# Removed on every exit path, including the failures above this line's successors:
# a half-downloaded tarball left in /tmp is the kind of thing a later run finds
# and trusts.
trap 'rm -rf "$NUZUR_TMP"' EXIT INT TERM

NUZUR_ASSET_NAME="nuzur-cli_${NUZUR_OS}_${NUZUR_ARCH}.tar.gz"
# The next two lines are drift-locked against deploy.CLIReleaseAssetURL and
# deploy.CLIReleaseChecksumsURL by TestInstallScriptComposesTheReleaseURLs. That
# test renders the Go helpers with these very shell placeholders as arguments and
# requires the result to appear here verbatim, which is what makes "the installer,
# the deploy bootstrap and the pre-flight probe fetch the same file" a checked
# claim rather than an intention. Edit these strings only together with those
# helpers.
NUZUR_ASSET_URL="https://github.com/nuzur/nuzur-cli/releases/download/v${NUZUR_VERSION}/nuzur-cli_${NUZUR_OS}_${NUZUR_ARCH}.tar.gz"
NUZUR_SUMS_URL="https://github.com/nuzur/nuzur-cli/releases/download/v${NUZUR_VERSION}/nuzur-cli_${NUZUR_VERSION}_checksums.txt"

nuzur_say "downloading ${NUZUR_ASSET_NAME} (v${NUZUR_VERSION})"
if ! curl -fSL --retry 3 "$NUZUR_ASSET_URL" -o "$NUZUR_TMP/$NUZUR_ASSET_NAME"; then
	nuzur_fail "could not download nuzur-cli v${NUZUR_VERSION} for ${NUZUR_OS}/${NUZUR_ARCH}
tried: $NUZUR_ASSET_URL
If the release was just published its assets may still be uploading — retry in a
minute, pin a known version with NUZUR_VERSION=v1.6.1, or see https://nuzur.com/cli"
fi

if ! curl -fsSL --retry 3 "$NUZUR_SUMS_URL" -o "$NUZUR_TMP/checksums.txt"; then
	nuzur_fail "could not download the checksums for nuzur-cli v${NUZUR_VERSION}
tried: $NUZUR_SUMS_URL
The archive downloaded but cannot be verified, so nothing was installed. See https://nuzur.com/cli"
fi

# sha256sum (coreutils, Linux) or shasum -a 256 (perl, shipped with macOS). One of
# the two is present on every machine this script supports; neither is universal,
# which is why both branches exist and both are tested.
nuzur_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		nuzur_fail "neither sha256sum nor shasum is available, so the download cannot be verified — refusing to install. See https://nuzur.com/cli"
	fi
}

NUZUR_WANT_SUM="$(grep " ${NUZUR_ASSET_NAME}\$" "$NUZUR_TMP/checksums.txt" | cut -d' ' -f1)"
if [ -z "$NUZUR_WANT_SUM" ]; then
	nuzur_fail "no checksum for ${NUZUR_ASSET_NAME} — refusing to install
  archive:   $NUZUR_ASSET_URL
  checksums: $NUZUR_SUMS_URL"
fi
NUZUR_GOT_SUM="$(nuzur_sha256 "$NUZUR_TMP/$NUZUR_ASSET_NAME")"
if [ "$NUZUR_GOT_SUM" != "$NUZUR_WANT_SUM" ]; then
	nuzur_fail "checksum mismatch for ${NUZUR_ASSET_NAME} — refusing to install
  want: $NUZUR_WANT_SUM
  got:  $NUZUR_GOT_SUM
  archive:   $NUZUR_ASSET_URL
  checksums: $NUZUR_SUMS_URL
The downloaded file is not the one that was published. Retry; if it persists, report it at https://nuzur.com/cli"
fi
nuzur_say "checksum verified"

# ── 6. install ──────────────────────────────────────────────────────────────────
# What is there now, read BEFORE anything is overwritten — it is the only chance
# to say "upgraded 1.5.2 → 1.6.1" instead of the far less useful "installed".
NUZUR_OLD=""
if [ -x "$NUZUR_DEST/nuzur-cli" ]; then
	NUZUR_OLD="$("$NUZUR_DEST/nuzur-cli" --version 2>/dev/null | head -n 1 || true)"
fi

# Single member: the archive also carries LICENSE and README, which have no
# business landing in a bin directory.
tar -xzf "$NUZUR_TMP/$NUZUR_ASSET_NAME" -C "$NUZUR_TMP" nuzur-cli
install -m 0755 "$NUZUR_TMP/nuzur-cli" "$NUZUR_DEST/nuzur-cli"

# ── 7. the nuzur alias ──────────────────────────────────────────────────────────
# `nuzur-cli` is the real binary and the name every doc, message and URL uses;
# `nuzur` is the convenience alias people actually type. Homebrew ships it; Scoop
# and hand-extracted archives cannot, so this installer is the other place it
# comes from.
#
# The target is RELATIVE, so the pair survives the directory being moved or
# bind-mounted somewhere else. A pre-existing `nuzur` that is NOT a symlink is
# somebody else's file — quite possibly another tool's binary — and gets a warning
# rather than being replaced.
if [ -e "$NUZUR_DEST/nuzur" ] && [ ! -L "$NUZUR_DEST/nuzur" ]; then
	nuzur_say "warning: $NUZUR_DEST/nuzur already exists and is not a symlink — leaving it alone (use nuzur-cli)"
else
	ln -sf nuzur-cli "$NUZUR_DEST/nuzur"
fi

# ── 8. report ───────────────────────────────────────────────────────────────────
NUZUR_NEW="$("$NUZUR_DEST/nuzur-cli" --version 2>/dev/null | head -n 1 || true)"
if [ -z "$NUZUR_NEW" ]; then
	NUZUR_NEW="nuzur-cli version ${NUZUR_VERSION}"
fi

if [ -z "$NUZUR_OLD" ]; then
	nuzur_say "installed ${NUZUR_NEW}"
elif [ "$NUZUR_OLD" = "$NUZUR_NEW" ]; then
	nuzur_say "reinstalled ${NUZUR_NEW} (already up to date)"
else
	nuzur_say "upgraded ${NUZUR_OLD} → ${NUZUR_NEW}"
fi
nuzur_say "binary: $NUZUR_DEST/nuzur-cli (alias: nuzur)"

case ":${PATH}:" in
*":${NUZUR_DEST}:"*) ;;
*)
	nuzur_say "$NUZUR_DEST is not on your PATH. Add it:"
	nuzur_say "  export PATH=\"$NUZUR_DEST:\$PATH\""
	;;
esac

# Another nuzur-cli elsewhere on PATH (brew, an old manual copy) either shadows
# this install or is shadowed by it — and in the shadowed-by-it case an
# ALREADY-OPEN shell can still run the old binary from its command-location
# cache, so the user's very first `nuzur-cli --version` appears to prove the
# install failed. Scan PATH for other copies and say which situation this is.
NUZUR_OTHER=""
NUZUR_SAVED_IFS="${IFS}"
IFS=:
for nuzur_dir in $PATH; do
	[ -n "$nuzur_dir" ] || continue
	[ "$nuzur_dir" = "$NUZUR_DEST" ] && continue
	if [ -x "$nuzur_dir/nuzur-cli" ]; then
		NUZUR_OTHER="$nuzur_dir/nuzur-cli"
		break
	fi
done
IFS="${NUZUR_SAVED_IFS}"
if [ -n "$NUZUR_OTHER" ]; then
	NUZUR_RESOLVED="$(command -v nuzur-cli 2>/dev/null || true)"
	if [ "$NUZUR_RESOLVED" != "$NUZUR_DEST/nuzur-cli" ]; then
		nuzur_say "note: another nuzur-cli at $NUZUR_OTHER comes FIRST on your PATH and will shadow this install."
		nuzur_say "  remove it (brew: brew uninstall nuzur/tap/nuzur-cli) or put $NUZUR_DEST earlier in PATH."
	else
		nuzur_say "note: another nuzur-cli exists at $NUZUR_OTHER; this install at $NUZUR_DEST comes first on PATH."
		nuzur_say "  already-open shells may still run the old one from cache — open a new terminal, or run: hash -r"
	fi
elif [ -n "$NUZUR_OLD" ]; then
	nuzur_say "already-open shells may still run the old version from cache — open a new terminal, or run: hash -r"
fi

nuzur_say "next: nuzur-cli --help  ·  docs at https://nuzur.com/cli"
