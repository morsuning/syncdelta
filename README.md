# SyncDelta · 离线增量同步

给"只能靠拷贝文件来同步"的场景准备的小工具:**记录上一次拷贝时的基线,下次只导出变化的文件**(新增、修改,以及被删除文件的清单),打成压缩的增量包(默认「通用压缩」,也可选体积更小的「极限压缩」),在目标端一键解开并覆盖应用。

- 单个可执行文件,**零运行时依赖**(纯 Go、`CGO_ENABLED=0`;压缩库 [ctxmix](https://github.com/morsuning/ctxmix) 和 [zpaq-go](https://github.com/morsuning/zpaq-go) 已编进程序,无需安装任何东西)
- macOS(Apple Silicon)、Windows x64、Linux x64
- 双击运行 → 自动用系统浏览器打开界面(只监听 `127.0.0.1`);关掉页面,程序自动退出
- 不需要 git,没有提交/变更历史

## 快速开始

按下面「从源码构建」自己编出 `dist/` 里的文件,各平台的使用方式如下:

| 平台 | 文件 | 备注 |
|---|---|---|
| macOS Apple Silicon | `dist/syncdelta-macos-arm64` | 首次运行:`chmod +x`;若提示"无法验证开发者",右键 → 打开,或 `xattr -dr com.apple.quarantine <文件>`(自己在本机用 `build.sh` 编出来的不会有这个提示) |
| Windows x64 | `dist/syncdelta-windows-amd64.exe` | 双击即可;SmartScreen 提示时点"更多信息 → 仍要运行"(未签名) |
| Linux x64 | `dist/syncdelta-linux-amd64` | `chmod +x` 后运行;需要有浏览器(`xdg-open`),没有的话用 `--no-browser` 并手动打开终端里打印的地址 |

> 程序会在**可执行文件所在目录**下创建 `syncdelta-data/` 文件夹,保存全部配置和基线(见下文「数据存放」)。所以请把程序放在一个有写入权限的、固定的位置;换位置时连同 `syncdelta-data/` 一起移动。

### 源端(离线环境)——每次拷贝前

1. 「同步 · 源端」页:选择**源文件夹**和**导出到**(例如 U 盘上的目录)。
2. (可选)在「排除」里勾选不想同步的文件/文件夹,或写排除规则。
3. 第一次:
   - 目标端**还没有**拷贝 → 直接点「导出增量」,得到一份全量包。
   - 目标端**已经有**一份完整拷贝 → 点「仅记录基线」(不复制任何文件),以此为起点。
4. 之后每次:先「扫描预览」看看变了什么,再「导出增量」。
   程序会在导出目录下生成 `inc_时间戳/`:

   ```
   inc_20261104_150000/
   ├── data.sdpk     ← 新增和修改过的文件内容(固实压缩成一个文件)
   ├── CHANGES.txt   ← 变更清单:A=新增 M=修改 D=删除(纯文本,可直接查看)
   ├── DELETED.txt   ← 源端已删除的文件(相对路径)
   └── META.json     ← 来源、数量、原始/压缩后大小
   ```

   「导出方式」可选:**通用压缩**(默认)、**极限压缩**、**直接复制**(生成 `files/` 文件夹,不压缩,可手工覆盖)。

### 目标端(本机)

- 「应用 · 目标端」页选择增量包(或存放多个 `inc_*` 的上级文件夹)和目标文件夹,点「应用」:程序解压 `data.sdpk`、覆盖文件(保留修改时间和可执行权限),并按 `DELETED.txt` 删除源端已删除的文件(可取消勾选)。多个增量包按时间先后依次应用;已应用过的会标注。
- `data.sdpk` **只能用本程序解开**(见下文「压缩」);所以目标端也需要这个程序。如果目标端不能运行程序,导出时选「直接复制」,把 `files/` 里的内容手工覆盖过去即可。
- 解压前会逐块校验 SHA-256,包损坏或被截断会明确报错,而不是写出错误的文件。

## 压缩

压缩来自两个纯 Go 的库,都属于 PAQ / ZPAQ 这一类"体积优先"的上下文混合(context mixing)算法:[ctxmix](https://github.com/morsuning/ctxmix)(`github.com/morsuning/ctxmix`)和 [zpaq-go](https://github.com/morsuning/zpaq-go)(`github.com/morsuning/zpaq-go`),两者在 pkg.go.dev 上都有文档。导出方式有三种:

| 导出方式 | 引擎 | 说明 |
|---|---|---|
| 通用压缩(默认) | zpaq-go(5 级) | 每个数据段都是一个**标准 ZPAQ 流**,官方 `zpaq` 程序能解开;体积通常比极限压缩大几个百分点 |
| 极限压缩 | ctxmix | 体积最小;**不是**标准 ZPAQ 格式 |
| 直接复制 | — | 不压缩,生成 `files/` 文件夹,可手工覆盖 |

配置文件里的 `compress` 取值为 `zpaq`(通用压缩)、`ctxmix`(极限压缩)、`none`(直接复制);缺省或无法识别的值按通用压缩处理。

- 所有文件按"扩展名 → 路径"排序后连成一条固实数据流,同类文件相邻,跨文件的重复内容也能被利用;JPG/PNG/ZIP/视频等已压缩格式(以及采样熵很高的文件)自动单独存放,不浪费时间。
- 实测(Go 标准库 `net` 目录源码打成 tar,4,075,520 字节):极限压缩(ctxmix)503,885,官方 `zpaq` 7.15 的 `-m5`(即通用压缩的档位)523,382,`xz -9e` 693,600,也就是比 xz 小约 27%。`cmd/compile/internal/ssa` 源码(1,505,280 字节):极限压缩 208,873,`zpaq -m5` 216,333,`xz -9e` 286,600。具体数据不同,差距会变化,个别数据(例如大量重复的 HTML)上通用压缩会更小;更多数据和方法见 [ctxmix 的说明](https://github.com/morsuning/ctxmix#measurements)。
- **代价是速度**:极限压缩约每核 0.3~0.4 MB/s,压缩和解压速度相当;数据量大时按块并行(极限压缩最多 6 个线程,每个线程约 150MB 内存;通用压缩最多 4 个线程,每个约 400MB)。首次全量导出几百 MB 会花较长时间;日常的增量一般很小。界面有进度条,可随时取消。
- 为了保证结果在不同 CPU(x86 / ARM)上逐位一致,极限压缩(ctxmix)内部**只用整数运算**,并在 amd64、arm64、32 位 x86 和大端 s390x 上对照固定的测试向量。
- **关于格式**:`data.sdpk` 是本程序自己的容器(清单、权限、修改时间、校验都在里面),只能用本程序解开。选通用压缩时,容器里的每个数据段本身是标准 ZPAQ 流,即使以后没有这个程序,也可以按块表里的偏移把它们切出来,用官方 `zpaq x` 还原成原始数据(这一点在测试里用官方 zpaq 7.15 验证过)。
- 绝对的极限(cmix / paq8px 等)速度慢几个数量级,不实用。进一步缩小体积的主要空间是"拿目标端旧版本当字典压缩修改过的文件",这需要目标端必须恰好是上一份基线,会牺牲"可跳过某个增量包"的鲁棒性,所以没有做。

- 一个任务同一时刻只运行一个扫描/导出/应用。
- 界面是浏览器页面而不是原生窗口——这是为了做到单文件、零依赖、三平台直接交叉编译(Go 的原生 GUI 库在 macOS/Linux 上需要 CGO 和系统图形库)。

## 从源码构建

需要 Go 1.22+(macOS: `brew install go`)。依赖只有 [ctxmix](https://github.com/morsuning/ctxmix) v0.1.0 和 [zpaq-go](https://github.com/morsuning/zpaq-go) v0.2.0,**首次构建需要联网**,Go 会自动下载并按 `go.sum` 校验,之后走本地缓存。`build.sh` 兼容 macOS 自带的 bash,在这台 Mac 上直接运行即可:

```bash
./build.sh                 # 构建全部:macOS arm64、Windows x64、Linux x64 → dist/
./build.sh native          # 只构建当前这台机器的版本(最快)
./build.sh mac win         # 只构建指定目标(mac / win / linux,可多选)
./build.sh --test          # 构建前先跑 go vet 和单元测试
VERSION=1.5.0 ./build.sh   # 指定版本号
```

产物在 `dist/`,同时生成 `dist/SHA256SUMS`。脚本会检查 Go 是否安装、版本是否够新,并在 PATH 里找不到时试一下常见安装位置。

开发调试:`go test ./...`;`go run . --data-dir ./syncdelta-data --no-auto-exit`。

代码结构:`engine.go`(扫描/基线/导出/应用)、`pack.go`(data.sdpk 容器,调用 ctxmix 和 zpaq-go)、`match.go`(.gitignore 语义的排除规则)、`store.go`(任务配置)、`server.go`(HTTP API)、`job.go`、`fsapi.go`、`main.go`、`web/index.html`(内嵌界面)。

## 许可证

syncdelta 以 [Mozilla Public License 2.0](LICENSE)(MPL-2.0)发布,每个源文件的开头都带有 MPL-2.0 的声明。它使用的 [zpaq-go](https://github.com/morsuning/zpaq-go) 和 [ctxmix](https://github.com/morsuning/ctxmix) 都是 MIT 许可证,声明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
