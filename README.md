# DBX JumpServer 插件

在 DBX 中获取 JumpServer 数据库临时凭证，并填入 DBX 数据库连接表单的插件。适合经常需要在多个 JumpServer 数据库之间切换、又不想反复复制临时用户名和密码的场景。

> **状态：`0.1.0` 候选版，尚未发布到 DBX 商店。** 当前产物是未签名候选包，正式安装需要经过 DBX Store 或私有插件仓库的签名流程。

## 功能

- **多凭证配置**：一个 JumpServer 连接可以保存多组具名配置，分别记录组织、应用/资产、数据库、系统账号和目标 DBX 连接；支持另存、切换、重命名和删除，并恢复最后使用的配置。
- **一键刷新并填入**：获取新的 `connection-token` 后，通过本机 `dbx://` 链接打开已填好的 DBX 连接表单。现有连接默认只更新用户名和密码，也可选择同时更新连接地址。
- **自动发现连接地址**：通过 JumpServer `terminal/endpoints/smart/` 读取 `host` 和 `magnus_listen_db_port`，不需要手动填写数据库代理主机或端口。
- **支持数据库**：MySQL / MariaDB、PostgreSQL、SQL Server。MariaDB 映射为 DBX 的 MySQL 连接。
- **凭证不落地到配置**：AccessKey 和 AccessKey Secret 保存在 DBX Secret Store；临时密码不会写入工作台配置存储。

## 安装

### 开发环境

```bash
pnpm install
pnpm dev
```

需要 Node.js 22+ 和 pnpm。Go 后端需要 Go 1.23+，只使用标准库。

### 构建候选包

```bash
pnpm run package
```

当前生成的 Windows x64 候选包：

```text
dist/io.github.misaka10091.jumpserver-0.1.0-windows-x64.dbxp
dist/io.github.misaka10091.jumpserver-0.1.0-windows-x64.artifact.json
```

这是官方插件 CLI 生成的**未签名**候选包。正式 DBX 会校验安装包签名，不能直接当作商店已签名插件安装。macOS / Linux 需要在对应平台重新打包。

### 发布 GitHub Release

推送与 `manifest.json` 版本一致的 tag 即会触发 `.github/workflows/plugin-release.yml`：

```bash
git tag v0.1.0
git push origin v0.1.0
```

工作流会先创建 GitHub Release，再调用 DBX 官方 reusable workflow 构建并上传：

- 五个平台的未签名 `.dbxp`：Linux x64/ARM64、Windows x64、macOS Intel/Apple Silicon
- 每个包对应的 `.artifact.json`
- 汇总的 `release-candidates.json`

tag 必须是 `v<manifest.version>`，不匹配会直接失败。Release 资产不可覆盖，修改版本或内容时必须发布新的语义版本。

## 快速开始

1. 在 JumpServer 中创建个人 API Key，记录 **AccessKey ID** 和 **AccessKey Secret**。
2. 在 DBX 新建连接，选择 **JumpServer 凭证服务**。
3. 填写 JumpServer HTTPS 地址、AccessKey ID、AccessKey Secret；组织 ID 可留空，进入工作台后再选；令牌有效期默认 60 分钟。
4. 测试并连接该凭证服务，打开对应工作台。
5. 选择实际组织、资源类型（应用或资产）、数据库和系统账号。
6. 选择目标 DBX 连接，或保留“新建数据库连接”。
7. 点击 **刷新并填入 DBX**，在打开的 DBX 连接表单中检查并保存。

## 多配置

一个 JumpServer 凭证服务可以保存多套数据库凭证配置：

1. 先选择一套完整配置。
2. 点击 **另存为配置**，输入名称；首次保存会把自动创建的“默认配置”替换为该名称。
3. 切换配置后修改选择，修改会自动保存到当前配置。
4. 需要另一套组合时，再次 **另存为配置** 复制当前选择，再修改数据库或目标连接。
5. 用顶部下拉框切换配置，然后点击 **刷新并填入 DBX**。

切换配置不会创建令牌，只有点击刷新按钮时才会生成新凭证。最后一个配置不能删除；删除配置不会删除或修改 DBX 中已有连接。最多保存 50 个配置。

旧版本的 `binding:<connectionId>` 单条选择会自动迁移为“默认配置”，只迁移一次。

## 配置字段

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| 连接名称 | 是 | 例如“JumpServer / MySQL” |
| JumpServer 地址 | 是 | 自己的 HTTPS 地址，例如 `https://jumpserver.example.com` |
| AccessKey ID | 是 | JumpServer 个人 API Key ID |
| AccessKey Secret | 是 | JumpServer 个人 API Key Secret |
| 初始组织 ID | 否 | 留空时在进入工作台后选择实际组织 |
| 令牌有效期 | 是 | 默认 60 分钟，范围 1–1440 分钟 |

数据库代理主机和端口不需要填写，由 `terminal/endpoints/smart/` 自动返回。

## 工作方式

```text
DBX 工作台
  → Go Sidecar 调用 JumpServer API
  → 读取组织、应用/资产、系统账号
  → terminal/endpoints/smart/ 获取 host + magnus_listen_db_port
  → 创建 connection-token
  → 本机 dbx://connection/new 打开 DBX 连接表单
  → 用户检查并保存
```

数据库代理登录使用 `connection-token` 响应的原始 `id` 作为用户名，使用对应 `secret` 作为密码，不添加 `JMS-` 前缀。

### 关于 DBX 插件 API

DBX 的 `connection/action` 可以通过 `fieldValues` 更新插件自己声明的连接表单；`host.connections:read` 只提供已配置连接的脱敏摘要，目前没有修改现有内置数据库连接的 Host API。因此本插件通过本机 `dbx://connection/new?id=...` 打开 DBX 的连接编辑表单，由 DBX 完成实际保存。`window.dbxPlugin.openUrl` 只接受 HTTP / HTTPS，不能用于打开 `dbx://`。

DBX 工作台使用 `sandbox="allow-scripts"`，不放开原生表单提交，所以刷新按钮使用直接 `click` 处理，不依赖 `submit` 事件。

## 兼容与限制

- DBX 需要支持 `host.storage`、`host.connections:read` 和 `dbx://connection/new` 现有连接编辑。
- JumpServer 需要提供可用的数据库代理及 `terminal/endpoints/smart/` 接口。
- 当前适配 JumpServer v2 兼容接口的 `system_user` / `application` / `asset` 模型；仅提供 `account` 模型的新版本需要单独适配。
- 自动填入要求 Sidecar 与 DBX 桌面版运行在同一台电脑。Web / Docker 部署请使用 **获取参数** 后手动复制。
- 深链更新支持 DBX 内置 MySQL、PostgreSQL 和 SQL Server 的字段模式连接；特殊驱动或连接字符串模式可能被 DBX 拒绝。
- 尚未使用真实 JumpServer 凭据做完整端到端验证，也未在已签名安装的 DBX 客户端中完成最终验证。

## 安全说明

- AccessKey ID / Secret 由 DBX Secret Store 管理，Sidecar 仅在连接期间保留必要凭据。
- 工作台配置存储只包含配置名称、组织/资源/账号/目标 ID、数据库名和更新地址开关；不保存密码或令牌。
- JumpServer API 使用 HTTPS 证书校验，HTTP 跳转不会携带签名凭据继续请求。
- “刷新并填入 DBX”生成的链接包含临时用户名和密码，不要转发或截图分享。
- “获取参数”仅在当前工作台内显示临时凭证，可点击“清除显示”；复制密码会写入系统剪贴板。

## 常见问题

| 现象 | 排查方向 |
| --- | --- |
| 401 / `Invalid signature` | 检查 AccessKey、Secret 是否正确；本机时间是否准确。请求使用 UTC 的 GMT 日期并对实际查询串签名。 |
| 资源列表为空 | 切换实际组织；确认数据库在“我的应用”还是普通资产中，并检查该用户授权。 |
| 系统账号为空 | 检查账号授权，以及账号协议是否为 MySQL / MariaDB / PostgreSQL / SQL Server。 |
| 凭证服务未连接 | 先在 DBX 连接树连接“JumpServer 凭证服务”，再打开或重新加载工作台。 |
| 已创建令牌但没有打开 DBX | 检查本机是否注册了 `dbx://` 协议处理器；也可使用“获取参数”手动填入。 |
| DBX 提示先关闭连接窗口 | 关闭当前连接编辑窗口后回到插件重试，深链不会覆盖未保存草稿。 |
| smart 接口没有返回地址或端口 | 检查 JumpServer 数据库代理、应用路由和协议；插件使用 `magnus_listen_db_port`，不会使用 HTTP / SSH 端口。 |
| 提示令牌已过期或时间格式不支持 | 两种错误会分别提示。先检查系统时间，再确认响应时间是否带时区；支持 ISO 8601 和 JumpServer v2 的 `YYYY/MM/DD HH:mm:ss +0800`。 |
| 令牌生成但数据库登录失败 | 核对自动发现的代理地址、协议、令牌有效期和部署的数据库代理认证方式。用户名应为令牌原始 `id`，密码为对应 `secret`。 |
| 提示数据库类型不匹配 | 目标 DBX 连接的数据库类型必须与 JumpServer 系统账号协议一致。 |
| 更新连接被 DBX 拒绝 | 检查 DBX 版本是否支持现有连接深链更新，以及目标是否为内置字段模式连接。 |
| 点击按钮无反应 | 使用当前 0.1.x 版本并重新打开工作台。旧版依赖原生表单提交，会被 DBX 沙箱拦截。 |

## 项目结构

```text
.
├── manifest.json              # 插件 Manifest
├── dbx-plugin.toml            # 构建配置
├── ui/                        # 工作台界面
│   ├── index.html
│   ├── app.js
│   └── style.css
├── backend/                   # Go Sidecar
│   ├── main.go
│   ├── jumpserver.go
│   └── *_test.go
├── tests/                     # UI 与沙箱回归测试
├── .github/workflows/
│   └── plugin-release.yml     # tag 触发的 Release 工作流
├── docs/
│   └── JUMPSERVER_API.md      # JumpServer 接口开发参考
├── openapi.json               # JumpServer API 规格
└── connection_token.py        # 独立 API 验证脚本
```

## 开发与测试

```bash
pnpm run test:ui      # 多配置与界面交互测试
pnpm run test:browser # 真实 Chrome / Edge 沙箱回归
cd backend && go test ./...
cd backend && go vet ./...
pnpm run package      # 构建当前平台的未签名候选包
```

测试使用本机模拟 JumpServer 和模拟 DBX 桥接，不会创建真实服务端令牌，也不会打开真实 DBX。

## 文档

- 本文档即完整使用说明
- [JumpServer API 接入参考](docs/JUMPSERVER_API.md)
- [JumpServer OpenAPI 规格](openapi.json)
- [DBX 插件开发文档](https://dbxio.com/cn/docs/plugin-development)

## License

[Apache-2.0](LICENSE)
