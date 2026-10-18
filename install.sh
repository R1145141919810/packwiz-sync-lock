#!/bin/sh
# =============================================================================
#  packwiz-sync-lock installer  (github.com/R1145141919810/packwiz-sync-lock)
#
#  Installs the patched packwiz binary for this machine.
#  If packwiz is already installed, the existing binary is backed up and
#  replaced in place, so "packwiz" always points at the new build afterwards.
#
#  Usage:
#    ./install.sh                      install or replace packwiz
#    ./install.sh --prefix DIR         install into DIR/bin if none exists yet
#    ./install.sh --bin-dir DIR        take the binaries from DIR
#    ./install.sh --base-url URL       download the binaries from URL
#    ./install.sh --installer-jar JAR  also install packwiz-installer-guarded
#    ./install.sh --no-replace         never touch an existing packwiz
#    ./install.sh --dry-run            only show what would happen
#    ./install.sh --version            print the installer version
#    ./install.sh -h | --help          this help
# =============================================================================
set -eu

INSTALLER_VERSION="1.0.0"
PROGRAM="packwiz"
PREFIX=""
BIN_DIR=""
BASE_URL=""
INSTALLER_JAR=""
REPLACE=1
DRY_RUN=0
tmp_dir=""

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	if [ -r "$0" ]; then
		sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
	else
		echo "packwiz sync-lock installer $INSTALLER_VERSION"
		echo ""
		echo "Usage: install.sh [options]"
		echo "  --prefix DIR          install into DIR/bin when no packwiz exists yet"
		echo "  --bin-dir DIR         take the binaries from DIR"
		echo "  --base-url URL        download the binaries from URL (default: GitHub releases)"
		echo "  --installer-jar FILE  also install packwiz-installer-guarded"
		echo "  --no-replace          never touch an existing packwiz"
		echo "  --dry-run             only show what would happen"
		echo "  --version             print the installer version"
	fi
}

cleanup() {
	if [ -n "$tmp_dir" ] && [ -d "$tmp_dir" ]; then
		rm -rf "$tmp_dir"
	fi
	return 0
}
trap cleanup EXIT INT TERM

while [ "$#" -gt 0 ]; do
	case "$1" in
		--prefix)          PREFIX="$2"; shift 2 ;;
		--prefix=*)        PREFIX="${1#--prefix=}"; shift ;;
		--bin-dir)         BIN_DIR="$2"; shift 2 ;;
		--bin-dir=*)       BIN_DIR="${1#--bin-dir=}"; shift ;;
		--base-url)        BASE_URL="$2"; shift 2 ;;
		--base-url=*)      BASE_URL="${1#--base-url=}"; shift ;;
		--installer-jar)   INSTALLER_JAR="$2"; shift 2 ;;
		--installer-jar=*) INSTALLER_JAR="${1#--installer-jar=}"; shift ;;
		--no-replace)      REPLACE=0; shift ;;
		--dry-run)         DRY_RUN=1; shift ;;
		--version)         say "packwiz sync-lock installer $INSTALLER_VERSION"; exit 0 ;;
		-h|--help)         usage; exit 0 ;;
		*)                 die "unknown option: $1 (try --help)" ;;
	esac
done

# ---------------------------------------------------------------- platform ---
uname_s=$(uname -s 2>/dev/null || echo unknown)
uname_m=$(uname -m 2>/dev/null || echo unknown)

case "$uname_s" in
	Linux)                  os=linux ;;
	Darwin)                 os=darwin ;;
	MINGW*|MSYS*|CYGWIN*)   os=windows ;;
	*)                      die "unsupported operating system: $uname_s" ;;
esac

case "$uname_m" in
	x86_64|amd64)   arch=amd64 ;;
	aarch64|arm64)  arch=arm64 ;;
	*)              die "unsupported architecture: $uname_m" ;;
esac

bin_name="packwiz-$os-$arch"
if [ "$os" = "windows" ]; then
	bin_name="$bin_name.exe"
fi

# ------------------------------------------------------------- source file ---
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif have openssl; then
		openssl dgst -sha256 "$1" | sed 's/.*= //'
	else
		echo ""
	fi
}

try_download() {
	url="$1"
	dest="$2"
	if have curl; then
		if curl -fsSL "$url" -o "$dest" 2>/dev/null; then return 0; fi
	fi
	if have wget; then
		if wget -qO "$dest" "$url" 2>/dev/null; then return 0; fi
	fi
	return 1
}

find_local_binary() {
	if [ -n "$BIN_DIR" ]; then
		if [ -f "$BIN_DIR/$bin_name" ]; then
			printf '%s\n' "$BIN_DIR/$bin_name"
			return 0
		fi
		return 1
	fi
	for candidate in "$script_dir/$bin_name" "$script_dir/dist/$bin_name" "$script_dir/../dist/$bin_name"; do
		if [ -f "$candidate" ]; then
			printf '%s\n' "$candidate"
			return 0
		fi
	done
	return 1
}

# Fall back to the published release assets when there is no local binary
if [ -z "$BASE_URL" ]; then
	BASE_URL="https://github.com/R1145141919810/packwiz-sync-lock/releases/latest/download"
fi

src=""
if src=$(find_local_binary); then
	say "Using bundled binary: $src"
elif [ -n "$BASE_URL" ]; then
	tmp_dir=$(mktemp -d 2>/dev/null || echo "/tmp/packwiz-install-$$")
	mkdir -p "$tmp_dir"
	src="$tmp_dir/$bin_name"
	say "Downloading $BASE_URL/$bin_name ..."
	if ! try_download "$BASE_URL/$bin_name" "$src"; then
		die "could not download $BASE_URL/$bin_name"
	fi
	if try_download "$BASE_URL/SHA256SUMS" "$tmp_dir/SHA256SUMS"; then
		expected=$(awk -v n="$bin_name" '$2 == n { print $1; exit }' "$tmp_dir/SHA256SUMS" || true)
		if [ -n "$expected" ]; then
			actual=$(sha256_of "$src")
			if [ "$actual" != "$expected" ]; then
				die "checksum mismatch for $bin_name (expected $expected, got $actual)"
			fi
			say "Checksum verified."
		fi
	fi
else
	die "no binary for $os/$arch found; pass --base-url or --bin-dir"
fi

if [ ! -f "$src" ]; then
	die "binary not found: $src"
fi

# ------------------------------------------------------- find existing tool ---
existing=""
if have packwiz; then
	existing=$(command -v packwiz 2>/dev/null || true)
fi
if [ -z "$existing" ]; then
	for candidate in /usr/local/bin/packwiz /usr/bin/packwiz "$HOME/.local/bin/packwiz" /opt/homebrew/bin/packwiz; do
		if [ -f "$candidate" ]; then
			existing="$candidate"
			break
		fi
	done
fi

# ------------------------------------------------------------ pick target ----
as_root() {
	if [ "$(id -u 2>/dev/null || echo 1)" -eq 0 ]; then
		"$@"
	elif have sudo; then
		sudo "$@"
	else
		die "need root to write there and sudo is not available: $*"
	fi
}

can_write_dir() {
	[ -d "$1" ] && [ -w "$1" ]
}

if [ -n "$existing" ] && [ "$REPLACE" -eq 1 ]; then
	target="$existing"
	say "Existing packwiz found: $target (it will be replaced)"
else
	if [ -n "$PREFIX" ]; then
		prefix="$PREFIX"
	elif [ "$(id -u 2>/dev/null || echo 1)" -eq 0 ] || can_write_dir /usr/local/bin; then
		prefix=/usr/local
	else
		prefix="$HOME/.local"
	fi
	target="$prefix/bin/packwiz"
	if [ "$os" = "windows" ]; then
		target="$target.exe"
	fi
	if [ -n "$existing" ]; then
		warn "an existing packwiz was found at $existing but --no-replace was given"
	fi
	say "Installing to: $target"
fi

target_dir=$(dirname -- "$target")
mkdir -p "$target_dir" 2>/dev/null || true

if [ "$DRY_RUN" -eq 1 ]; then
	say ""
	say "dry run:"
	say "  source : $src"
	say "  target : $target"
	if [ -n "$INSTALLER_JAR" ]; then
		say "  guard  : $target_dir/packwiz-installer-guarded"
	fi
	exit 0
fi

# ----------------------------------------------------------------- install ---
# Back up the previous build before it is overwritten
if [ -n "$existing" ] && [ -f "$existing" ] && [ "$existing" != "$src" ]; then
	stamp=$(date +%Y%m%d%H%M%S 2>/dev/null || echo backup)
	if cp -p "$existing" "$existing.bak.$stamp" 2>/dev/null; then
		say "Previous version backed up to: $existing.bak.$stamp"
	else
		warn "could not back up the previous version (continuing)"
	fi
fi

if can_write_dir "$target_dir"; then
	cp -f "$src" "$target"
	chmod 0755 "$target"
else
	say "Elevating to write $target ..."
	as_root cp -f "$src" "$target"
	as_root chmod 0755 "$target"
fi

# ------------------------------------------------------------------ verify ---
if "$target" --help 2>/dev/null | grep -q "lock"; then
	say "Verified: the new binary reports the sync-lock commands."
else
	warn "could not verify the new binary, run '$target --help' manually"
fi

# ------------------------------------------------------- optional guard ------
if [ -n "$INSTALLER_JAR" ]; then
	if [ ! -f "$INSTALLER_JAR" ]; then
		die "installer jar not found: $INSTALLER_JAR"
	fi
	jar_dir=$(CDPATH= cd -- "$(dirname -- "$INSTALLER_JAR")" && pwd)
	jar_abs="$jar_dir/$(basename -- "$INSTALLER_JAR")"
	guard="$target_dir/packwiz-installer-guarded"
	cat > "$guard" <<GUARD
#!/bin/sh
# Generated by the packwiz sync-lock installer.
# Checks a pack against its own locked folders before packwiz-installer runs, so the
# installer can never be handed a pack that syncs files outside of them.
set -eu
if [ "\$#" -lt 1 ]; then
	echo "usage: packwiz-installer-guarded <pack.toml url or file> [extra args...]" >&2
	exit 2
fi
PACK_URL="\$1"
shift
# --require-lock makes an unlocked pack a hard failure.
# Add --preflight to also download and hash every listed file first (slower, strictest).
if ! "$target" lock check --url "\$PACK_URL" --quiet --require-lock; then
	echo "Refusing to install: this pack is not inside the folders it locked." >&2
	echo "Run '$target lock check --url \$PACK_URL' for the full report." >&2
	exit 1
fi
exec java -jar "$jar_abs" "\$PACK_URL" "\$@"
GUARD
	chmod 0755 "$guard" 2>/dev/null || true
	say "Guarded launcher installed: $guard"
fi

# ----------------------------------------------------------------- summary ---
say ""
say "packwiz ($os/$arch) installed at: $target"
say ""
say "Next steps:"
say "  packwiz lock init mods          lock the pack down to the mods folder"
say "  packwiz lock show               show the active lock"
say "  packwiz lock check              verify index.toml against the lock"
say "  packwiz modrinth scan           write Modrinth metadata for local mods"
say ""
say "Clients can verify a pack before installing it with:"
say "  packwiz lock check --url https://example.com/pack.toml"
