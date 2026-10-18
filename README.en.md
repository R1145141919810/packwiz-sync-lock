# packwiz-sync-lock

**English** · [简体中文](README.md)

> **This is a fork of [packwiz](https://github.com/packwiz/packwiz) (unofficial)**, built on its MIT-licensed
> source. Huge thanks to the packwiz authors and contributors — if you just want upstream behaviour, use upstream.

Five additions to packwiz: **lock a pack to specific folders**, **turn local mods into Modrinth downloads**,
**fall back to CurseForge for the rest**, **a client installer that actually enforces the lock**, and
**automatic fallback to your own server when a mod host fails**.

| Feature | What it does |
| --- | --- |
| 🔒 **Sync lock** | A `[sync]` table in `pack.toml` declares which folders may be synced (default: only `mods`). Out-of-scope files never reach `index.toml`, so packwiz-installer **never downloads them**. |
| 🔍 **Modrinth scanner** | `packwiz modrinth scan` hashes every file in your mods folder, looks it up on Modrinth and writes `.pw.toml` metadata, so clients download straight from the Modrinth CDN. |
| 🧩 **CurseForge pass** | `packwiz mod scan` fingerprints whatever Modrinth did not know against CurseForge; matches become `metadata:curseforge` metadata, so clients download them from CurseForge automatically. |
| 🛡 **Client enforcer** | `packwiz sync` is a native installer (no Java) that validates **every destination path** and refuses the whole run if a single one is out of scope. |
| 🌐 **Timeout fallback** | When a Modrinth/CurseForge download times out, is blocked or fails its hash check, the client downloads the file from your own server instead. |

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
packwiz mod scan --prune        # 2. Modrinth first, then CurseForge for the rest
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

### `packwiz mod scan` (recommended)

One command, two passes:

```sh
packwiz mod scan [folders...] [--dry-run] [--overwrite] [--prune] [--strict] [--side S] [--algorithm sha1]
```

1. **Modrinth** — every file is hashed (sha1 + sha512) and looked up via `POST /v2/version_files`.
2. **CurseForge** — only the files Modrinth did not know are fingerprinted and looked up via
   `POST /v1/fingerprints`.

Matches get a `<slug>.pw.toml` next to them with the right download mode, so stock packwiz-installer
fetches Modrinth files from the Modrinth CDN and CurseForge files through the CurseForge API.
Files neither host knows are left untouched and reported.

```text
=== Summary ===
Scanned on disk: 3
Downloadable from Modrinth: 2
Downloadable from CurseForge: 0
Metadata files written: 2
No metadata anywhere: 1
  - mods/my-private-mod.jar
```

### `packwiz modrinth scan` — Modrinth only

This one never talks to CurseForge; use `packwiz mod scan` for both.

```sh
packwiz modrinth scan [folders...] [--dry-run] [--overwrite] [--prune] [--strict] [--side S] [--algorithm sha1]
```

Every file is hashed (sha1 + sha512) and looked up via `POST /v2/version_files`; matches get a
`<slug>.pw.toml` next to them (Modrinth CDN URL + hash + `[update.modrinth]`) and an index entry.
Files that are not on Modrinth are left untouched and listed.

### `packwiz curseforge scan` — CurseForge only

This one never talks to Modrinth.

```sh
packwiz curseforge scan                  # scan the locked folders
packwiz curseforge scan mods --dry-run   # preview only
packwiz curseforge scan --prune          # delete local jars after writing metadata
```

Each file's **CurseForge fingerprint** (murmur2, whitespace stripped) is looked up via
`POST /v1/fingerprints`; matches get a `<slug>.pw.toml` with the standard CurseForge download mode:

```toml
[download]
hash-format = "sha1"
hash = "1e6aec741b4134258ba1d7d458bae96a2e8e187e"
mode = "metadata:curseforge"

[update]
[update.curseforge]
file-id = 9195226
project-id = 394468
```

That is exactly what packwiz-installer understands, so clients resolve the file through the CurseForge API
themselves. Files with third-party downloads disabled are reported (the installer asks for a manual download),
and unknown files are left untouched.

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

### Download fallback (optional)

```toml
[sync.download]
fallback-to-server = true   # default: true
timeout = 60                # seconds per download attempt, default 60
```

With this on, a client switches to **your pack server** (same path it would install the file to, e.g.
`mods/sodium.jar`) when:

- the Modrinth CDN times out, is unreachable, or returns an error;
- the CurseForge API or its CDN times out or is unreachable;
- the downloaded bytes do not match the recorded hash.

The fallback download is hash-verified too. Override per run:

```sh
packwiz sync --pack-url "URL" --target ./inst --timeout 20   # fail over after 20s
packwiz sync --pack-url "URL" --target ./inst --no-fallback  # never fall back
```

> **Keep the files on your server** for the fallback to work. `packwiz modrinth scan --prune` deletes the
> local jars, so skip `--prune` (or upload a copy to the same path) if the server is the client's only
> safety net. `packwiz serve` also serves the files that metadata points at, even when they are not in
> `index.toml`, so serving with it makes the fallback work out of the box.
>
> ⚠️ **The fallback is a `packwiz sync` feature.** Stock packwiz-installer has no such mechanism.

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

Fourteen files change; the download/update logic is untouched, and **packs without a `[sync]` table behave
exactly like upstream**.

| File | Change |
| --- | --- |
| `core/synclock.go` ★new | `[sync]` model, path normalisation, escape checks, `[sync.download]` policy |
| `core/index.go` | lock enforced in both `Refresh()` and `Write()` |
| `core/pack.go` | `Pack` gains the `Sync` field |
| `core/indexfiles.go` | index entry accessors |
| `core/packlocation.go` ★new | local / HTTP pack locations, downloads with a deadline |
| `core/installplan.go` ★new | install plan, CurseForge resolution, hash checks, CDN→server fallback, preflight |
| `core/scanutil.go` ★new | shared scanner plumbing (walking, naming, writing metadata) |
| `cfapi/cfapi.go` ★new | tiny CurseForge API client (used by `sync` for `metadata:curseforge`) |
| `cmd/lock.go` ★new | `lock init/show/check` |
| `cmd/sync.go` ★new | client installer |
| `cmd/serve.go` | also serves the files metadata points at, so the fallback works |
| `modrinth/scan.go` ★new | `modrinth scan`, chaining the CurseForge pass |
| `curseforge/scan.go` ★new | `curseforge scan` (fingerprint lookup) |
| `modscan/modscan.go` ★new | `packwiz mod scan`: Modrinth then CurseForge, one index write |
| `curseforge/request.go` | timeout for the CurseForge client so a blocked host cannot hang commands |

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

**Do I need my own CurseForge API key?** No, the key that packwiz and packwiz-installer ship with is used.
If `api.curseforge.com` is unreachable the scan says so and skips, without affecting the Modrinth results.

**Clients keep timing out on Modrinth/CurseForge.** Use `packwiz sync` and keep
`[sync.download] fallback-to-server = true`; it then downloads from your server instead, with the same hash
checks. Keep the jars on the server.

**What about mods neither host knows?** They stay in the pack and are served by you — `scan` only converts
what it can prove.

---

## License

**MIT**, see [LICENSE](LICENSE).

- Upstream: [packwiz/packwiz](https://github.com/packwiz/packwiz) (MIT)
- Upstream copyright notices are preserved
- Modifications maintained by **R1145141919810**
