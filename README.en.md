# packwiz-sync-lock

**English** · [简体中文](README.md)

> **This is a fork of [packwiz](https://github.com/packwiz/packwiz) (unofficial)**, built on its MIT-licensed
> source. Huge thanks to the packwiz authors and contributors — if you just want upstream behaviour, use upstream.

Three additions to packwiz: **lock a pack to specific folders**, **turn local mods into Modrinth downloads**,
and **a client installer that actually enforces the lock**.

| Feature | What it does |
| --- | --- |
| 🔒 **Sync lock** | A `[sync]` table in `pack.toml` declares which folders may be synced (default: only `mods`). Out-of-scope files never reach `index.toml`, so packwiz-installer **never downloads them**. |
| 🔍 **Modrinth scanner** | `packwiz modrinth scan` hashes every file in your mods folder, looks it up on Modrinth and writes `.pw.toml` metadata, so clients download straight from the Modrinth CDN. |
| 🛡 **Client enforcer** | `packwiz sync` is a native installer (no Java) that validates **every destination path** and refuses the whole run if a single one is out of scope. |

---

## Install

### One-liner

```sh
curl -fsSL https://raw.githubusercontent.com/R1145141919810/packwiz-sync-lock/main/install.sh | sh
```

Or download the script first (needed if you want flags like `--prefix`):

```sh
curl -fsSLO https://raw.githubusercontent.com/R1145141919810/packwiz-sync-lock/main/install.sh
chmod +x install.sh && ./install.sh
```

The installer detects your OS/arch, uses `dist/` when present, otherwise downloads the matching binary
from GitHub Releases and verifies it against `SHA256SUMS`. **If packwiz is already installed it is backed up to
`packwiz.bak.<timestamp>` and replaced in place**, then verified with `packwiz --help | grep lock`.

```sh
./install.sh --dry-run            # show what would happen
./install.sh --prefix /opt/pack   # install prefix when nothing exists yet
./install.sh --installer-jar packwiz-installer-bootstrap.jar   # also write a guarded wrapper
```

### Manual

Grab the binary for your platform from [Releases](https://github.com/R1145141919810/packwiz-sync-lock/releases),
rename it to `packwiz` and put it on your `PATH`:

```sh
chmod +x packwiz-darwin-arm64
sudo mv packwiz-darwin-arm64 /usr/local/bin/packwiz
packwiz --help | grep lock
```

### From source

```sh
git clone https://github.com/R1145141919810/packwiz-sync-lock.git
cd packwiz-sync-lock
go build -o packwiz .        # Go 1.23+
```

---

## Quick start

```sh
packwiz lock init mods          # 1. lock the pack to the mods folder
packwiz modrinth scan --prune   # 2. convert local mods into Modrinth metadata
packwiz refresh                 # 3. re-hash
packwiz lock check              # 4. verify
```

Before publishing, verify **the copy clients actually download**:

```sh
packwiz lock check --url "https://your.host/pack.toml" --preflight
```

On the client:

```sh
# A. keep using packwiz-installer, gated by a check
packwiz lock check --url "https://your.host/pack.toml" || exit 1
java -jar packwiz-installer-bootstrap.jar "https://your.host/pack.toml"

# B. use the bundled installer (strong guarantee, no Java needed)
packwiz sync --pack-url "https://your.host/pack.toml" --target ~/.minecraft
```

---

## Commands

### `packwiz lock`

| Command | Description |
| --- | --- |
| `packwiz lock init [folders...]` | Write `[sync]` and rebuild the index (default `mods`) |
| `packwiz lock init mods config kubejs` | Lock several folders at once |
| `packwiz lock init mods --mode deny` | Strict mode: exit non-zero when something is out of scope |
| `packwiz lock show` | Show the active lock |
| `packwiz lock check` | Verify the local index |
| `packwiz lock check --url "URL"` | Verify what clients download |
| `packwiz lock check --url "URL" --preflight` | Also download every file and verify its hash |
| `packwiz lock check --url "URL" --preflight-limit 200` | Cap the preflight for huge packs |
| `packwiz lock check --allow-unlocked` | Warn instead of failing when there is no lock |
| `packwiz lock check --json` | Machine-readable output |

Exit code `0` means everything checks out; `1` means out-of-scope paths, a missing lock, an index hash
mismatch or a failed preflight. The `Source:` line tells you whether the local file (`pack.toml`) or the
served copy (`http…/index.toml`) was checked.

### `packwiz sync`

```sh
packwiz sync --pack-url "https://host/pack.toml" --target ~/.minecraft
packwiz sync --pack-url "https://host/pack.toml" --target ./server --side server
packwiz sync --target ./instance --dry-run
```

| Flag | Description |
| --- | --- |
| `--pack-url URL` | Install a remote pack (omit to use the local pack) |
| `--target DIR` | Instance folder to install into (default: current directory) |
| `-s, --side client\|server\|both` | Filter by side (default `client`) |
| `--dry-run` | Show what would be installed |
| `--include-optional` | Also install optional files |
| `--allow-unlocked` | Allow packs without a `[sync]` table |
| `--ignore-lock` | ⚠️ Dangerous: install even out-of-scope files |
| `--workers N` | Parallel downloads (default 8) |

It validates the index hash first, computes every destination path, and **refuses the entire run if any
destination is out of scope — nothing is written**. Files are hash-verified; mismatches are reported and
skipped while the rest installs, exiting non-zero. It **never deletes files**.

### `packwiz modrinth scan`

```sh
packwiz modrinth scan [folders...] [--dry-run] [--overwrite] [--prune] [--strict] [--side S] [--algorithm sha1]
```

Every file is hashed (sha1 + sha512) and looked up via `POST /v2/version_files`; matches get a
`<slug>.pw.toml` next to them (Modrinth CDN URL + hash + `[update.modrinth]`) and an index entry.
Files that are not on Modrinth are left untouched and listed.

### Existing commands

`refresh`, `update`, `add`, `remove`, `pin`, `serve`, `modrinth export` and friends all still work as
upstream — but when a pack has a `[sync]` table, **every command that writes the index enforces the lock**.

---

## Lock configuration

```toml
[sync]
allowed-paths = ["mods"]   # folders that may be synced, relative to pack.toml
mode = "strip"             # strip (default) / deny (exit non-zero) / warn / off
block-escapes = true       # reject "..", absolute paths and Windows drive letters
note = "mods only"
```

---

## What gets blocked

| Case | Result |
| --- | --- |
| `config/evil.toml` and other out-of-scope files | removed from the index |
| metadata added by `modrinth add` / `url add` / `update` outside the lock | removed on write, with a warning |
| `filename = "../../pwned.jar"` | rejected as an escape |
| `filename = "/pwned.jar"` | resolves to the pack root → rejected |
| `C:/Windows/Temp/x.dll` | drive-letter component → rejected |
| index `alias` pointing outside the lock | rejected |
| `%2e%2e` encoding tricks, case tricks on Windows/macOS | handled |

---

## Differences from upstream

Eight files change; the download/update logic is untouched, and **packs without a `[sync]` table behave
exactly like upstream**.

| File | Change |
| --- | --- |
| `core/synclock.go` ★new | `[sync]` model, path normalisation, escape checks |
| `core/index.go` | lock enforced in both `Refresh()` and `Write()` |
| `core/pack.go` | `Pack` gains the `Sync` field |
| `core/indexfiles.go` | index entry accessors |
| `core/packlocation.go` ★new | local / HTTP pack locations |
| `core/installplan.go` ★new | install plan, verification, preflight |
| `cmd/lock.go` ★new | `lock init/show/check` |
| `cmd/sync.go` ★new | client installer |
| `modrinth/scan.go` ★new | `modrinth scan` |

---

## FAQ

**Do clients need a new tool?** No — packwiz-installer keeps working, because the lock acts through
`index.toml`. `packwiz sync` is an optional, stronger alternative.

**What happens to folders I exclude?** They stop being distributed, and they are not deleted. If clients
still need them (`config`, `kubejs`, …) list them in `allowed-paths`.

**Why do clients report `hash invalid!`?** The hash in the index does not match the served file — usually a
stale index, Git line-ending conversion (`core.autocrlf`), a missing upload, or `no-internal-hashes`.
`packwiz lock check --url … --preflight` pinpoints the file.

**`Your index file hash is invalid!`?** `pack.toml` and `index.toml` come from different refreshes —
`packwiz refresh` updates both, publish them together.

---

## License

**MIT**, see [LICENSE](LICENSE).

- Upstream: [packwiz/packwiz](https://github.com/packwiz/packwiz) (MIT)
- Upstream copyright notices are preserved
- Modifications maintained by **R1145141919810**
