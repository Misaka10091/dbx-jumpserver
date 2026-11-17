# JumpServer API 接入参考

> 本文是插件底层接口的开发参考，不属于插件安装或使用说明。插件用户请阅读项目根目录的 [README](../README.md)。
> 示例脚本：[connection_token.py](../connection_token.py)；API 规格：[openapi.json](../openapi.json)。

## 一、接入总览

1. 准备 AccessKey
2. 认证方式：HTTP Signature 签名
3. 组织隔离：X-JMS-ORG
4. 业务接口流程
5. 踩坑点速查表

## 二、准备 AccessKey

登录 JumpServer Web 控制台 → 右上角头像 → **个人设置** → **API Key**
（或直接访问 `/luna/#/profile`）→ 创建。

- **AccessKey ID**：UUID 格式
- **AccessKey Secret**：创建时显示一次，务必保存

接入前确认：

- AccessKey 未被禁用、未过期
- 所属用户账号未被禁用
- 该用户已登录过至少一次（某些版本 LDAP/AD 用户需要先登录初始化）

## 三、认证方式：HTTP Signature 签名

JumpServer 的 AccessKey **不是** JWT，而是 [HTTP Signature](https://datatracker.ietf.org/doc/html/draft-cavage-http-signatures) 草案风格的请求头签名：

```text
Authorization: Signature keyId="<AK>",algorithm="hmac-sha256",headers="(request-target) accept date",signature="<sig>"
```

签名字符串由三部分组成：

```text
(request-target): <小写method> <完整路径含查询字符串>
accept: application/json
date: <GMT格式时间>
```

对签名字符串做 `HMAC-SHA256(secret)` 再 `base64` 编码即得 signature。
**`accept` / `date` 两个头必须原样随请求发送**，否则服务端用实际的头值验签会失败。

Python 伪代码：

```python
import hmac, hashlib, base64, datetime
from urllib.parse import urlencode

GMT = "%a, %d %b %Y %H:%M:%S GMT"

def sign(ak, sk, method, full_path):
    date = datetime.datetime.utcnow().strftime(GMT)
    headers = {"Accept": "application/json", "Date": date}
    string_to_sign = (
        "(request-target): %s %s\n"
        "accept: application/json\n"
        "date: %s"
    ) % (method.lower(), full_path, date)
    sig = base64.b64encode(
        hmac.new(sk.encode(), string_to_sign.encode(), hashlib.sha256).digest()
    ).decode()
    headers["Authorization"] = (
        'Signature keyId="%s",algorithm="hmac-sha256",'
        'headers="(request-target) accept date",signature="%s"'
    ) % (ak, sig)
    return headers
```

### 签名相关的坑

1. **`(request-target)` 必须用完整路径**（含 `/api/v1` 前缀）
   - 正确：`/api/v1/users/profile/`
   - 错误：`/users/profile/` → `Invalid signature.`
2. **带查询参数时，查询字符串必须进入签名**，且编码方式必须与实际请求一致
   - 用 `urlencode(params)` 生成签名串，再用同样的 `params` 发请求
   - 错误：签名只含路径、请求带 `?limit=100&offset=0` → `Invalid signature.`
3. **`date` 对系统时间敏感**：本机时钟与服务端偏差过大 → 验签失败
4. **每个请求都要重新生成 `date` 并重签**，不能复用旧 header
5. 用 `Authorization: Bearer <token>` 的方式是给账号密码登录换取的 session token 用的，不适用于 AccessKey

## 四、组织隔离：X-JMS-ORG

JumpServer v3 数据按组织隔离。所有请求需要带：

```text
X-JMS-ORG: <组织ID>
```

### 组织相关坑

1. **“所有组织”（全局）的 ID 是全零 UUID**：
   ```text
   00000000-0000-0000-0000-000000000000
   ```
   不要记成 `...0001` 或 `...0002`（那是别的东西）。
2. **`Default` 组织** ID 是 `00000000-0000-0000-0000-000000000002`。如果实际资产在别的组织，用 Default 查出来会永远为空，此时返回 `{"count": 0, "results": []}` 不会报错，容易误判为“没权限”。
3. **组织列表不用手动查**：`GET /api/v1/users/profile/` 响应里直接带 `workbench_orgs` 数组，从中选择即可：
   ```json
   "workbench_orgs": [
     {"id": "000...000", "name": "所有组织", "is_root": true},
     {"id": "000...002", "name": "Default", "is_default": true},
     {"id": "11111111-1111-4111-8111-111111111111", "name": "研发组织"},
     ...
   ]
   ```

## 五、业务接口流程

以创建连接令牌获取某资产的连接凭证为例：

```text
[1] GET /api/v1/users/profile/
    → 验证签名，顺带拿 workbench_orgs

[2] 选择组织，设置 X-JMS-ORG

[3] 列出资源（二选一）：
    普通资产: GET /api/v1/perms/users/assets/?limit=100&offset=0[&search=xxx]
    应用发布: GET /api/v1/perms/users/applications/?limit=100&offset=0

[4] 列出可用系统用户（账号）：
    普通资产: GET /api/v1/perms/users/assets/<asset_id>/system-users/
    应用发布: GET /api/v1/perms/users/applications/<app_id>/system-users/

[5] 创建连接令牌：
    POST /api/v1/authentication/connection-token/
    {
      "id": "<uuid>",
      "type": "asset" | "application",
      "secret": "<随机串>",
      "date_expired": "2026-11-17T16:30:45.781Z",
      "asset": "<asset_id>",           # type=asset 时
      "application": "<app_id>",       # type=application 时
      "system_user": "<系统用户id>"
    }

[6] （可选）获取客户端连接 URL：
    GET /api/v1/authentication/connection-token/client-url/?id=<token_id>
```

### 请求体 / 响应相关的坑

- **`date_expired` 必须传具体时间**（ISO 8601），不能传 `null`
  → 传 null 返回 `400 {"date_expired":["该字段不能为 null。"]}`
- **系统用户接口返回可能是纯数组**，不是统一的分页对象 `{count, results}`，解析前先判断类型
- **应用 ≠ 资产**：Web 界面“我的应用”（如 MySQL/数据库发布）走 `application` 类接口，创建 token 时 `type` 必须用 `"application"`，传 `"asset"` 会 404/400
- 资产的“名字”在 `name` 字段，`hostname` 通常只是 IP，展示时优先 `name`
- POST 请求记得 `Content-Type: application/json`，发 JSON body 而不是 form

## 六、踩坑点速查表

| 症状 | 原因 | 解法 |
|---|---|---|
| 401 `Invalid signature.`（无参数请求） | `(request-target)` 没带 `/api/v1` 前缀 | 签名用完整路径 |
| 401 `Invalid signature.`（带参数请求） | 查询字符串没进签名 | `urlencode(params)` 拼进签名串 |
| 401 `authentication_failed` | Secret 错 / Key 禁用 / 用户禁用 | 检查凭据与账号状态 |
| 401 时钟相关 | 本机时间偏差大 | 校准系统时间 |
| 资产列表空 | 组织不对（默认在 Default，资产在别处） | 从 `profile.workbench_orgs` 选对组织，设置 `X-JMS-ORG` |
| 资产“对不上”Web 界面 | Web 树上是应用不是资产 | type 选 `application`，走 applications 接口 |
| 400 `date_expired 不能为 null` | 请求体传了 null | 传 ISO 时间，如 `+60min` |
| `AttributeError: 'list' object has no attribute 'get'` | 接口返回纯数组 | 判断类型后取 `results` 或直接用 |
| 字段“没名字” | `hostname` 是 IP | 展示优先 `name` 字段 |

## 七、运行示例脚本

在项目根目录准备本地 `.env`（不要提交到版本库）：

```bash
# .env
JMS_ACCESS_KEY=<你的AK>
JMS_SECRET=<你的Secret>
JMS_ORG=<可选,组织ID,跳过交互选择>
```

然后运行：

```bash
python connection_token.py
```

依赖：`pip install requests`（`.env` 解析在未安装 `python-dotenv` 时用内置兜底实现）。

脚本流程：

1. 从 `.env` 读取凭据（`JMS_ACCESS_KEY` / `JMS_SECRET`，可选 `JMS_ORG`）
2. 每次请求自动生成 `date` 并按 HTTP Signature 规则签名（完整路径 + 查询串）
3. 验证 `/users/profile/`，列出 `workbench_orgs` 供选择
4. 资产 / 应用二选一，列出资源与系统用户
5. 创建 ConnectionToken，可选获取 Client URL

## 八、其它说明

- **Private Token**：如果 AccessKey 不方便，也可在服务器上用 `python manage.py shell` 生成用户的 private token，用 `Authorization: Token <token>` 方式调用（参考官方文档）
- **Session**：登录后 Cookie 的 `jms_sessionid` 也可作为认证，适合浏览器内场景
- API 文档地址：`https://jumpserver.example.com/api/docs/`（Swagger UI），OpenAPI 原始 JSON：`?format=openapi`
- 官方文档：https://docs.fit2cloud.com/jumpserver/v3/dev/rest_api/
