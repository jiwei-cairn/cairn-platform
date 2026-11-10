# `.cairnpkg` 包格式规范

> 版本：schema 1
> 状态：**已定稿，待实现**
> 相关文档：[decisions.md](./decisions.md) · [ota-server.md](./ota-server.md)

---

## 1. 概述

`.cairnpkg` 是积微 Cairn 的**微服务分发单元**，用于：

- 本地安装（Web 上传）
- 本地升级 / 回退（Web 上传）
- 远程升级（OTA 下载）

**三种场景使用完全相同的包格式**，差异仅在「包从哪来」。

---

## 2. 容器格式

| 项 | 规定 |
|---|---|
| 容器 | **ZIP**（deflate 或 store 均可） |
| 扩展名 | `.cairnpkg` |
| 命名 | `{name}-{version}-{os}-{arch}.cairnpkg` |
| 示例 | `data-collector-1.3.0-linux-arm64.cairnpkg` |

**为什么用 ZIP**：Go 标准库原生支持、Windows 可双击查看、多数工具可解、支持流式读取。压缩率不是目标，可维护性更重要。

---

## 3. 包内结构

```
{name}-{version}-{os}-{arch}.cairnpkg
├── manifest.json                    ← 必需，必须是第一个条目
├── bin/{可执行文件}                 ← 主程序，路径由 manifest.entrypoint 指定
├── dependencies/                    ← 可选：动态库等运行时依赖
│   └── *.so / *.dll
└── config/                          ← 可选：初始配置
    └── *.conf
```

### 硬性约束

| # | 约束 | 校验时机 |
|---|---|---|
| Z1 | `manifest.json` **必须是 zip 中第一个条目** | 解包前（流式首读） |
| Z2 | 条目路径**不得含 `..`**、不得以 `/` 或盘符开头 | 逐条目校验 |
| Z3 | 除 `manifest.json` 外，每个条目路径**必须在 `files[]` 中登记** | 解包后比对 |
| Z4 | **不得含符号链接、硬链接、设备文件** | 解包时 |
| Z5 | 解压总量 **≤ `totalSizeBytes`**（防 zip bomb） | 流式累计 |
| Z6 | 单文件路径长度 ≤ 255 字符 | 解包前 |

> **Z1 的意义**：允许「先读清单 → 判断兼容性 → 再决定是否下载/解包」，避免无谓传输。
> 对 OTA 尤其重要：设备可先拿到 manifest 发现「架构不匹配」而不下载整个包。

---

## 4. `manifest.json` 字段定义

```jsonc
{
  // ═══════════ 格式元信息（必需）═══════════
  "schema": 1,
  // 包格式版本。当前固定 1。平台据此决定解析逻辑，
  // 未来格式演进时递增，旧平台可明确拒绝新格式包。

  "name": "data-collector",
  // 服务唯一标识，同时是安装目录名。
  // 正则：^[a-z0-9][a-z0-9._-]{0,63}$

  "version": "1.3.0",
  // 语义化版本，强制。
  // 正则：^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$

  "os": "linux",
  // 目标平台：linux | windows

  "arch": "arm64",
  // 目标架构：amd64 | arm64 | arm（GOARCH 命名）

  "entrypoint": "bin/data-collector",
  // 主可执行文件在包内的相对路径。必需，不做任何推理。
  // ⚠️ 与旧格式的 binary 字段不同：旧格式缺省会推理为 bin/{name}，
  //    新格式要求显式声明，避免「服务名与文件名不一致」时找不到文件。

  "totalSizeBytes": 12582912,
  // 解压后总字节数（所有 files[] 之和）。防 zip bomb。

  // ═══════════ 文件清单（必需）═══════════
  "files": [
    {
      "path": "bin/data-collector",
      // 包内相对路径，使用 / 分隔符（跨平台统一）

      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      // 文件内容 SHA-256，小写十六进制

      "sizeBytes": 12582912,
      // 文件字节数

      "mode": "0755",
      // Unix 权限八进制串。Windows 忽略。
      // 约定：可执行文件 0755，普通文件 0644

      "role": "binary"
      // 文件角色，用于升级时的处理策略：
      //   binary     主程序（必需，且必须与 entrypoint 一致）
      //   dependency 运行时依赖（动态库等）
      //   config     配置文件（升级时按策略处理，见 §7）
      //   data       随包携带的初始数据（少见）
    }
  ],

  // ═══════════ 展示信息（可选）═══════════
  "displayName": "数据采集器",
  "author": "…",
  "description": "…",
  "tags": ["collector", "iot"],
  "webUrl": "http://localhost:8080",
  "releaseNotes": "修复采集抖动",

  // ═══════════ 运行约束（可选，缺省用平台默认）═══════════
  "args": ["-c", "config/app.conf"],
  // 启动参数。相对路径以服务工作目录为基准。

  "env": { "CAIRN_LOG_LEVEL": "info" },
  // 追加的环境变量（与系统环境合并，同名覆盖）

  "deps": ["gateway"],
  // 依赖的服务名列表。安装/升级时解析为服务 id。

  "ports": [8080],
  // 端口占用预检。为空则不检查。

  "minDiskMB": 50,
  // 安装所需最小磁盘余量

  "restartPolicy": {
    "mode": "backoff",        // backoff | periodic
    "backoffBase": 2,         // 秒
    "backoffMax": 60,         // 秒
    "periodicSec": 3,         // mode=periodic 时的固定间隔
    "maxRetries": 0,          // 0 = 不熔断
    "windowSec": 0
  },

  "healthCheck": {
    "type": "http",           // none | http | tcp
    "target": "http://127.0.0.1:8080/health",
    "intervalSec": 10,
    "timeoutSec": 3,
    "readyHoldSec": 5,
    "failuresThreshold": 3,
    "recoverThreshold": 2,
    "degradedRestartSec": 30
  },

  "logPolicy": {
    "mode": "file",           // stdout | file
    "path": "logs/app.log",   // 相对服务工作目录
    "maxSizeMB": 10,
    "maxBackups": 3
  },

  "configFile": {
    "path": "config/app.conf",
    "validateCmd": "",
    "applyMode": "restart",   // restart | command
    "applyCmd": ""
  },

  // ═══════════ 兼容性（可选）═══════════
  "minPlatformVersion": "0.4.0",
  // 低于此平台版本时拒绝安装

  // ═══════════ 签名（预留，本轮恒为 null）═══════════
  "signature": null
  // 本轮不实现签名，字段保留以便未来启用时不破坏兼容。
  //
  // 未来启用时的结构：
  //   "signature": {
  //     "algo": "ed25519",
  //     "keyId": "release-2026",
  //     "sig": "base64..."
  //   }
  //
  // 若未来需要多签（密钥轮换的双签过渡期），允许为数组：
  //   "signature": [
  //     { "algo":"ed25519", "keyId":"dev-2026",     "sig":"..." },
  //     { "algo":"ed25519", "keyId":"release-2026", "sig":"..." }
  //   ]
  //
  // 解析规则（本轮）：
  //   字段缺失 或 值为 null → 跳过验签，视为合法
  //   字段非 null         → 本轮平台明确拒绝（「本版本不支持签名包」）
  //                         这样未来启用时不会出现「旧平台静默接受未验签包」
}
```

---

## 5. 版本号规范

```
^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$
```

| 规则 | 说明 |
|---|---|
| 必须三段数字 | 拒绝 `v1`、`1.0`、`latest` |
| 预发布后缀允许 | `-rc.1`、`-beta.2` |
| 构建元数据允许 | `+build.7`（不参与比较） |
| 比较规则 | MAJOR.MINOR.PATCH 数值比较；预发布版**低于**同版本正式版 |

**用户可否覆盖**：可以，但必须仍符合此正则，且 UI 标注「已手动覆盖」。

> **为什么强制**：版本号是机器的标识符，不是给人看的标签。
> 展示名应在 `displayName`。版本号不统一会导致「OTA 清单里的 1.3.0 与本地手填的 v1.3 永远匹配不上」。

---

## 6. 验签与校验流程

### 本轮（无签名）

```
1. 打开包 → 读第一条目 manifest.json
2. 解析 JSON → 校验字段（schema/name/version/os/arch/entrypoint/files 完整性）
3. 校验 §3 的 Z1–Z6 约束
4. 校验 os / arch 与本机匹配
5. 校验 version 符合语义化正则
6. 逐条目：核对路径在 files[] 中、解压后比对 sha256 与 sizeBytes
7. 校验解压总量 == totalSizeBytes
8. 通过 → 进入部署流水线
```

### 未来（有签名时，仅供参考）

```
在第 2 步后插入：
  2.5 若 signature 非 null：
        a. 取出 signature，从 manifest 中剔除该字段
        b. 按键名字母序重新序列化为紧凑 JSON → 待验签字节
        c. 用 keyId 在 TrustedKeys 中查公钥
        d. ed25519.Verify(公钥, 待验签字节, sig)
        e. 通过则继续；失败则拒绝
```

**为什么签 manifest 而非整包**：ZIP 的元数据（时间戳、压缩级别、条目顺序）在不同机器上不同，签整包会导致「同一份内容打出的包验签失败」。签 manifest 后，文件完整性由 `files[].sha256` 保证，形成「签名保护清单，清单保护文件」的信任链。

---

## 7. 升级时的文件处理策略

升级 = **覆盖**，但不同 `role` 的文件处理方式不同：

| role | 升级行为 |
|---|---|
| `binary` | 直接覆盖（旧文件先重命名为 `.old`，见 decisions.md §11） |
| `dependency` | 直接覆盖；旧版独有的依赖文件**删除**（避免残留） |
| `config` | 覆盖，但**覆盖前备份**为 `{filename}.bak`（见下） |
| `data` | 若目标位置已存在则**保留不覆盖**（避免冲掉用户数据） |

### 配置文件的备份规则

```
升级前：  config/app.conf        → config/app.conf.bak
升级时：  包内 config/app.conf   → 写入 config/app.conf
```

**为什么备份**：单个配置文件仅几 KB，备份成本可忽略；但能避免「用户改了配置，升级后莫名失效」这类难排查的问题。
**与「不做数据备份」不冲突**：前者是几 KB 文本，后者是几百 MB 数据。

> 注意：`config/app.conf.bak` 在下次升级时会被覆盖（只保留最近一次），不构成版本管理。

---

## 8. 导出安装包

平台需支持把**已安装服务的当前状态**导出为 `.cairnpkg`：

```
POST /api/services/{id}/export
  → 读取当前服务的：
      · 可执行文件与依赖（从 services/{name}/ 收集）
      · 配置文件（从 config/ 收集）
      · 运行参数（从 DB 的 Service 定义生成 manifest 字段）
  → 按本规范打包
  → 返回 .cairnpkg 下载
```

**导出时的版本来源**：取服务当前记录的版本号（用户手填的值，已通过语义化校验）。

**导出包可否直接用于升级**：可以。导出包与手工打包的包格式一致，无任何区别。

---

## 9. 打包工具（`cairn-pack`）

需要提供一个 CLI 工具：

```
cairn-pack build \
  --name data-collector \
  --version 1.3.0 \
  --os linux --arch arm64 \
  --entrypoint bin/data-collector \
  --binary ./build/data-collector \
  --dependency ./build/libfoo.so \
  --config ./config/app.conf \
  --args "-c" "config/app.conf" \
  --out data-collector-1.3.0-linux-arm64.cairnpkg
```

工具职责：
1. 收集文件、计算 sha256、生成 `files[]`
2. 生成 `manifest.json`（`signature: null`）
3. 按 ZIP 打包，**确保 manifest.json 是第一个条目**
4. 输出合规包

> 本轮可先用脚本近似实现，但最终应提供正式 CLI，避免手工打包出错。

---

## 10. 校验错误码（建议）

| 码 | 含义 |
|---|---|
| `PKG_SCHEMA_UNSUPPORTED` | schema 版本不被支持 |
| `PKG_NOT_ZIP` | 不是有效 ZIP |
| `PKG_MANIFEST_MISSING` | 缺少 manifest.json |
| `PKG_MANIFEST_NOT_FIRST` | manifest.json 不是第一个条目 |
| `PKG_FIELD_INVALID` | 字段格式非法（name/version/os/arch 等） |
| `PKG_VERSION_NOT_SEMVER` | 版本号不符合语义化规范 |
| `PKG_PLATFORM_MISMATCH` | os / arch 与本机不匹配 |
| `PKG_FILE_UNLISTED` | 包内存在未登记的文件 |
| `PKG_FILE_MISSING` | 清单登记的文件在包内缺失 |
| `PKG_HASH_MISMATCH` | 文件 sha256 不符 |
| `PKG_SIZE_MISMATCH` | 解压总量与 totalSizeBytes 不符 |
| `PKG_PATH_TRAVERSAL` | 条目路径含 `..` 或绝对路径 |
| `PKG_SYMLINK_REJECTED` | 包内含符号链接 |
| `PKG_SIZE_EXCEEDED` | 超出 totalSizeBytes（zip bomb 防护） |
| `PKG_SIGNATURE_UNSUPPORTED` | 包带签名但本版本不支持 |
| `PKG_PLATFORM_VERSION_TOO_LOW` | 低于 minPlatformVersion |
