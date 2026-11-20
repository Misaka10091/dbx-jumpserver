package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const allOrgs = "00000000-0000-0000-0000-000000000000"
const maxBody = 8 << 20

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type object = map[string]any

type jmsClient struct {
	base   *url.URL
	ak, sk string
	http   *http.Client
	now    func() time.Time
}

func newClient(endpoint, ak, sk string) (*jmsClient, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("JumpServer 地址无效，请填写 HTTPS 地址")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return nil, errors.New("JumpServer 必须使用 HTTPS（本机调试除外）")
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/api/v1") {
		path = strings.TrimSuffix(path, "/api/v1")
	} else if strings.HasSuffix(path, "/api") {
		path = strings.TrimSuffix(path, "/api")
	}
	u.Path, u.RawPath = path+"/api/v1", ""
	if strings.TrimSpace(ak) == "" || sk == "" || strings.ContainsAny(ak, "\r\n\"") {
		return nil, errors.New("请填写 AccessKey ID 和 Secret")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 4
	return &jmsClient{base: u, ak: ak, sk: sk, now: time.Now, http: &http.Client{
		Timeout: 25 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func signHeaders(req *http.Request, ak, sk, org string, now time.Time) {
	if org == "" {
		org = allOrgs
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	req.Header.Set("X-JMS-ORG", org)
	target := req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	canonical := "(request-target): " + strings.ToLower(req.Method) + " " + target +
		"\naccept: application/json\ndate: " + req.Header.Get("Date")
	mac := hmac.New(sha256.New, []byte(sk))
	_, _ = mac.Write([]byte(canonical))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", fmt.Sprintf(`Signature keyId="%s",algorithm="hmac-sha256",headers="(request-target) accept date",signature="%s"`, ak, signature))
}

func (c *jmsClient) request(ctx context.Context, method, path, org string, query url.Values, body any) (any, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var data io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("请求参数无法编码")
		}
		data = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), data)
	if err != nil {
		return nil, errors.New("无法创建 JumpServer 请求")
	}
	signHeaders(req, c.ak, c.sk, org, c.now())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 JumpServer，请检查地址、证书、网络和系统时间")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		hint := "请检查请求参数及服务端版本"
		switch resp.StatusCode {
		case 401:
			hint = "请检查 AccessKey、Secret 及本机时间"
		case 403:
			hint = "当前用户无权限，请检查组织及资产授权"
		case 404:
			hint = "资源不存在，请确认资产/应用类型及组织"
		case 429:
			hint = "请求过于频繁，请稍后重试"
		}
		return nil, fmt.Errorf("JumpServer HTTP %d：%s", resp.StatusCode, hint)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return nil, errors.New("JumpServer 响应过大或读取失败")
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, errors.New("JumpServer 返回了非 JSON 数据")
	}
	return result, nil
}

func urlValues(search string) url.Values {
	values := url.Values{}
	if search != "" {
		values.Set("search", search)
	}
	return values
}
func text(v any) string     { s, _ := v.(string); return s }
func asObject(v any) object { m, _ := v.(map[string]any); return m }
func number(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := strconv.Atoi(n.String())
		return i
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
func (c *jmsClient) profile(ctx context.Context, org string) (object, error) {
	v, err := c.request(ctx, "GET", "/users/profile/", org, nil, nil)
	if err != nil {
		return nil, err
	}
	m := asObject(v)
	if m == nil {
		return nil, errors.New("用户信息格式异常")
	}
	return m, nil
}

func (c *jmsClient) list(ctx context.Context, path, org string, query url.Values) ([]object, error) {
	if query == nil {
		query = url.Values{}
	}
	items := []object{}
	seen := map[string]bool{}
	const pageSize = 100
	for page := 0; page < 1000; page++ {
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("offset", strconv.Itoa(page*pageSize))
		v, err := c.request(ctx, "GET", path, org, query, nil)
		if err != nil {
			return nil, err
		}
		rows, plain := v.([]any)
		count := -1
		hasNext := false
		if !plain {
			m := asObject(v)
			if m == nil {
				return nil, errors.New("资源列表格式异常")
			}
			var ok bool
			rows, ok = m["results"].([]any)
			if !ok {
				return nil, errors.New("资源列表缺少 results 数组")
			}
			if m["count"] != nil {
				count = number(m["count"])
			}
			hasNext = text(m["next"]) != ""
		}
		added := 0
		for _, row := range rows {
			m := asObject(row)
			if m == nil {
				return nil, errors.New("资源条目格式异常")
			}
			if nested := asObject(m["asset"]); text(m["id"]) == "" && nested != nil {
				m = nested
			}
			id := text(m["id"])
			if id == "" {
				return nil, errors.New("资源条目缺少 ID")
			}
			if !seen[id] {
				items = append(items, m)
				seen[id] = true
				added++
			}
		}
		if plain || len(rows) == 0 || (count >= 0 && (page*pageSize+len(rows) >= count)) || (!hasNext && count < 0 && len(rows) < pageSize) {
			return items, nil
		}
		if added == 0 {
			return nil, errors.New("分页返回重复数据，已停止；请缩小搜索范围")
		}
	}
	return nil, errors.New("资源数量超出上限，请缩小搜索范围")
}

func resourcePath(kind string) (string, error) {
	switch kind {
	case "application":
		return "/perms/users/applications/", nil
	case "asset":
		return "/perms/users/assets/", nil
	}
	return "", errors.New("资源类型必须为 application 或 asset")
}
func dbType(protocol string) string {
	switch strings.ToLower(protocol) {
	case "mysql", "mariadb":
		return "mysql"
	case "postgresql", "postgres":
		return "postgres"
	case "sqlserver", "mssql":
		return "sqlserver"
	}
	return ""
}
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

type credentials struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Database  string `json:"database"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expiresAt"`
}

type selection struct {
	ConnectionID string `json:"connectionId"`
	OrgID        string `json:"orgId"`
	Kind         string `json:"kind"`
	ResourceID   string `json:"resourceId"`
	AccountID    string `json:"accountId"`
	TargetID     string `json:"targetId"`
	TargetType   string `json:"targetType"`
	Database     string `json:"database"`
}

func (s *session) mint(ctx context.Context, in selection) (*credentials, error) {
	if !safeID.MatchString(in.ResourceID) || !safeID.MatchString(in.AccountID) || !safeID.MatchString(in.OrgID) {
		return nil, errors.New("请先选择组织、资源和系统账号")
	}
	base, err := resourcePath(in.Kind)
	if err != nil {
		return nil, err
	}
	resources, err := s.client.list(ctx, base, in.OrgID, url.Values{"id": {in.ResourceID}})
	if err != nil {
		return nil, err
	}
	var resource object
	for _, r := range resources {
		if text(r["id"]) == in.ResourceID {
			resource = r
			break
		}
	}
	if resource == nil {
		return nil, errors.New("所选组织中找不到该资源，请重新加载")
	}
	accounts, err := s.client.list(ctx, base+in.ResourceID+"/system-users/", in.OrgID, nil)
	if err != nil {
		return nil, err
	}
	var account object
	for _, a := range accounts {
		if text(a["id"]) == in.AccountID {
			account = a
			break
		}
	}
	if account == nil {
		return nil, errors.New("所选系统账号已不可用，请重新选择")
	}
	protocol := text(account["protocol"])
	if protocol == "" {
		protocol = text(resource["type"])
	}
	kind := dbType(protocol)
	if kind == "" {
		return nil, errors.New("自动填入支持 MySQL、MariaDB、PostgreSQL 和 SQL Server")
	}
	if in.TargetID != "" && in.TargetType != kind {
		return nil, errors.New("目标 DBX 连接类型与 JumpServer 数据库协议不匹配")
	}
	host, port, err := s.client.databaseEndpoint(ctx, in.OrgID, in.Kind, in.ResourceID, protocol)
	if err != nil {
		return nil, err
	}
	id, err := randomUUID()
	if err != nil {
		return nil, errors.New("无法生成令牌 ID")
	}
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(secretBytes); err != nil {
		return nil, errors.New("无法生成令牌密钥")
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	expiry := s.client.now().UTC().Add(time.Duration(s.expireMinutes) * time.Minute).Format(time.RFC3339Nano)
	body := object{"id": id, "type": in.Kind, "system_user": in.AccountID, "secret": secret, "date_expired": expiry, in.Kind: in.ResourceID}
	v, err := s.client.request(ctx, "POST", "/authentication/connection-token/", in.OrgID, nil, body)
	if err != nil {
		return nil, err
	}
	token := asObject(v)
	if token == nil || text(token["id"]) != id {
		return nil, errors.New("令牌响应 ID 不一致，未填入 DBX")
	}
	if valid, ok := token["is_valid"].(bool); ok && !valid {
		return nil, errors.New("JumpServer 返回的令牌无效")
	}
	if returned := text(token["secret"]); returned != "" {
		secret = returned
	}
	if returned := text(token["date_expired"]); returned != "" {
		parsed, err := parseTokenExpiry(returned)
		if err != nil {
			return nil, err
		}
		if !parsed.After(s.client.now()) {
			return nil, errors.New("JumpServer 返回的令牌已过期，请检查服务端和本机时间")
		}
		expiry = parsed.UTC().Format(time.RFC3339Nano)
	}
	// Use the exact ConnectionToken response ID for database proxy login.
	username := text(token["id"])
	name := text(resource["name"])
	if name == "" {
		name = text(resource["hostname"])
	}
	return &credentials{Host: host, Port: port, Username: username, Password: secret, Database: in.Database, Type: kind, Name: name, ExpiresAt: expiry}, nil
}

// JumpServer v2 can serialize dates with DATETIME_FORMAT as
// "2006/01/02 15:04:05 +0800", rather than RFC 3339. Require an explicit zone
// instead of guessing the server's timezone from the plugin's local timezone.
func parseTokenExpiry(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006/01/02 15:04:05 -0700",
		"2006/01/02 15:04:05 -07:00",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05Z07:00",
	} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("JumpServer 返回的令牌时间格式不受支持，需要包含时区")
}

func validateDiscoveredEndpoint(host string, port int) error {
	if host == "" || strings.ContainsAny(host, "/\\@?#\r\n\t ") || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return errors.New("JumpServer smart 接口未返回有效的数据库代理主机，未填入连接")
	}
	if port < 1 || port > 65535 {
		return errors.New("JumpServer smart 接口未返回有效的 magnus_listen_db_port，未填入连接")
	}
	return nil
}

func (c *jmsClient) databaseEndpoint(ctx context.Context, org, kind, resourceID, protocol string) (string, int, error) {
	if !safeID.MatchString(resourceID) || dbType(protocol) == "" {
		return "", 0, errors.New("数据库资源或协议无效")
	}
	query := url.Values{"protocol": {protocol}}
	switch kind {
	case "application":
		query.Set("app_id", resourceID)
	case "asset":
		query.Set("asset_id", resourceID)
	default:
		return "", 0, errors.New("资源类型必须为 application 或 asset")
	}
	value, err := c.request(ctx, "GET", "/terminal/endpoints/smart/", org, query, nil)
	if err != nil {
		return "", 0, err
	}
	endpoint := asObject(value)
	host := strings.TrimSpace(text(endpoint["host"]))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	port := number(endpoint["magnus_listen_db_port"])
	if err := validateDiscoveredEndpoint(host, port); err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func newConnectionLink(c credentials) (string, error) {
	if err := validateDiscoveredEndpoint(c.Host, c.Port); err != nil {
		return "", err
	}
	q := url.Values{
		"v":        {"1"},
		"user":     {c.Username},
		"password": {c.Password},
		"type":     {c.Type},
		"host":     {c.Host},
		"port":     {strconv.Itoa(c.Port)},
		"name":     {c.Name + " · JumpServer"},
	}
	if c.Database != "" {
		q.Set("database", c.Database)
	}
	for _, values := range q {
		if len(values[0]) > 4096 {
			return "", errors.New("连接参数过长")
		}
	}
	link := "dbx://connection/new?" + q.Encode()
	if len(link) > 16384 {
		return "", errors.New("连接链接过长")
	}
	return link, nil
}
