#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
JumpServer API 交互式脚本:用 AccessKey/Secret 调用
    POST /api/v1/authentication/connection-token/
前置步骤:先选择资产(asset)和系统用户(system_user)

认证方式(HTTP Signature 草案):
    Authorization: Signature keyId="<ak>",algorithm="hmac-sha256",
                   headers="(request-target) accept date", signature="<sig>"
    signature = base64( HMAC-SHA256( secret,
        "(request-target): <method> <path>\naccept: application/json\ndate: <GMT时间>" ) )

依赖: pip install requests
"""

import base64
import datetime
import hashlib
import hmac
import json
import os
import secrets
import sys
import time
import uuid
from urllib.parse import urlencode

try:
    import requests
    requests.packages.urllib3.disable_warnings()
except ImportError:
    print("缺少依赖: pip install requests")
    sys.exit(1)

JMS_URL = "https://jumpserver.example.com"   # 示例地址，请替换为自己的 JumpServer 地址
X_ORG = "00000000-0000-0000-0000-000000000002"     # 默认组织
GMT_FMT = "%a, %d %b %Y %H:%M:%S GMT"


# ---------------------------- 签名 ----------------------------
def generate_signature(secret: str, string_to_sign: str) -> str:
    h = hmac.new(secret.encode("utf-8"), string_to_sign.encode("utf-8"),
                 hashlib.sha256)
    return base64.b64encode(h.digest()).decode("utf-8")


def build_headers(ak: str, sk: str, method: str, path: str,
                  content_type=None, org_id=None):
    """生成带 HTTP Signature 认证的请求头"""
    signature_headers = ["(request-target)", "accept", "date"]
    date = datetime.datetime.utcnow().strftime(GMT_FMT)
    headers = {
        "Accept": "application/json",
        "X-JMS-ORG": org_id or X_ORG,
        "Date": date,
    }
    # 构建待签名字符串(request-target 用小写 method)
    request_target = "%s %s" % (method.lower(), path)
    string_to_sign = "(request-target): %s\n" % request_target
    for name in signature_headers[1:]:
        # accept/date 均用请求头中的值
        string_to_sign += "%s: %s\n" % (name, headers[name.capitalize()])
    string_to_sign = string_to_sign.rstrip("\n")
    sig = generate_signature(sk, string_to_sign)
    headers["Authorization"] = (
        'Signature keyId="%s",algorithm="hmac-sha256",'
        'headers="%s",signature="%s"' % (ak, " ".join(signature_headers), sig))
    if content_type:
        headers["Content-Type"] = content_type
    return headers


# ---------------------------- HTTP 客户端 ----------------------------
class JumpServerClient:
    def __init__(self, base_url, ak, sk, org_id=None, timeout=30):
        self.base = base_url.rstrip("/")
        if not self.base.endswith("/api"):
            self.base += "/api"
        self.base += "/v1"
        self.ak, self.sk = ak, sk
        self.org_id = org_id  # None -> X-JMS-ORG 默认值
        self.timeout = timeout
        self.session = requests.Session()

    def _request(self, method, path, params=None, body=None,
                  content_type=None):
        full_path = "/api/v1" + path  # (request-target) 必须是完整路径
        if params:
            # 签名需包含查询字符串,且与实际发送的一致
            full_path += "?" + urlencode(params)
        headers = build_headers(self.ak, self.sk, method, full_path,
                                 content_type, org_id=self.org_id)
        return self.session.request(
            method, self.base + path, headers=headers,
            params=params, json=body,
            verify=False, timeout=self.timeout)

    def get(self, path, params=None):
        return self._request("GET", path, params=params)

    def post(self, path, body=None):
        return self._request("POST", path, body=body,
                             content_type="application/json")

    def _json(self, resp):
        if resp.status_code >= 300:
            print("\n[HTTP %s] %s" % (resp.status_code, resp.url), file=sys.stderr)
            print(resp.text[:2000], file=sys.stderr)
            resp.raise_for_status()
        return resp.json()

    # ---------------- 业务 API ----------------
    def my_profile(self):
        return self._json(self.get("/users/profile/"))

    def current_org(self):
        return self._json(self.get("/orgs/orgs/current/"))

    def list_my_assets(self, limit=100, search=None):
        params = {"limit": limit, "offset": 0}
        if search:
            params["search"] = search
        return self._json(self.get("/perms/users/assets/", params=params))

    def list_my_applications(self, limit=100, search=None):
        params = {"limit": limit, "offset": 0}
        if search:
            params["search"] = search
        return self._json(self.get("/perms/users/applications/", params=params))

    def list_app_system_users(self, app_id):
        return self._json(self.get(
            "/perms/users/applications/%s/system-users/" % app_id))

    def list_asset_system_users(self, asset_id):
        return self._json(
            self.get("/perms/users/assets/%s/system-users/" % asset_id))

    def create_connection_token(self, token_type, asset_id=None,
                                system_user_id=None, application_id=None,
                                expire_minutes=60):
        date_expired = datetime.datetime.now(
        ) + datetime.timedelta(minutes=expire_minutes)
        body = {
            "id": str(uuid.uuid4()),
            "type": token_type,  # asset | application
            "secret": secrets.token_urlsafe(16),
            "date_expired": date_expired.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z",
        }
        if token_type == "asset":
            body["asset"] = asset_id
            body["system_user"] = system_user_id
        else:
            body["application"] = application_id
            body["system_user"] = system_user_id
        return self._json(
            self.post("/authentication/connection-token/", body))

    def get_client_url(self, token_id):
        return self._json(self.get(
            "/authentication/connection-token/client-url/",
            params={"id": token_id, "ids": token_id}))


# ---------------------------- 配置 (.env) ----------------------------
BASE_DIR = os.path.dirname(os.path.abspath(__file__))
ENV_FILE = os.path.join(BASE_DIR, ".env")
ENV_EXAMPLE = os.path.join(BASE_DIR, ".env.example")


def _parse_dotenv(path):
    """极简 .env 解析(无 python-dotenv 依赖时兜底):支持 KEY=value 与 # 注释"""
    cfg = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, _, v = line.partition("=")
            cfg[k.strip()] = v.strip().strip('"').strip("'")
    return cfg


def load_env():
    """按 Python 标准做法从同目录 .env 读取 JMS_ACCESS_KEY / JMS_SECRET"""
    template = (
        "# JumpServer API 凭据\n"
        "JMS_ACCESS_KEY=你的AccessKeyID\n"
        "JMS_SECRET=你的AccessKeySecret\n"
    )
    if not os.path.exists(ENV_FILE):
        with open(ENV_EXAMPLE, "w", encoding="utf-8") as f:
            f.write(template)
        print("未找到 .env,已生成模板: %s" % ENV_EXAMPLE)
        print("复制为 .env 并填入 JMS_ACCESS_KEY / JMS_SECRET 后重新运行。")
        sys.exit(1)
    try:
        from dotenv import load_dotenv  # python-dotenv(推荐)
        load_dotenv(ENV_FILE, override=True)
        ak = os.environ.get("JMS_ACCESS_KEY", "").strip()
        sk = os.environ.get("JMS_SECRET", "").strip()
    except ImportError:
        cfg = _parse_dotenv(ENV_FILE)
        ak, sk = cfg.get("JMS_ACCESS_KEY", "").strip(), cfg.get("JMS_SECRET", "").strip()
    if not ak or not sk or ak == "你的AccessKeyID":
        print("请先在 %s 中填写 JMS_ACCESS_KEY / JMS_SECRET" % ENV_FILE)
        sys.exit(1)
    return ak, sk


# ---------------------------- 交互 ----------------------------
def ask(prompt, default=None):
    while True:
        try:
            value = input(prompt).strip()
        except (EOFError, KeyboardInterrupt):
            print("\n已退出")
            sys.exit(1)
        if value:
            return value
        if default is not None:
            return default
        print("不能为空,请重新输入")


def choose(items, label, name=None):
    if not items:
        print("没有可选项: %s" % label)
        sys.exit(1)
    print("\n== %s ==" % label)
    for i, it in enumerate(items):
        if callable(name):
            display = name(it)
        else:
            display = (it.get(name) if name else None) \
                or it.get("hostname") or it.get("name") \
                or it.get("username") or it.get("id")
        extra = it.get("ip") or it.get("address") or it.get("protocol") or ""
        print("  [%2d] %-38s %s%s" % (
            i, str(it.get("id", ""))[:38], display,
            ("  (%s)" % extra) if extra else ""))
    while True:
        raw = input("选择 %s 编号(0-%d,q 退出): " % (label, len(items) - 1)).strip()
        if raw.lower() in ("q", "quit", "exit"):
            sys.exit(0)
        try:
            idx = int(raw)
            if 0 <= idx < len(items):
                return items[idx]
        except ValueError:
            pass
        print("输入无效,请重新输入")


def main():
    print("=" * 55)
    print(" JumpServer Connection Token 交互式脚本")
    print(" 地址:", JMS_URL)
    print(" 认证: AccessKey HTTP Signature (hmac-sha256)")
    print("=" * 55)

    ak, sk = load_env()
    print("已从 .env 读取凭据")

    # 组织: 可在 .env 配 JMS_ORG(组织 ID),否则交互输入(留空用默认)
    org_id = os.environ.get("JMS_ORG", "").strip() or None

    client = JumpServerClient(JMS_URL, ak, sk, org_id=org_id)

    # 1) 验证认证
    print("\n[2/6] 验证认证中...")
    try:
        me = client.my_profile()
        print("认证成功!用户: %s" % (me.get("name") or me.get("username") or me))
    except Exception as e:
        print("认证失败: %s" % e)
        print("\n排查建议:")
        print(" 1. AccessKey 是否已在 JumpServer 控制台创建并启用")
        print(" 2. Secret 是否复制完整(无多余空格/换行)")
        print(" 3. 系统时间是否正确(签名含 date,时钟偏差大会导致验签失败)")
        sys.exit(1)

    # 1.5) 组织: 从 profile 的 workbench_orgs 选择
    print("\n[3/6] 选择组织...")
    orgs = me.get("workbench_orgs") or []
    if orgs:
        org = choose(orgs, "组织",
                     name=lambda o: "%s%s" % (
                         o.get("name"),
                         " (默认)" if o.get("is_default") else ""))
        client.org_id = org["id"]
        print("使用组织: %s (%s)" % (org.get("name"), org["id"]))
    else:
        raw = ask("未获取到组织列表,输入组织 ID(留空用默认): ", default="")
        if raw:
            client.org_id = raw

    # 2) 资产 or 应用
    print("\n[4/6] 资产(1) or 应用(2)? ")
    kind = ask("类型 [1=asset, 2=application, 默认 1]: ", default="1").strip()
    search = ask("搜索关键字(可留空,回车): ", default="")

    if kind == "2":
        # 应用(Web 界面的"我的应用",如 MySQL 数据库发布)
        data = client.list_my_applications(search=search or None)
        apps = data.get("results", [])
        app = choose(apps, "应用",
                     name=lambda a: "%s [%s/%s]" % (
                         a.get("name"), a.get("category_display"),
                         a.get("type_display")))
        su_data = client.list_app_system_users(app["id"])
        su_list = su_data.get("results", []) if isinstance(su_data, dict) else su_data
        su = choose(su_list, "系统用户")
        print("\n[6/6] 创建连接令牌...")
        token = client.create_connection_token(
            "application", application_id=app["id"], system_user_id=su["id"])
        obj = "应用 %s" % app.get("name")
    else:
        data = client.list_my_assets(search=search or None)
        assets = data.get("results", [])
        if assets and "asset" in assets[0] and "id" not in assets[0]:
            assets = [r["asset"] for r in data.get("results", []) if r.get("asset")]
        asset = choose(assets, "资产",
                       name=lambda a: "%s (%s)" % (a.get("name"), a.get("ip")))

        # 3) 系统用户
        print("\n[5/6] 获取系统用户列表...")
        su_data = client.list_asset_system_users(asset["id"])
        su_list = su_data.get("results", []) if isinstance(su_data, dict) else su_data
        su = choose(su_list, "系统用户")
        print("\n[6/6] 创建连接令牌...")
        token = client.create_connection_token(
            "asset", asset_id=asset["id"], system_user_id=su["id"])
        obj = "资产 %s (%s)" % (asset.get("name"), asset.get("ip"))

    print("\n== Connection Token (%s) ==" % obj)
    print(json.dumps(token, indent=2, ensure_ascii=False, default=str))

    # 可选: client url
    want = ask("\n获取客户端连接 URL? [Y/n]: ", default="y")
    if want.lower() != "n":
        try:
            urls = client.get_client_url(token.get("id"))
            print("== Client URL ==")
            print(json.dumps(urls, indent=2, ensure_ascii=False, default=str))
        except Exception as e:
            print("获取 client-url 失败(可忽略): %s" % e)

    print("\n完成!")


if __name__ == "__main__":
    main()