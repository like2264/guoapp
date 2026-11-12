# guoapp 定制版 · 功能移植清单

> 用途：作者发布新版本时，从这份清单里勾选要保留/移植的功能，逐项套用即可，不必每次重做。
> 最后更新：2026-11-13 · 对应版本 **1.0.17+117** · 仓库 `like2264/guoapp`

---

## 0. 基线档案（改之前必须先确认，否则补丁全部套错）

| 项 | 值 |
|---|---|
| 内核版本 | 作者 **1.0.11+111**（旧架构） |
| 架构特征 | `native/core` 有 **161 个 `.go`**；类型 `nativeDrama`；站源写在 `provider_*.go` |
| 反例（新架构） | core 只有 **67 个 `.go`**；类型 `LibraryDrama`；多出 `provider_host.go` / `provider_registry.go` |
| 站源数量 | **17 个**（见第 5 节） |
| 版本号现状 | `pubspec.yaml` → `1.0.17+117` |
| bundle id | `com.guoapp.phoenix`（可与作者版共存需改成 `com.guoapp.all`） |
| 出包平台 | 只保留 iOS 真果鉴；android / windows / macos 全部关闭 |

**判断作者新版本是什么架构，看两个文件就够：**

```bash
# 存在 → 新架构（1.0.8+），本清单 P2/P3 需要改签名
curl -s https://raw.githubusercontent.com/<作者>/guoapp/main/native/core/provider_host.go | head -1

# 文件数量：67 ≈ 新架构，161 ≈ 旧架构
curl -s "https://api.github.com/repos/<作者>/guoapp/git/trees/main?recursive=1" \
  | python -c "import sys,json;print(len([t for t in json.load(sys.stdin)['tree'] if t['path'].startswith('native/core/') and t['path'].endswith('.go')]))"
```

---

## 1. 补丁总表

| 编号 | 功能 | 来源版本 | 改动文件 | 优先级 |
|---|---|---|---|---|
| **P1** | 全站源开关（少了它整包退化成「红果鉴」） | 1.0.11 原版 | `scripts/app_build.py` | 🔴 必须 |
| **P2** | 精准搜索（搜索出口统一过滤 + 停止无效翻页） | 自研 | `native/core/app_search_precision.go`（新）<br>`native/core/app_runtime.go`<br>`native/core/app_search_precision_test.go` | 🔴 必须 |
| **P3** | 黄果 AI 加密取流（ECDH 通道） | 自研 | `native/core/provider_huangguo_api.go` | 🔴 必须 |
| **P4** | 纯黑深色模式 | 作者 1.0.8 | `lib/ui/theme/app_theme.dart`<br>`lib/ui/app.dart`<br>`lib/local_store.dart`<br>`lib/ui/settings/appearance_settings.dart` | 🟡 可选 |
| **P5** | 构建平台精简（只出 iOS 真果鉴） | 自研 | `.github/workflows/build.yml` | 🟡 建议保留 |
| **P6** | 版本号 | — | `pubspec.yaml` | 每次必改 |

> ⚠️ 每次跑 CI 前**必须提升版本号**，否则 `prepare` 会因 `refs/tags/app-v<version>` 已存在而拦下整个运行。

---

## 2. 各补丁明细

### P1 · 全站源开关 🔴

**症状（缺了它）**：IPA 文件名是 `zhenguojian-...`，但 App 名字显示「红果鉴」，站源只剩 1 个，黄果 AI 等入口全部消失，且不报错。

**根因**：1.0.11 内核的版本开关有两个，`APP_FLAVOR` 这个参数在内核里根本没人读：

| 位置 | 读取的开关 | 缺了会怎样 |
|---|---|---|
| Dart `lib/app_build.dart` | `bool.fromEnvironment('ALL_SOURCES')` | 名字变「红果鉴」 |
| Go `native/core/app_build.go` | `-X duanjuapp/native/core.buildAllSources=true` | 站源只剩红果 |
| Xcode Run Script（`project.pbxproj`） | `DART_DEFINES` 里的 `ALL_SOURCES=true` | `CFBundleDisplayName` 写死 `HongGuoJian` |

三处全部 fallback 成 hongguojian，而 `lib/core_bridge.dart` 的一致性校验只比「两侧是否相同」，两侧同时退化 → 静默通过。

**改法**：`scripts/app_build.py` 里两行（`BuildVariant` 类内）

```python
# flutter 参数
arguments = ['--dart-define=ALL_SOURCES=' + str(self.all_sources).lower(),
             '--dart-define=APP_FLAVOR=' + self.slug]

# Go 链接参数
flags = '-s -w -X duanjuapp/native/core.buildAllSources=' + str(self.all_sources).lower()
```

**验证**：装包后「关于」页名字是 **真果鉴**，站源列表出现 **17 个**。

---

### P2 · 精准搜索 🔴

**症状（缺了它）**：搜「琼明」出来一堆明星换脸，只有 1 条是对的。

**根因**：多数站源的搜索是**向量检索**（响应里 `engine: "vector"`），24 条里可能只 1 条标题命中。早期版本为了「避免搜到 0 条」写了「整页都不相关就原样返回」的兜底，结果第 2 页起的噪声被一页页追加进列表。

**改法（3 个文件）**：

1. 新增 `native/core/app_search_precision.go` — 入口 `NativePreciseSearch(items []nativeDrama, query string) []nativeDrama`，分级：

   | 级别 | 条件 | 处理 |
   |---|---|---|
   | 5 | 标题完全相同 | 保留 |
   | 4 | 标题以关键词开头 | 保留 |
   | 3 | 标题包含关键词 | 保留 |
   | 2 | 标题含全部拆词 | 保留 |
   | 1 | 标签/分类命中 | 保留（排在后面） |
   | 0 | 仅简介命中 / 完全不相关 | **1.0.17 起：剔除** |

2. `app_runtime.go` — 搜索出口统一过滤，并在整页无命中时停止翻页：

```go
func (engine *nativeEngine) nativeCatalog(ctx context.Context, input nativeInput) (nativeCatalogResult, error) {
	result, err := engine.nativeCatalogRaw(ctx, input)
	if err != nil {
		return result, err
	}
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return result, nil
	}
	filtered := NativePreciseSearch(result.Items, query)
	if len(filtered) == 0 {
		result.HasMore = false   // 关键：不再继续翻页
	}
	result.Items = filtered
	return result, nil
}
```

3. `native/core/app_search_precision_test.go` — 断言「整页不相关 → 返回空」，防止以后又被改回「原样返回」。

**注意**：旧架构里 `nativeCatalog` 原本是唯一的列表入口。若新架构有 `nativeCatalog` / `ListCatalog` 两个出口，**两个都要挂**。

**验证**：搜「琼明」→ 只剩「琼明神女录」；搜一个绝对不存在的词 → 显示「没有找到」，而不是一屏垃圾。

---

### P3 · 黄果 AI 加密取流 🔴

**症状（缺了它）**：详情能看，点播放提示「该集暂无可播放地址」。

**根因**：站点 2026-11-09 改版，`GET /api/videos/{id}/play` 返回 `status:1` 但 `video_url` 为空；新接口整体走 **ECDH 加密信封**。

**协议（协议名 `dx150-edge-v1`，源码在 `https://huangguoai.com/static/web/js/edge-api-crypto.js`）**

| 步 | 内容 |
|---|---|
| 1 | `POST /api/playback/request` `{"v":1,"type":"hello","client_pub":"<b64url(P-256 公钥 raw,65B)>"}` |
| 2 | 返回 `{type:"hello", kid, sid, site, server_pub, salt(32B), server_time, expires_at}` |
| 3 | `shared = ECDH(P-256)`；`info = "dx150-edge-v1\nplayback\n<site>\n<kid>\n<sid>"` |
| 4 | `reqKey = HKDF-SHA256(shared, salt, info+"\nreq", 32)`；`resKey` 同理 `+"\nres"` |
| 5 | `iv = 4×0x00 + BE_uint32(seq)`；`ts = now + (server_time - now)` |
| 6 | `AAD = info + "\n" + dir + "\n" + seq + "\n" + ts`（dir 为 `req` / `res`） |
| 7 | 明文 `{"request":{"method":"POST","path":"/api/videos/{id}/episodes/{ep}/playback"}}`（**不带 body**） |
| 8 | 信封 `{"v":1,"type":"call","kid","sid","seq","ts","ciphertext":"<b64url>"}` → 返回 `type:"result"` + 密文 |
| 9 | 解密后 `{status, headers, body(base64)}`，body 内 `data.streams.main` 即 m3u8 |

**四个坑**

1. `path` 必须是 `/api/videos/{id}/episodes/{ep}/playback`；写成 `/playback?ep=2` 或 `body={"ep":2}` 一律 400「请求无法处理」
2. 握手与取流**必须同一台主机**，不能走域名轮换（`sid` 对不上）
3. m3u8、`crypt.key`、`.ts` 分片**不需要**额外鉴权头，URL 自带 `auth_key` 即可
4. Go 侧 `crypto/ecdh` + `crypto/hkdf` 需 Go 1.24+（CI 里是 1.24.1，够用）

**验证**：任意一部黄果 AI 的剧，点第 1/2/3 集都能起播；日志里 `fetchHuangguoAIPlayURL` 耗时 0.5–1.5s。

**站点再次改版时**：入口永远是 `edge-api-crypto.js`，重新对一遍协议名、`info` 拼法、`AAD` 拼法即可，业务接口路径通常不变。

---

### P4 · 纯黑深色模式 🟡

作者 1.0.8 的功能，纯 UI 改动，**4 个文件、净增约 40 行**，与内核架构无关，任何版本都能照搬。

```dart
// ① lib/ui/theme/app_theme.dart —— build() 加参数
static ThemeData build(Color seed, Brightness brightness, {
  required bool television,
  bool pureBlack = false,          // 新增
}) {
  final seeded = ColorScheme.fromSeed(seedColor: seed, brightness: brightness);
  final scheme = pureBlack && brightness == Brightness.dark
      ? seeded.copyWith(
          surface: Colors.black,
          surfaceDim: Colors.black,
          surfaceContainerLowest: Colors.black,
          surfaceContainerLow: const Color(0xFF0B0B0B),
          surfaceContainer: const Color(0xFF121212),
          surfaceContainerHigh: const Color(0xFF1A1A1A),
          surfaceContainerHighest: const Color(0xFF242424),
        )
      : seeded;
  ...
```

```dart
// ② lib/ui/app.dart —— 构建主题时传入，并纳入缓存 key
final pureBlack = store?.pureBlack ?? false;
if ((seed, television, pureBlack) != _themeKey) {
  _themeKey = (seed, television, pureBlack);
  _themes = (
    AppTheme.build(seed, Brightness.light, television: television),
    AppTheme.build(seed, Brightness.dark, television: television, pureBlack: pureBlack),
  );
}
```

```dart
// ③ lib/local_store.dart —— 三处：flag 白名单、getter、setter、导出
const _flags = [ ..., 'televisionKeyboard', 'pureBlack' ];
bool get pureBlack => _flag('pureBlack', false);
Future<void> setPureBlack(bool value) => _setting('pureBlack', value);
// 导出 JSON 里加一行
'pureBlack': pureBlack,
```

```dart
// ④ lib/ui/settings/appearance_settings.dart —— 开关
SwitchTile(
  title: '纯黑深色模式',
  subtitle: '深色时背景改为纯黑，适合 OLED 屏幕',
  value: store.pureBlack,
  onChanged: (value) => saveSetting(context, () => store.setPureBlack(value)),
),
```

**验证**：设置 → 外观 → 打开「纯黑深色模式」，深色下页面背景变纯黑（#000）。

---

### P5 · 构建平台精简 🟡

作者 workflow 一次申请 **4 台 macOS**（iOS×2 + macOS×2），而 GitHub 托管 macOS arm64 池经常 15 分钟抢不到，导致整轮白等。改 4 处：

| 位置 | 改动 |
|---|---|
| `android:` | 下面加 `if: ${{ false }}` |
| `windows:` | 下面加 `if: ${{ false }}` |
| `macos:` | 下面加 `if: ${{ false }}` |
| `ios:` 的 `strategy` | 不再用 `*editions` 锚点，改写独立 matrix，只留 zhenguojian |
| `ios:` 的 `runs-on` | `macos-15-intel`（**必改**，见下） |

```yaml
  ios:
    needs: prepare
    runs-on: macos-15-intel
    strategy:
      fail-fast: false
      matrix:
        include:
          - edition: zhenguojian
            arguments: '--all-sources'
            signing: ZHENGUOJIAN
```

**为什么必须是 `macos-15-intel`**：`macos-latest` / `macos-14` / `macos-15` 全是 arm64，同一个枯竭的池子；`macos-13` 已于 2025-12 退役。`macos-15-intel` 是唯一还在服务的 x86_64 镜像（保留到 2027-08），是独立池，实测秒分配。产物架构不受影响（iOS 是真机 arm64，交叉编译）。

> ⚠️ `&editions` 锚点被 android / windows / ios / macos 共用，**不能直接改锚点**，否则会连 Windows 一起改。

---

## 3. 新增一个站源的标准步骤（以后要加站用）

共 **8 个注册点**，漏一个就会出现「枚举里有、点进去空白」或「catalog 报内容分类无效」：

| # | 文件 | 改什么 |
|---|---|---|
| 1 | `native/core/provider_huangguo.go` | 加 `sourceXXX = "xxx"` 常量 |
| 2 | `native/core/provider_huangguo.go` | `canonicalProviderSource()` 加别名 case（含域名写法） |
| 3 | `native/core/app_categories.go` | 3 处 `switch source`（分类列表 / 默认分类 / 分类校验） |
| 4 | `native/core/app_cover_metadata.go` | 1 处 `switch source`（封面） |
| 5 | `native/core/app_metadata.go` | 1 处 `switch source`（详情元数据） |
| 6 | `native/core/app_runtime.go` | 4 处 `switch source`（catalog / detail / chapter / playback） |
| 7 | `native/core/provider_media.go` | 加 `configured, fallback = d.cfg.XxxURL, xxxBaseURL`，并在 `app_runtime.go` 的 Config 结构体加字段 |
| 8 | `lib/models.dart` | `restrictedValues` 加 `SourceSite('xxx', '中文名', '描述')`；支持在线搜索的还要加进 `pagedSearch` 的 `id ==` 链 |

再补两样：

- `native/core/provider_xxx.go` 实现文件（参考 `provider_xduanju.go` / `provider_huangtao.go`）
- `native/core/provider_xxx_test.go` 单测

**权威中文名**只认 `lib/models.dart` 的 `SourceSite`，**不要按 id 拼音猜**（`guipian` 是「鬼片」不是「桂片」，`hanxiaoquan` 是「韩小圈」，`sorani` 是「青空」）。

---

## 4. 作者发新版本时怎么操作

```
作者更新
   │
   ├─ ① 先判断架构（第 0 节两条命令）
   │
   ├─ 同架构（1.0.11 系 / core 161 文件）
   │     → 只把 P1–P6 逐项套到新代码上，一个文件一个文件比
   │     → 站源文件（provider_*.go）不要覆盖，除非要修某站
   │
   └─ 新架构（1.0.8 系 / core 67 文件 / 有 provider_host.go）
         → 全量替换会丢掉所有补丁，需要重写：
            P1 全部作废（新架构改用 APP_FLAVOR + 站源独立模块）
            P2 把 NativePreciseSearch 的入参类型从 nativeDrama 换成 LibraryDrama
            P3 可以整文件照搬（协议与 DTO 无关），但要确认 Host 接口是否仍提供 Fetch
            P4 整文件照搬即可（纯 Dart）
            P5 照抄 4 处改动
```

**两个硬性提醒**

1. **源码快照必须留**：每个交付批次都保留 `upload-1xx/` 目录，出问题能立刻回退到上一版。
2. **`build.yml` 会被覆盖**：如果把作者的新 `build.yml` 一起传进来，P5 就丢了，iOS 又会一次申请 4 台机器。传之前先看这个文件有没有被改。

---

## 5. 17 个站源（`lib/models.dart` → `SourceSite`，权威顺序）

| # | id | 中文名 | 归属 |
|---|---|---|---|
| 1 | `hongguo` | 红果 | 作者 |
| 2 | `dsd` | 帝果 | 作者 |
| 3 | `sorani` | 青空 | 作者 |
| 4 | `guipian` | 鬼片 | 作者 |
| 5 | `hanxiaoquan` | 韩小圈 | 作者 |
| 6 | `huangdou` | 黄豆 | 作者 |
| 7 | `huangju` | 剧果 | 作者 |
| 8 | `yeguo` | 野果 | 作者 |
| 9 | `jinguoduanju` | 禁果短剧 | 作者 |
| 10 | `jinguo` | 禁果 | 作者 |
| 11 | `heitao` | 黑桃 | 作者 |
| 12 | `beabox` | 笔盒 | 作者 |
| 13 | `xduanju` | X短剧 | **自加** |
| 14 | `huangtao` | 黄桃 | **自加** |
| 15 | `huangguo-video` | 黄果视频 | 作者 |
| 16 | `huangguoai` | 黄果 AI | 作者（播放逻辑由 P3 重写） |
| 17 | `cloudfront` | 黄果旧版 | 作者（旧 API 剧库） |

> 17 个站源的实现文件在 `native/core/` 里全部齐备（`provider_yeguo*`、`provider_dsd_*`、`provider_jinguoduanju*`、`provider_heitao.go`、`provider_beabox.go` …），**不存在「缺站源待迁移」这种情况**。以后只有「某站改版失效，去修」。

---

## 6. 装包后 5 分钟自检表

| # | 检查项 | 期望 |
|---|---|---|
| 1 | 关于页版本号 | `1.0.17（117）` |
| 2 | 站源总数 | 17 个 |
| 3 | App 名称 | 真果鉴（不是红果鉴） |
| 4 | 黄果 AI → 任意剧 → 第 1/2/3 集 | 能起播 |
| 5 | 黄桃 → 搜「琼明」 | 只剩「琼明神女录」；搜不存在的词显示「没有找到」 |
| 6 | 设置 → 外观 | 有「纯黑深色模式」开关且生效 |

---

## 7. 环境与 CI 的坑（血泪版）

| 坑 | 现象 | 解法 |
|---|---|---|
| macOS arm64 池枯竭 | `The job was not acquired by Runner of type hosted` + `capacity constraints`，精确排 15 分钟后 cancelled，0 个步骤 | 换 `macos-15-intel` |
| `GITHUB_TOKEN` 无 `workflow` 权限 | 提交含 `.github/workflows/` 改动被拒：`refusing to allow a GitHub App to create or update workflow` | 无法在 `permissions:` 开启；用 `git reset -q HEAD -- .github/workflows && git checkout HEAD -- .github/workflows` 保住原文件 |
| `.gitattributes`（`* text=auto`） | 行尾被归一化，blob 相同也被算改动 | 同上；或上传时保持 CRLF |
| `checks` 全绿是假的 | 所有步骤 `continue-on-error: true` | 只看 `ios` 的 build 步骤结果 |
| `android` 永远红 | 缺 `*_KEYSTORE_BASE64` Secrets，脚本里 `exit 1` | 已用 `if: ${{ false }}` 关闭 |
| `release` 永远 skipped | `needs` 含必然失败的 android | 正常，产物从 Artifacts 下载 |
| GitHub 未认证 API 60 次/小时 | 403 rate limit exceeded | 改抓 HTML：`curl -A "Mozilla/5.0" https://github.com/<owner>/<repo>/actions/runs/<id>`，去标签后匹配 `Status\s+(\w+)\s+Total duration` |
| 版本号未提 | `prepare` 因 tag 已存在而拦下 | 每次跑之前改 `pubspec.yaml` |

---

## 8. 快照索引（出问题回退用）

| 目录 | 内容 | 说明 |
|---|---|---|
| `upload-115/restore-1.0.15.tar.gz` | 1.0.11 基线 + 精准搜索 + 纯黑（480 文件） | 完整回滚基线 |
| `upload-119/` | `scripts/app_build.py`、`pubspec.yaml` | P1 修复（1.0.16+116） |
| `upload-120/` | P2 + P3 的 4 个 core 文件 + `pubspec.yaml` | 1.0.17+117 |
| `upload-117` / `upload-118` | 两版 `build.yml` | P5（118 是 `macos-15-intel` 终版） |
| `scratchpad/base115full/` | 1.0.17 完整源码树 | 本地 `go vet` / `go test` 用 |

**本地验证命令（Go 1.24.1）**

```bash
export PATH="$PWD/scratchpad/go/bin:$PATH" \
  GOPATH="$PWD/scratchpad/gopath" GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd scratchpad/base115full/native
go vet ./core
go test -run TestNativePreciseSearch -count=1 ./core
go build -o /dev/null ./core
```

> 本机没有 Flutter，**Dart 侧（P4）无法本地编译验证**，只能靠 CI 的 `flutter build ios` 兜。

---

## 9. 交接给其他 AI / 其他工具时的提示词模板

> 背景：`main` 分支就是当前可用版本（1.0.17+117，commit `79134fb`）。别的 AI 拉 `main` 拿到的就是这一版源码。
> **但「在 1.0.17 上改」不会自动成立**——它需要下面这些约束，否则最常见的两种事故是
> 「顺手把作者新版全量覆盖进来」和「顺手改了 build.yml」，两者都会把三个补丁一起弄丢。

### 第 0 步：先把本清单传进仓库

```
https://github.com/like2264/guoapp/upload/main   →   拖入 guoapp-移植清单.md
```

传上去之后，任何 AI 一打开仓库就能看到补丁清单与坑表，不必每次口头解释。

### 交接提示词（整段复制给对方）

```text
仓库：https://github.com/like2264/guoapp ，main 分支，当前版本 pubspec 1.0.17+117，
已验证可用的基线 commit 是 79134fb。

改之前先读仓库根目录的 guoapp-移植清单.md，里面有 6 个历史补丁（P1–P6）和 8 条 CI 坑。

硬性约束（违反任意一条都会让包坏掉，且多数情况下不报错）：
1. 不要动 .github/workflows/build.yml
   - ios 必须保持 runs-on: macos-15-intel（arm64 池会排队 15 分钟被回收）
   - android / windows / macos 三个 job 的 if: ${{ false }} 必须保留
2. 不要动 scripts/app_build.py 里的这两行（站源总开关，少了整包退化成只有 1 个站源的「红果鉴」）：
   --dart-define=ALL_SOURCES=<true|false>
   -X duanjuapp/native/core.buildAllSources=<true|false>
3. 不要恢复 native/core/app_search_precision.go 里「整页都不相关就原样返回」的兜底；
   app_runtime.go 的 nativeCatalog 必须保留「过滤后为空则 result.HasMore = false」。
4. 只改我指定的文件，逐文件输出 diff，不要全量覆盖；
   不要把作者新版本的其他文件一起引入（会覆盖掉 P1/P2/P3）。
5. pubspec.yaml 只改 version 一行，且必须大于当前版本（否则 prepare 会因 tag 已存在拦下整轮）；
   不要动依赖版本。
6. 不要改 bundle id，保持 com.guoapp.phoenix（否则无法覆盖安装）。
7. 改动必须能叠在 79134fb 之上增量生效，不要重写整个文件。

交付要求：给出「改了哪几个文件 + 每个文件改了什么 + 怎么验证」，
并说明是否影响 P1–P6 中的任何一项。
```

### 改完之后必须走的四步

| 步 | 动作 | 不做会怎样 |
|---|---|---|
| 1 | 提交到 **`main`**（不是 fork / 不是新分支） | 构建永远拉不到你的改动 |
| 2 | Actions → **Build app packages** → **Run workflow** | 这个仓库没有 push 自动构建，不点就没有包 |
| 3 | 版本号已 +1 | `prepare` 因 `refs/tags/app-v<version>` 已存在直接失败 |
| 4 | 产物从运行页最下方 **Artifacts** 下载 | release 页永远是 skipped（`needs` 含被跳过的 job） |

### 六种「静默失败」速查

| 现象 | 多半是 |
|---|---|
| App 名字变成「红果鉴」、站源只剩 1 个 | P1 被覆盖（`scripts/app_build.py` 退化成只传 `APP_FLAVOR`） |
| 搜索又出现大批无关作品（1 条对的 + 一堆明星换脸） | P2 的兜底被改回「原样返回」 |
| 黄果 AI 提示「该集暂无可播放地址」 | P3 被覆盖，或站点又改版（重新对 `edge-api-crypto.js`） |
| 排队 15 分钟、0 个步骤、`not acquired by Runner` | `runs-on` 被改回 `macos-latest` |
| `prepare` 秒失败 | 版本号没加 |
| 装完是两个 App / 提示签名冲突 | bundle id 被改 |
