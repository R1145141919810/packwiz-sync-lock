# packwiz-sync-lock

[English](README.en.md) · **简体中文**

> **本项目是 [packwiz](https://github.com/packwiz/packwiz) 的二次开发（非官方项目）**，基于其 MIT 协议源码构建。
> 感谢 packwiz 作者与所有贡献者；需要原版功能请使用上游项目。

给 packwiz 加了三样东西：**把整合包锁死在某几个文件夹**、**把本地模组批量转成 Modrinth 下载**、
以及**一个真正会执行锁的客户端安装器**。

| 能力 | 说明 |
| --- | --- |
| 🔒 **同步锁** | 在 `pack.toml` 里用 `[sync]` 声明允许同步的文件夹（默认只有 `mods`）。越界文件不会进入 `index.toml`，因此 packwiz-installer **不会下载它们**。 |
| 🔍 **Modrinth 扫描** | `packwiz modrinth scan` 扫描 mods 文件夹，按哈希反查 Modrinth，自动生成 `.pw.toml` 元数据，客户端直接从 Modrinth CDN 下载。 |
| 🧩 **CurseForge 补扫** | `packwiz mod scan` 在 Modrinth 之后，自动把没认出来的文件拿去 CurseForge 按指纹反查；命中的写成 `metadata:curseforge` 元数据，客户端自动从 CurseForge 下载。 |
| 🛡 **客户端执行器** | `packwiz sync` 是原生安装器（不需要 Java），**逐条校验目标路径**，只要有一个越界就整体拒绝、不写任何字节。 |
| 🌐 **超时回退** | Modrinth / CurseForge 下载超时、被墙或校验失败时，客户端自动改为从整合包服务器上的同一路径下载。 |
| ⬆️ **模组更新** | `packwiz mod update` 把模组升级到 Modrinth/CurseForge 上的新版本，**并直接替换服务端的 jar**，元数据与索引同步改写。 |

---

## 目录

- [为什么需要它](#为什么需要它)
- [安装](#安装)
- [快速开始](#快速开始)
- [命令使用方法](#命令使用方法)
- [锁的配置](#锁的配置)
- [会拦截什么](#会拦截什么)
- [与上游的差异](#与上游的差异)
- [常见问题](#常见问题)
- [协议与致谢](#协议与致谢)

---

## 为什么需要它

packwiz 的整合包默认可以往实例的**任意位置**写文件——包括 `config/`、`server.properties`、
`banned-players.json`，甚至用 `filename = "../../"` 跳出整合包目录。服务器上跑过一次
packwiz-installer 之后，服务端配置经常被整合包覆盖。

本项目的做法是：**锁 + 两道执行点**。

```text
pack.toml [sync] ──▶ index.toml（写盘时强制过滤）──▶ 客户端
                                   ├─ packwiz-installer：索引里没有 = 不会下载
                                   └─ packwiz sync：逐条校验路径，越界即整体拒绝
```

---

## 安装

### 方式一：一键安装（推荐）

```sh
curl -fsSL https://raw.githubusercontent.com/R1145141919810/packwiz-sync-lock/main/install.sh | sh
```

或者先把脚本下载下来再执行（这样能用到 `--prefix` 等参数）：

```sh
curl -fsSLO https://raw.githubusercontent.com/R1145141919810/packwiz-sync-lock/main/install.sh
chmod +x install.sh
./install.sh
```

脚本会：

1. 用 `uname` 判断系统（linux / darwin / windows）与架构（amd64 / arm64）；
2. 优先使用同目录 `dist/` 里的二进制，否则从 GitHub Releases 下载并校验 `SHA256SUMS`；
3. **检测到机器上已有 packwiz 时，先备份为 `packwiz.bak.<时间戳>`，再原地覆盖**；
4. 写好后执行 `packwiz --help | grep lock` 自检；
5. 需要 root 时自动使用 `sudo`（没有 sudo 会明确报错，不会静默失败）。

安装参数：

```sh
./install.sh                        # 安装 / 原地替换已有的 packwiz
./install.sh --dry-run              # 只看会做什么，不落盘
./install.sh --prefix /opt/pack     # 没装过时安装到指定前缀（默认 /usr/local 或 ~/.local）
./install.sh --base-url URL         # 从自定义地址下载二进制
./install.sh --bin-dir DIR          # 使用指定目录里的二进制
./install.sh --no-replace           # 不覆盖已有的 packwiz
./install.sh --installer-jar FILE   # 额外生成 packwiz-installer-guarded（带校验的包装器）
./install.sh --version | --help
```

### 方式二：手动下载

到 [Releases](https://github.com/R1145141919810/packwiz-sync-lock/releases) 下载对应平台的二进制，
改名为 `packwiz` 放进 `PATH`：

```sh
# macOS / Linux 示例（Apple Silicon）
chmod +x packwiz-darwin-arm64
sudo mv packwiz-darwin-arm64 /usr/local/bin/packwiz
packwiz --help | grep lock      # 验证
```

### 方式三：从源码构建

```sh
git clone https://github.com/R1145141919810/packwiz-sync-lock.git
cd packwiz-sync-lock
go build -o packwiz .          # 需要 Go 1.23+
```

---

## 快速开始

```sh
packwiz lock init mods          # ① 锁死：只同步 mods（可写多个：mods config kubejs）
packwiz mod scan --prune        # ② 扫 mods：先 Modrinth，剩下的自动再问 CurseForge
packwiz refresh                 # ③ 重新计算哈希
packwiz lock check              # ④ 自检
```

发布前，**对客户端实际拿到的那一份**做一次完整预检：

```sh
packwiz lock check --url "https://your.host/pack.toml" --preflight
```

客户端侧两种用法：

```sh
# A. 继续用 packwiz-installer，先过一道校验（推荐给已有玩家）
packwiz lock check --url "https://your.host/pack.toml" || exit 1
java -jar packwiz-installer-bootstrap.jar "https://your.host/pack.toml"

# B. 用自带的安装器（强保证，不需要 Java）
packwiz sync --pack-url "https://your.host/pack.toml" --target ~/.minecraft
```

---

## 命令使用方法

### `packwiz lock` —— 同步锁

| 命令 | 说明 |
| --- | --- |
| `packwiz lock init [folders...]` | 写入 `[sync]` 并按锁重建索引，默认 `mods` |
| `packwiz lock init mods config kubejs` | 一次锁多个文件夹 |
| `packwiz lock init mods --mode deny` | 严格模式：越界时命令以非 0 退出（适合 CI） |
| `packwiz lock init mods --force` | 覆盖已有的锁 |
| `packwiz lock show` | 打印当前锁（路径、模式、逃逸防护） |
| `packwiz lock check` | 自检本机索引是否越界 |
| `packwiz lock check --url "URL"` | 检查**客户端实际拿到**的那一份 |
| `packwiz lock check --url "URL" --preflight` | 再下载每个文件、逐个校验哈希 |
| `packwiz lock check --url "URL" --preflight-limit 200` | 大整合包只抽验前 200 个 |
| `packwiz lock check --allow-unlocked` | 无锁时只警告（默认判为失败） |
| `packwiz lock check --json` | 输出 JSON，便于脚本判断 |

`lock check` 的退出码：`0` 通过，`1` 有问题（越界 / 无锁 / 索引哈希不符 / 预检失败）。
输出第一行的 `Source:` 是判据：`pack.toml` 表示本机文件，`http…/index.toml` 表示远端。

### `packwiz sync` —— 客户端安装器

需要 Java 的 packwiz-installer 的替代品，自己执行锁。

```sh
packwiz sync --pack-url "https://host/pack.toml" --target ~/.minecraft
packwiz sync --pack-url "https://host/pack.toml" --target ./server --side server
packwiz sync --target ./instance --dry-run          # 先看会装什么
```

| 参数 | 说明 |
| --- | --- |
| `--pack-url URL` | 安装远端整合包（省略则使用当前目录的本地整合包） |
| `--target DIR` | 安装到哪个实例目录（默认当前目录） |
| `-s, --side client\|server\|both` | 按端过滤（默认 `client`） |
| `--dry-run` | 只显示会装什么，不落盘 |
| `--include-optional` | 同时安装整合包标记为 optional 的文件 |
| `--allow-unlocked` | 允许安装没有锁的整合包（默认拒绝） |
| `--ignore-lock` | ⚠️ 危险：即使越界也安装 |
| `--workers N` | 并发下载数（默认 8） |

行为要点：

- 先校验 `index.toml` 与 `pack.toml` 的哈希是否一致，不一致直接拒绝；
- 逐条解析元数据、算出落地路径，**只要有一个越界就整体拒绝，一个字节都不写**；
- 下载按哈希校验，不匹配的文件报错跳过、其余照常安装，最后以非 0 退出；
- **从不删除文件**（与 packwiz-installer 不同）。

### `packwiz mod scan` —— 一条命令识别两个平台（推荐）

```sh
packwiz mod scan                 # 扫描锁定的文件夹（无锁时扫 mods/plugins/...）
packwiz mod scan mods            # 指定文件夹
packwiz mod scan --dry-run       # 只预览
packwiz mod scan --prune         # 写出元数据后删掉本地 jar
packwiz mod scan --strict        # 有文件无法转换时以非 0 退出
packwiz mod scan --side client   # 强制 side
packwiz mod scan --algorithm sha1
```

两轮，按顺序：

1. **Modrinth**：每个文件算 sha1 + sha512，批量 `POST /v2/version_files` 反查；
2. **CurseForge**：**只对 Modrinth 没认出来的文件**算指纹，批量 `POST /v1/fingerprints` 反查。

命中的文件各自写出标准元数据（`[update.modrinth]` 或 `mode = "metadata:curseforge"`）并登记进索引，
客户端用原版 packwiz-installer 就能分别从两个平台下载。两个平台都不认识的文件原样保留。

```text
=== Summary ===
Scanned on disk: 3
Downloadable from Modrinth: 2
Downloadable from CurseForge: 0
Metadata files written: 2
No metadata anywhere: 1
  - mods/my-private-mod.jar
```

### `packwiz mod update` —— 更新模组并替换服务端文件

```sh
packwiz mod update sodium --dry-run                # 只看计划，不下载不写盘
packwiz mod update sodium                          # 交互确认后执行
packwiz mod update sodium lithium -y               # 多个模组，跳过确认
packwiz mod update --all --dry-run                 # 全部可更新模组
packwiz mod update --all --yes                     # 全部，直接执行
packwiz mod update --all --prefer-version-number   # 按版本号优先选版（默认关闭）
packwiz mod update --all --include-disabled        # 连 .jar.dis 的也更新
packwiz mod update sodium --metadata-only          # 只改元数据，不动文件
```

与上游 `packwiz update` 的区别：上游只重写 `.pw.toml`，**不会下载文件**；本命令会把新版本
真正下载下来、替换 `mods/` 里的 jar，并同步改写索引和 `pack.toml`，服务端重启后即生效。

#### 选版规则（默认只认正式版）

1. 只考虑作者标记为 **release** 的版本；beta / alpha / 预发布一律跳过。若该项目在你这个
   整合包下压根没有 release，才会回退并打印 `Note: ... falling back to pre-release versions`。
2. 在 release 里取**发布时间最新**的那一版——Modrinth 的版本号是自由文本
   （`forge-15.4.12`、`v2.0.0-prerelease.3`），不可靠，所以默认不看它。
3. **不会降级**：如果选中的版本号低于当前安装的版本（作者给旧分支补发构建的常见情况），
   该模组会列进 `Held back` 并跳过，而不是默默退化。
4. 想按版本号优先：`--prefer-version-number`（**默认关闭**，需要时才开）。

```text
Update plan (full releases only):
  Just Enough Items     jei-1.20.1-forge-15.49.0.200.jar -> jei-1.20.1-forge-15.56.0.205.jar

Held back: 1
  - Joban Client Mod: would downgrade 2.2.4 -> 2.0.0-prerelease.3

Skipped because the file is disabled: 1 (use --include-disabled to update them)
Up to date: 44, pinned: 0, skipped: 0
```

#### 两层验证

| 层 | 做什么 |
| --- | --- |
| 下载后哈希校验 | 按平台公布的哈希比对（sha512 → sha256 → sha1，取元数据里最强的那一种），**对不上直接拒绝**，防下载损坏与文件被替换 |
| 版本双重核对 | 在"只认 release + 发布时间"之上再用版本号做一次**降级判断**，把"发布晚但版本更低"的候选挡掉 |

**校验失败会立刻停下来**：不是悄悄回退到旧版本继续跑，而是回滚该模组、把已完成的部分写盘、
以非 0 退出——出问题第一时间就能看见，不用事后猜。

#### 锁文件 `packwiz-mod-lock.toml`

每次成功更新都会记录：来源、版本号、版本 ID、发布类型、文件名、哈希、选版依据、时间。

```toml
[mods."jei"]
source = "modrinth"
version-id = "9jqubC9n"
version = "15.56.0.205"
release-type = "release"
filename = "jei-1.20.1-forge-15.56.0.205.jar"
hash-format = "sha512"
hash = "1306fe15c81d54aa9ff8f3d9f04812c0d61ffaaae433c29621f926db8f8e2c898669ccdd99e025849859d38b7a2eac170c466390b467f4dfa77542b5dbdd1678"
selected-by = "newest full release"
updated = "2026-10-29T06:47:54Z"
```

**下次运行前会先拿磁盘上的文件跟它比一遍**：哈希对不上就说明这个 jar 被外部动过，该模组会被
跳过并提示 `changed since ...`；确实要覆盖时加 `--force`。这样"哪天被谁偷偷换了"能立刻发现。

#### 禁用文件不会被自动改名

`filename` 以 `.jar.dis` / `.disabled` 结尾的模组直接跳过并计数
（`Skipped because the file is disabled`），不会像以前那样更新后变成启用的 `.jar`。

#### 回滚与备份

替换前把 `pack.toml`、`index.toml` 和将被替换的 jar 一起放进 `.packwiz-backup/<时间戳>/`；
任何失败都会把旧文件和旧元数据原样放回。

| 措施 | 说明 |
| --- | --- |
| 先下载后落盘 | 新文件先下到临时文件，**哈希校验通过**才替换 |
| 自动备份 | `pack.toml`、`index.toml` 与旧 jar 一并保存到 `.packwiz-backup/<时间戳>/` |
| 失败回滚 | 任何一步出错 → 恢复旧文件 + 旧元数据，退出码非 0 |
| 校验失败即停 | 不降级、不静默、不复用旧文件 |
| 只写锁内路径 | 落地路径必须通过 `[sync]` 锁校验 |
| 默认确认 | 不加 `--yes` 会先列出计划再问；`--dry-run` 完全不写 |
| 只用显式目标 | 必须给名字或显式 `--all`，不会静默全量更新 |

#### 参数

| 参数 | 说明 |
| --- | --- |
| `-a, --all` | 更新所有有可用来源的模组 |
| `-n, --dry-run` | 只列计划 |
| `-y, --yes` | 跳过确认 |
| `--metadata-only` | 只改元数据，不下载 |
| `--prefer-version-number` | 按版本号优先选版（默认关闭） |
| `--include-disabled` | 连 `.dis` / `.disabled` 的模组也更新 |
| `--force` | 即使锁检查发现文件被改过也继续 |
| `--no-lock` / `--no-lock-check` | 不写 / 不比对 `packwiz-mod-lock.toml` |
| `--backup-dir DIR` | 备份目录（默认 `.packwiz-backup`） |
| `--no-backup` | 不备份，直接删除旧文件 |
| `--timeout N` | 单次下载超时秒数（默认 60） |

不会做的事：**不改 Minecraft 版本 / 加载器**（只做现有模组升级）、**不自动加依赖**（与上游一致，
缺依赖请手动 `packwiz modrinth add`）、**不删任何不在更新列表里的文件**。被 `pin` 的模组会被跳过。

> 服务端建议：第一次跑之前先停服或做一次快照；`.packwiz-backup/` 与 `.packwiz-*.tmp`
> 已加入默认忽略规则，不会被写进索引。

### `packwiz modrinth scan` —— 只转 Modrinth

只查 Modrinth，**不会**去问 CurseForge（要两个一起用 `packwiz mod scan`）：

```sh
packwiz modrinth scan                 # 扫描锁定的文件夹
packwiz modrinth scan mods            # 指定文件夹
packwiz modrinth scan --dry-run       # 只预览
packwiz modrinth scan --overwrite     # 已有元数据的文件也重建
packwiz modrinth scan --prune         # 写出元数据后删掉本地 jar
packwiz modrinth scan --strict        # 有文件无法转换时以非 0 退出
packwiz modrinth scan --side client   # 强制 side
packwiz modrinth scan --algorithm sha1
```

命中就在同目录写出 `<slug>.pw.toml`（Modrinth CDN 地址 + 哈希 + `[update.modrinth]`）并登记进索引。
没命中的私有模组原样保留并列出。

### `packwiz curseforge scan` —— 只转 CurseForge

只查 CurseForge，**不会**去问 Modrinth：

```sh
packwiz curseforge scan                 # 扫描锁定的文件夹
packwiz curseforge scan mods --dry-run  # 只预览
packwiz curseforge scan --prune         # 写出元数据后删掉本地 jar
packwiz curseforge scan --strict        # 有文件无法转换时以非 0 退出
```

命中后取回项目信息，在同目录写出 `<slug>.pw.toml`：

```toml
name = "Sodium"
filename = "sodium-fabric-0.9.3-beta.1+mc26.3.jar"
side = "both"

[download]
hash-format = "sha1"
hash = "1e6aec741b4134258ba1d7d458bae96a2e8e187e"
mode = "metadata:curseforge"

[update]
[update.curseforge]
file-id = 9195226
project-id = 394468
```

这是 packwiz / packwiz-installer 的标准格式：客户端读到 `metadata:curseforge` 后
会自己调用 CurseForge API 取下载地址，**和原版 installer 完全兼容**。

两类文件会被单独列出：

- **Not known to CurseForge**：私有模组、自己改过的 jar，原样保留；
- **需要手动下载**：作者关闭了第三方下载（`downloadUrl` 为空），installer 会提示玩家手动下载。

### 原有命令

`packwiz refresh`、`packwiz update`、`packwiz add`、`packwiz remove`、`packwiz pin`、
`packwiz serve`、`packwiz modrinth export` 等全部保留，行为与上游一致；
**但只要整合包有 `[sync]` 表，所有写索引的命令都会强制执行锁**。

---

## 锁的配置

```toml
[sync]
allowed-paths = ["mods"]   # 允许同步的文件夹（相对 pack.toml）
mode = "strip"             # strip(默认) / deny(非0退出) / warn(只警告) / off
block-escapes = true       # 拒绝 ..、绝对路径、C: 盘符
note = "仅分发 mods"
```

`packwiz lock init` 会写入这张表，并打印索引条目变化；如果一次剔除了 90% 以上的条目，
会提示你是否漏了要同步的文件夹。

### 下载回退（可选）

```toml
[sync.download]
fallback-to-server = true   # 默认 true
timeout = 60                # 单次下载超时（秒），默认 60
```

开启后，客户端在下面这些情况下会**自动改从整合包服务器下载同一个文件**
（路径 = 该文件在实例里的落地路径，例如 `mods/sodium.jar`）：

- Modrinth CDN 超时 / 连不上 / 返回 4xx、5xx；
- CurseForge API 或它的 CDN 超时 / 连不上；
- 下载到的内容哈希与整合包里记录的**不一致**。

回退下载同样会校验哈希，通过才落盘。命令行可以临时覆盖：

```sh
packwiz sync --pack-url "URL" --target ./inst --timeout 20   # 20 秒还没下完就回退
packwiz sync --pack-url "URL" --target ./inst --no-fallback  # 禁用回退，出错即失败
```

> **服务器上要留着这些文件**，回退才有东西可下。`packwiz modrinth scan --prune` 会删掉本地 jar，
> 如果你的服务器是客户端唯一的兜底来源，请不要加 `--prune`，或另外上传一份到相同路径。
>
> `packwiz serve` 也会把「元数据指向的目标文件」一并对外提供（即使它们不在 `index.toml` 里），
> 所以用 `packwiz serve` 托管整合包时回退可以直接工作。
>
> ⚠️ **回退是 `packwiz sync` 的能力**。原版 packwiz-installer 没有这个机制，它只会报错。

---

## 会拦截什么

| 情况 | 结果 |
| --- | --- |
| `config/evil.toml` 等包外文件 | 从索引剔除 |
| `modrinth add` / `url add` / `update` 新加的包外元数据 | 写盘时剔除并警告 |
| 元数据 `filename = "../../pwned.jar"` | 判定逃逸，剔除 |
| 元数据 `filename = "/pwned.jar"` | 按 installer 语义落在包根 → 越界，剔除 |
| `C:/Windows/Temp/x.dll` | 盘符组件，剔除 |
| 索引 `alias` 指向包外 | 剔除 |
| `%2e%2e` 编码绕过、大小写绕过 | 已处理 |

---

## 与上游的差异

改动集中在 16 个文件，不触碰原有下载/更新逻辑；**没有 `[sync]` 表的整合包行为与原版完全一致**。

| 文件 | 说明 |
| --- | --- |
| `core/synclock.go` ★新 | `[sync]` 模型、路径规范化、越界判定、`[sync.download]` 回退策略 |
| `core/index.go` | `Refresh()` 与 `Write()` 双重强制锁 |
| `core/pack.go` | `Pack` 增加 `Sync` 字段 |
| `core/indexfiles.go` | 索引条目访问器 |
| `core/packlocation.go` ★新 | 本地 / HTTP 来源抽象、带超时的下载 |
| `core/installplan.go` ★新 | 安装计划、CurseForge 解析、哈希校验、CDN→服务器回退、预检 |
| `core/scanutil.go` ★新 | 扫描器的公共部分：遍历、命名、写元数据 |
| `cfapi/cfapi.go` ★新 | 极简 CurseForge API 客户端（`sync` 解析 `metadata:curseforge` 用） |
| `cmd/lock.go` ★新 | `lock init/show/check` |
| `cmd/sync.go` ★新 | 客户端安装器 |
| `cmd/serve.go` | 额外提供「元数据指向的目标文件」，让回退可用 |
| `modrinth/scan.go` ★新 | `modrinth scan`，并在其后自动补一轮 CurseForge |
| `curseforge/scan.go` ★新 | `curseforge scan`（指纹反查） |
| `modscan/modscan.go` ★新 | `packwiz mod scan`：Modrinth → CurseForge 两轮合并，一次写索引 |
| `modupdate/modupdate.go` ★新 | `packwiz mod update`：下载新版本、替换文件、备份与回滚 |
| `cmd/mod.go` ★新 | `packwiz mod` 父命令 |
| `curseforge/request.go` | CurseForge 客户端加超时，避免被墙时整个命令卡死 |

---

## 常见问题

**Q: 客户端要换工具吗？**
不用。用原来的 packwiz-installer 即可，锁通过 `index.toml` 生效；`packwiz sync` 是可选的更强方案。

**Q: 加锁后被排除的文件夹会怎样？**
不再分发，也不会被删除。若客户端还需要它们（如 `config`、`kubejs`），一并写进 `allowed-paths`。

**Q: 为什么客户端报 `hash invalid!`？**
索引里的哈希与实际提供的文件不一致。常见原因：改了文件没 `packwiz refresh`、Git 换行符转换（`core.autocrlf`）、
发布时漏传、`no-internal-hashes`。用 `packwiz lock check --url ... --preflight` 可精确定位到文件。

**Q: `Your index file hash is invalid!`？**
`pack.toml` 与 `index.toml` 不同步。`packwiz refresh` 会同时更新两者，一起发布即可。

**Q: `lock check` 本地通过但客户端仍然乱装？**
检查的是本机文件，不是对外服务的那一份。用 `--url` 检查，并对比输出里的 `Source:`。

**Q: CurseForge 扫描要自己申请 API key 吗？**
不用。用的是 packwiz / packwiz-installer 内置的那把 key（与上游保持一致），开箱即可用。
如果所在网络访问不了 `api.curseforge.com`，扫描会明确报错并跳过，不会影响 Modrinth 的结果。

**Q: 客户端从 Modrinth/CurseForge 下载老是超时怎么办？**
用 `packwiz sync`（而不是原版 installer），并在 `pack.toml` 里保持 `[sync.download] fallback-to-server = true`。
CDN 失败时它会自动改从你的服务器下载同一路径的文件，并照常校验哈希。
记得服务器上要留着这些 jar。

**Q: 两个平台都认不出来的模组怎么办？**
原样保留在整合包里，继续由你自己的服务器分发——`scan` 只负责「能转的转掉」，不会动它们。

---

## 协议与致谢

本项目使用 **MIT License**，见 [LICENSE](LICENSE)。

- 上游项目：[packwiz/packwiz](https://github.com/packwiz/packwiz)（MIT）
- 上游的许可证与版权声明已保留
- 本项目的修改部分由 **R1145141919810** 维护

再次感谢 packwiz 作者与贡献者打下的基础。
