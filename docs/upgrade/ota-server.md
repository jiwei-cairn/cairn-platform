# OTA 服务器目录与清单规范

> 版本：schema 1
> 状态：**已定稿，待实现**
> 相关文档：[decisions.md](./decisions.md) · [package-format.md](./package-format.md)

---

## 1. 概述

OTA 升级采用 **HTTP(S) 静态文件托管 + Pull 模式**。服务器只需提供静态文件，无需任何服务端逻辑。

**本规范定义**：
- 服务器目录结构
- 三份 `index.json` 的格式与职责
- 设备端的取用逻辑

---

## 2. 目录结构

```
{server_root}/                          ← 静态托管根目录
└── cairn/                              ← 平台固定前缀
    ├── index.json                      ← 总索引（唯一入口）
    │
    ├── services/                       ← 微服务域（原子单元）
    │   ├── index.json                  ← 服务清单
    │   ├── data-collector/
    │   │   ├── data-collector-1.3.0-linux-arm64.cairnpkg
    │   │   ├── data-collector-1.2.0-linux-arm64.cairnpkg
    │   │   ├── data-collector-1.3.0-windows-amd64.cairnpkg
    │   │   └── data-collector-1.2.0-windows-amd64.cairnpkg
    │   └── gateway/
    │       ├── gateway-2.0.1-linux-arm64.cairnpkg
    │       └── gateway-2.0.0-linux-arm64.cairnpkg
    │
    └── products/                       ← 产品 / 组态域（服务组合方案）
        ├── index.json                  ← 产品清单（本轮仅占位）
        └── smart-energy/
            ├── smart-energy-1.0.0.cairnproduct
            └── smart-energy-0.9.0.cairnproduct
```

### 设计理由

| 设计 | 理由 |
|---|---|
| 前缀 `cairn/` | 服务器上可能还托管其他内容，隔离命名空间 |
| `services/` 与 `products/` 分域 | 对应平台的两个核心概念：微服务（原子）与组态（组合） |
| 按 `{name}/` 建子目录 | 一个服务的所有历史版本聚在一起；清理某服务即删目录 |
| 文件名含完整四元组 | 人工可读；唯一标识（同名同版本同平台不可能重复） |
| 保留历史版本 | 供 Web 回退下载（decisions.md B8） |
| 清单放各域根级 | 设备只拉自己需要的域，无需下载无关清单 |

---

## 3. 三份 `index.json` 的职责

| 文件 | 职责 | 本轮是否实现 |
|---|---|---|
| `cairn/index.json` | **总索引**：告知两个域的清单在哪 | ✅ 实现 |
| `cairn/services/index.json` | **服务清单**：所有服务的所有版本 | ✅ 实现 |
| `cairn/products/index.json` | **产品清单**：所有组态方案 | ⚠️ **仅占位** |

**分层拉取的意义**：设备若只需升级服务，只拉 `cairn/index.json` + `cairn/services/index.json`；未来做编排时才拉 products 域。避免清单随规模膨胀。

---

## 4. 根索引 `cairn/index.json`

```jsonc
{
  "schema": 1,
  "generatedAt": "2026-11-09T12:00:00Z",

  "domains": {
    "services": {
      "index": "services/index.json",
      // 相对本文件的路径。设备据此拼接完整 URL。

      "count": 2,
      // 该域下的服务数量（供展示，非强制校验）

      "updatedAt": "2026-11-09T12:00:00Z"
    },

    "products": {
      "index": "products/index.json",
      "count": 0,
      "updatedAt": "2026-11-09T12:00:00Z",
      "placeholder": true
      // 本轮产品域仅占位。设备看到 placeholder=true 应跳过该域。
    }
  }
}
```

**字段说明**

| 字段 | 类型 | 必需 | 说明 |
|---|---|---|---|
| `schema` | int | ✅ | 格式版本，当前 1 |
| `generatedAt` | string | ✅ | RFC3339 时间，清单生成时刻 |
| `domains.services.index` | string | ✅ | 服务清单相对路径 |
| `domains.services.count` | int | 否 | 服务数量，展示用 |
| `domains.services.updatedAt` | string | 否 | 该域最近更新时刻 |
| `domains.products.index` | string | ✅ | 产品清单相对路径 |
| `domains.products.placeholder` | bool | 否 | true = 该域仅占位，设备应跳过 |

---

## 5. 服务清单 `cairn/services/index.json`

```jsonc
{
  "schema": 1,
  "generatedAt": "2026-11-09T12:00:00Z",

  "packages": [
    {
      "name": "data-collector",
      "version": "1.3.0",
      "os": "linux",
      "arch": "arm64",

      "url": "../services/data-collector/data-collector-1.3.0-linux-arm64.cairnpkg",
      // ⭐ 相对本清单文件的路径。
      // 好处：服务器迁移 / 加 CDN / 换域名时，设备配置一行不用改。

      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      // 整包 SHA-256（下载完成后先校验这个，再解包）
      // 与包内 manifest 的 files[] 逐文件哈希构成两级校验：
      //   整包哈希 → 快速确认下载完整
      //   逐文件哈希 → 精确定位损坏文件

      "sizeBytes": 12582912,
      "minPlatformVersion": "0.4.0",
      "releasedAt": "2026-11-08T09:00:00Z",
      "notes": "修复采集抖动"
    },
    {
      "name": "data-collector",
      "version": "1.2.0",
      "os": "linux",
      "arch": "arm64",
      "url": "../services/data-collector/data-collector-1.2.0-linux-arm64.cairnpkg",
      "sha256": "…",
      "sizeBytes": 12000000,
      "releasedAt": "2026-10-01T10:00:00Z",
      "notes": "上一稳定版（供回退）"
    }
  ]
}
```

### 字段说明

| 字段 | 类型 | 必需 | 说明 |
|---|---|---|---|
| `name` | string | ✅ | 服务名，与包内 manifest 一致 |
| `version` | string | ✅ | 语义化版本 |
| `os` | string | ✅ | `linux` / `windows` |
| `arch` | string | ✅ | `amd64` / `arm64` / `arm` |
| `url` | string | ✅ | **相对本清单的路径** |
| `sha256` | string | ✅ | 整包哈希（小写十六进制） |
| `sizeBytes` | int | ✅ | 整包字节数 |
| `minPlatformVersion` | string | 否 | 平台版本下限 |
| `releasedAt` | string | 否 | RFC3339 |
| `notes` | string | 否 | 发布说明 |

### 关键设计

| # | 设计 | 理由 |
|---|---|---|
| S1 | `url` 用**相对路径** | 换域名 / CDN / 迁移时设备配置不变 |
| S2 | **同时列出历史版本** | 设备可展示「可下载的版本」，用户回退时能直接选（B8） |
| S3 | 每条含 `os` / `arch` | 一份清单服务所有平台，设备按本机过滤 |
| S4 | 含整包 `sha256` | 下载后**快速**完整性校验（比解包验签快得多），失败立即重试 |
| S5 | `schema` 版本号 | 未来扩展字段不破坏旧设备 |
| S6 | `generatedAt` | 设备可判断清单是否陈旧（如超过 N 天告警） |

---

## 6. 产品清单 `cairn/products/index.json`（本轮占位）

```jsonc
{
  "schema": 1,
  "generatedAt": "2026-11-09T12:00:00Z",
  "placeholder": true,
  "note": "产品 / 组态域尚未实现，实现留待编排功能落地",
  "products": []
}
```

**本轮只建目录与占位文件**，不实现消费逻辑。设备端应识别 `placeholder: true` 并跳过。

---

## 7. 设备端取用逻辑

```
┌─ 用户打开升级页面 ────────────────────────────────┐
│                                                    │
│ 1. GET {ota.baseUrl}/cairn/index.json              │
│    ├─ 失败 → 用内置默认清单展示（只读）             │
│    └─ 成功 → 继续                                   │
│                                                    │
│ 2. GET {ota.baseUrl}/cairn/{domains.services.index}│
│                                                    │
│ 3. 过滤：packages 中 os == 本机 && arch == 本机     │
│                                                    │
│ 4. 按 name 分组，取 version 最大者为「最新版」      │
│    （语义化版本比较，预发布版低于正式版）           │
│                                                    │
│ 5. 与本地当前版本比较：                             │
│      相等 → 无更新                                  │
│      不等 → 标记「可升级」（D3：不一致即可升）      │
│                                                    │
│ 6. 展示：服务名 / 当前版本 / 最新版本 / 发布说明    │
│                                                    │
└────────────────────────────────────────────────────┘

用户点击「升级」后：

┌─ 升级执行 ────────────────────────────────────────┐
│ 7. 解析 url（相对清单文件）→ 得到完整下载 URL       │
│ 8. GET {完整URL}（支持 Range 断点续传）            │
│ 9. 下载完成 → 校验整包 sha256                      │
│     ├─ 不符 → 重试（最多 N 次）                     │
│     └─ 相符 → 继续                                  │
│ 10. 解包 → 按 package-format.md §6 校验             │
│ 11. 进入部署流水线（停服 → 换文件 → 启服）          │
└────────────────────────────────────────────────────┘
```

### 内置清单的作用

| 场景 | 行为 |
|---|---|
| 能拉到远程清单 | 以远程为准 |
| 拉不到（离线 / 服务器不可达） | 展示内置清单，**仅展示，不可安装**（无二进制包） |

内置清单通过 `go:embed` 编译进二进制（decisions.md I1）。

---

## 8. 本地模拟服务器

### 方式一：MobaXterm 自带 httpd

| 能力 | 是否支持 | 说明 |
|---|---|---|
| 静态文件 GET | ✅ | 可用 |
| `Range` 断点续传 | ❓ **需实测** | 若不支持，续传无法验证 |
| `Content-Length` | ✅ | 一般有 |
| 404 响应 | ✅ | 用于测「清单不存在」 |
| 故障注入（500 / 超时 / 坏包） | ❌ | 做不到 |

**用法**：把 `{server_root}` 指向本地目录，起 httpd 指向它，设备配置 `ota.baseUrl` 为本机 IP。

### 方式二：仓库内 Go devserver（推荐用于系统测试）

建议在 `cmd/devserver` 提供一个开发服务器：

```
GET  /cairn/**              静态托管（支持 Range）
POST /cairn/dev/publish     上传新包并自动更新 index.json（造数据用）
GET  /cairn/dev/fault/{mode}  故障注入：
                               · bad-hash       返回哈希不符的清单
                               · corrupt-pkg    返回截断的包
                               · slow           限速响应
                               · 500            返回服务器错误
```

**为什么需要 devserver**：MobaXterm 无法模拟失败路径，而升级功能的质量主要取决于**失败处理**是否正确。仅有成功路径的验证不足以说明可靠。

### 两种方式的定位

| 方式 | 定位 |
|---|---|
| MobaXterm | 快速验证「网络通不通、清单能否拉到」 |
| Go devserver | 系统测试：Range、故障注入、边界条件 |

**两者不冲突**，可先后使用。

---

## 9. 发布操作流程

```
# 发布一个新版本
1. 打包
   $ cairn-pack build --name data-collector --version 1.4.0 \
       --os linux --arch arm64 --binary ./build/collector --out ./dist/

2. 复制到服务器
   $ cp ./dist/data-collector-1.4.0-linux-arm64.cairnpkg \
        {server_root}/cairn/services/data-collector/

3. 更新清单
   $ cairn-publish --root {server_root}/cairn
   （扫描目录 → 读每个包的首条目 manifest → 重新生成三份 index.json）

4. 完成 —— 设备下次打开页面即可看到新版本
```

**`cairn-publish` 的职责**：
- 扫描 `cairn/services/*/` 下所有 `.cairnpkg`
- 读每包首个条目 `manifest.json`，提取 `name`/`version`/`os`/`arch`
- 计算整包 `sha256` 与 `sizeBytes`
- 生成 `cairn/services/index.json` 与 `cairn/index.json`
- 生成 `cairn/products/index.json`（占位）

> **第 3 步是唯一的重复劳动**。本轮可先手工维护 `index.json`（包少时可行），但包数量增加后必须工具化——手工维护极易出现「包更新了但清单没更新」的故障。

---

## 10. 清单维护的常见错误（注意事项）

| 错误 | 后果 |
|---|---|
| 上传了新包但忘记更新 `index.json` | 设备永远看不到新版本 |
| `sha256` 填错 | 设备下载后校验失败，反复重试 |
| `url` 用了绝对路径 | 换域名/CDN 后全线失效 |
| 删除旧包但清单仍有记录 | 回退时下载 404 |
| `version` 不是语义化格式 | 设备无法比较大小 |
| 清单 `os`/`arch` 与实际包不符 | 设备下载了错误平台的包 |

> **建议**：`cairn-publish` 在生成时对以上各项做校验，任一项不合规则报错退出，避免把错误发布出去。

---

## 11. 未来演进：带鉴权 / 分组的私有服务

当前为静态托管 + 无鉴权。未来演进方向（decisions.md B3）：

| 演进项 | 影响 |
|---|---|
| 设备鉴权 | 需要设备身份（如 deviceId + token），清单与包改用带鉴权的下载 URL |
| 分组 / 灰度 | 清单按 `channel` 或设备分组返回不同内容 |
| 私有源 | 需服务端逻辑（不再是纯静态） |

**为这些演进预留的设计**：
- `index.json` 的 `url` 是相对路径，未来可替换为签名 URL
- 清单含 `schema` 版本号，可扩展字段而不破坏旧设备
- 根索引已按域分层，未来可增加 `channels` 域

> 这些演进**不影响当前固件**，因为设计上已留出扩展位。
