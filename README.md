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
| 🛡 **客户端执行器** | `packwiz sync` 是原生安装器（不需要 Java），**逐条校验目标路径**，只要有一个越界就整体拒绝、不写任何字节。 |

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
packwiz modrinth scan --prune   # ② 扫描 mods 并转成 Modrinth 元数据，删掉本地 jar
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

### `packwiz modrinth scan` —— 批量转 Modrinth

```sh
packwiz modrinth scan                 # 扫描锁定的文件夹（无锁时扫 mods/plugins/...）
packwiz modrinth scan mods            # 指定文件夹
packwiz modrinth scan --dry-run       # 只预览
packwiz modrinth scan --overwrite     # 已有元数据的文件也重建
packwiz modrinth scan --prune         # 写出元数据后删掉本地 jar
packwiz modrinth scan --strict        # 有文件无法转换时以非 0 退出
packwiz modrinth scan --side client   # 强制 side
packwiz modrinth scan --algorithm sha1
```

对每个文件同时算 sha1 与 sha512，批量调用 `POST /v2/version_files` 反查；
命中就在同目录写出 `<slug>.pw.toml`（Modrinth CDN 地址 + 哈希 + `[update.modrinth]`）并登记进索引。
没命中的私有模组原样保留并列出。

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

改动集中在 8 个文件，不触碰原有下载/更新逻辑；**没有 `[sync]` 表的整合包行为与原版完全一致**。

| 文件 | 说明 |
| --- | --- |
| `core/synclock.go` ★新 | `[sync]` 模型、路径规范化、越界判定 |
| `core/index.go` | `Refresh()` 与 `Write()` 双重强制锁 |
| `core/pack.go` | `Pack` 增加 `Sync` 字段 |
| `core/indexfiles.go` | 索引条目访问器 |
| `core/packlocation.go` ★新 | 本地 / HTTP 来源抽象 |
| `core/installplan.go` ★新 | 安装计划、校验、预检 |
| `cmd/lock.go` ★新 | `lock init/show/check` |
| `cmd/sync.go` ★新 | 客户端安装器 |
| `modrinth/scan.go` ★新 | `modrinth scan` |

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

---

## 协议与致谢

本项目使用 **MIT License**，见 [LICENSE](LICENSE)。

- 上游项目：[packwiz/packwiz](https://github.com/packwiz/packwiz)（MIT）
- 上游的许可证与版权声明已保留
- 本项目的修改部分由 **R1145141919810** 维护

再次感谢 packwiz 作者与贡献者打下的基础。
