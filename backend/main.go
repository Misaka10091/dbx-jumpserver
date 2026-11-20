package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const pluginID = "io.github.misaka10091.jumpserver"
const version = "0.2.11"

type session struct {
	client         *jmsClient
	terminalWeb    *jmsClient
	username       string
	loginUsername  string
	loginPassword  string
	sshHost        string
	sshPort        int
	sshFingerprint string
	expireMinutes  int
	orgID          string
	mcpRead        bool
	mcpOperations  bool
	readOnly       bool
}

type plugin struct {
	mu           sync.RWMutex
	sessions     map[string]*session
	open         func(string) error
	refreshing   map[string]bool
	terminals    map[string]*terminal
	prompt       userPrompt
	promptImages bool
	jobs         map[string]*sshJob
	transfers    map[string]*workbenchTransfer
}

func newPlugin() *plugin {
	return &plugin{sessions: map[string]*session{}, refreshing: map[string]bool{}, open: openDBX, terminals: map[string]*terminal{}, jobs: map[string]*sshJob{}, transfers: map[string]*workbenchTransfer{}}
}

type lifecycle struct {
	Connection struct {
		ID       string            `json:"id"`
		Host     string            `json:"host"`
		Username string            `json:"username"`
		ReadOnly bool              `json:"read_only"`
		Config   object            `json:"external_config"`
		Secrets  map[string]string `json:"connection_secrets"`
	} `json:"connection"`
	Runtime struct {
		Host  string `json:"host"`
		Port  int    `json:"port"`
		Proxy any    `json:"proxy"`
	} `json:"runtime"`
}

func parseSession(raw json.RawMessage) (string, *session, error) {
	var in lifecycle
	if err := json.Unmarshal(raw, &in); err != nil || in.Connection.ID == "" {
		return "", nil, errors.New("缺少 JumpServer 凭证服务连接")
	}
	// The API endpoint is an HTTPS URL, not a DBX TCP tunnel target.
	if in.Runtime.Proxy != nil || (in.Runtime.Host != "" && in.Runtime.Host != in.Connection.Host) {
		return "", nil, errors.New("JumpServer API 凭证服务暂不支持 DBX 隧道；请使用可直连的 HTTPS 地址")
	}
	c := in.Connection
	var client *jmsClient
	var err error
	auth := text(c.Config["auth_method"])
	if auth == "password" {
		client, err = newClientWithAuth(c.Host, "", "")
	} else if auth == "" || auth == "access_key" {
		client, err = newClient(c.Host, c.Secrets["access_key"], c.Secrets["secret_key"])
	} else {
		return "", nil, errors.New("不支持的 JumpServer 认证方式")
	}
	if err != nil {
		return "", nil, err
	}
	minutes := number(c.Config["expire_minutes"])
	if minutes == 0 {
		minutes = 60
	}
	if minutes < 1 || minutes > 1440 {
		return "", nil, errors.New("令牌有效期必须在 1–1440 分钟之间")
	}
	// Ignore legacy username_template values saved by older plugin versions.
	// Database proxy credentials always use the raw ConnectionToken ID.
	mcpRead, _ := c.Config["mcp_read"].(bool)
	mcpOperations, _ := c.Config["mcp_operations"].(bool)
	s := &session{client: client, expireMinutes: minutes, orgID: text(c.Config["org_id"]), mcpRead: mcpRead, mcpOperations: mcpOperations, readOnly: c.ReadOnly}
	s.sshHost = strings.TrimSpace(text(c.Config["ssh_gateway_host"]))
	s.sshPort = number(c.Config["ssh_gateway_port"])
	s.sshFingerprint = strings.TrimSpace(text(c.Config["ssh_host_fingerprint"]))
	if auth == "password" {
		s.terminalWeb = s.client
		s.loginUsername = c.Username
		s.loginPassword = c.Secrets["login_password"]
	} else if enabled, _ := c.Config["ak_web_terminal"].(bool); enabled {
		s.terminalWeb, err = newClientWithAuth(c.Host, "", "")
		if err != nil {
			return "", nil, err
		}
		s.loginUsername = text(c.Config["ak_web_username"])
		s.loginPassword = c.Secrets["ak_web_password"]
	}
	if s.terminalWeb != nil && (strings.TrimSpace(s.loginUsername) == "" || s.loginPassword == "") {
		return "", nil, errors.New("请填写 JumpServer 网页终端所需的登录账号和密码")
	}
	return c.ID, s, nil
}

func (s *session) authenticate(ctx context.Context, prompt userPrompt) error {
	if s.terminalWeb != nil {
		if s.terminalWeb != s.client {
			signedProfile, err := s.client.profile(ctx, s.orgID)
			if err != nil {
				return err
			}
			if !strings.EqualFold(text(signedProfile["username"]), s.loginUsername) {
				return errors.New("AccessKey 与网页终端必须使用同一个 JumpServer 账号")
			}
		}
		err := s.terminalWeb.webLogin(ctx, s.loginUsername, s.loginPassword, prompt)
		s.loginPassword = ""
		if err != nil {
			return err
		}
	}
	profile, err := s.client.profile(ctx, s.orgID)
	if err != nil {
		return err
	}
	s.username = text(profile["username"])
	if s.terminalWeb != nil {
		webProfile := profile
		if s.terminalWeb != s.client {
			webProfile, err = s.terminalWeb.profile(ctx, s.orgID)
			if err != nil {
				return err
			}
		}
		if !strings.EqualFold(text(webProfile["username"]), s.loginUsername) {
			return errors.New("JumpServer 未建立所选账号的登录会话")
		}
	}
	return nil
}

func (s *session) closeIdle() {
	s.client.http.CloseIdleConnections()
	if s.terminalWeb != nil && s.terminalWeb != s.client {
		s.terminalWeb.http.CloseIdleConnections()
	}
}

func (p *plugin) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "mcp/tools":
		return object{"tools": sshTools()}, nil
	case "mcp/call":
		return p.callMCP(ctx, raw)
	case "connection/test", "connection/connect":
		id, s, err := parseSession(raw)
		if err != nil {
			return nil, err
		}
		if s.terminalWeb != nil {
			s.terminalWeb.inlineCaptcha = p.promptImages
		}
		if err := s.authenticate(ctx, p.prompt); err != nil {
			s.closeIdle()
			return nil, err
		}
		if method == "connection/connect" {
			p.mu.Lock()
			old := p.sessions[id]
			p.closeConnectionTransfers(id)
			p.sessions[id] = s
			terminal := p.terminals[id]
			delete(p.terminals, id)
			p.mu.Unlock()
			if terminal != nil {
				terminal.stop()
			}
			if old != nil {
				old.closeIdle()
			}
		} else {
			s.closeIdle()
		}
		return object{"success": true, "message": "JumpServer 认证成功"}, nil
	case "connection/disconnect":
		var in lifecycle
		if json.Unmarshal(raw, &in) != nil || in.Connection.ID == "" {
			return nil, errors.New("缺少连接 ID")
		}
		p.mu.Lock()
		s := p.sessions[in.Connection.ID]
		delete(p.sessions, in.Connection.ID)
		terminal := p.terminals[in.Connection.ID]
		delete(p.terminals, in.Connection.ID)
		p.mu.Unlock()
		if terminal != nil {
			terminal.stop()
		}
		if s != nil {
			s.closeIdle()
		}
		return object{"success": true}, nil
	}
	if strings.HasPrefix(method, "jumpserver/ssh/files/") {
		return p.handleWorkbenchFiles(ctx, method, raw)
	}
	if isSSHMethod(method) {
		return p.handleSSH(ctx, method, raw)
	}
	var in struct {
		selection
		Search string `json:"search"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("请求参数无效")
	}
	p.mu.RLock()
	s := p.sessions[in.ConnectionID]
	p.mu.RUnlock()
	if s == nil {
		return nil, errors.New("凭证服务未连接，请先在 DBX 连接树中连接 JumpServer")
	}
	switch method {
	case "jumpserver/profile":
		orgs, err := s.organizations(ctx)
		if err != nil {
			return nil, err
		}
		return object{"username": s.username, "organizations": orgs, "expireMinutes": s.expireMinutes}, nil
	case "jumpserver/resources", "jumpserver/accounts":
		if !safeID.MatchString(in.OrgID) {
			return nil, errors.New("请先选择实际组织")
		}
		path, err := resourcePath(in.Kind)
		if err != nil {
			return nil, err
		}
		if len(in.Search) > 256 {
			return nil, errors.New("搜索关键词过长")
		}
		query := urlValues(in.Search)
		if method == "jumpserver/accounts" {
			if !safeID.MatchString(in.ResourceID) {
				return nil, errors.New("请先选择数据库资源")
			}
			path += in.ResourceID + "/system-users/"
		}
		rows, err := s.client.list(ctx, path, in.OrgID, query)
		if err != nil {
			return nil, err
		}
		items := []object{}
		for _, r := range rows {
			protocol := text(r["protocol"])
			if protocol == "" {
				protocol = text(r["type"])
			}
			if method == "jumpserver/accounts" && dbType(protocol) == "" {
				continue
			}
			if method == "jumpserver/resources" && in.Kind == "application" && dbType(protocol) == "" {
				continue
			}
			name := text(r["name"])
			if name == "" {
				name = text(r["hostname"])
			}
			if name == "" {
				name = text(r["username"])
			}
			items = append(items, object{"id": text(r["id"]), "name": name, "protocol": protocol, "username": text(r["username"]), "database": text(asObject(r["attrs"])["database"])})
		}
		return object{"items": items}, nil
	case "jumpserver/fill", "jumpserver/credentials":
		p.mu.Lock()
		if p.refreshing[in.ConnectionID] {
			p.mu.Unlock()
			return nil, errors.New("正在刷新凭证，请等待当前操作完成")
		}
		p.refreshing[in.ConnectionID] = true
		p.mu.Unlock()
		defer func() { p.mu.Lock(); delete(p.refreshing, in.ConnectionID); p.mu.Unlock() }()
		c, err := s.mint(ctx, in.selection)
		if err != nil {
			return nil, err
		}
		if method == "jumpserver/credentials" {
			return c, nil
		}
		link, err := connectionLink(*c, in.selection)
		if err != nil {
			return nil, err
		}
		if err = p.open(link); err != nil {
			return nil, errors.New("令牌已创建，但未能打开 DBX；请检查桌面版是否注册 dbx:// 协议，或使用“获取参数”手动填入")
		}
		return object{"opened": true, "expiresAt": c.ExpiresAt, "message": "已请求打开 DBX 连接表单，请在 DBX 中检查并保存"}, nil
	default:
		return nil, errMethod
	}
}

var errMethod = errors.New("不支持的方法")

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func serve(input io.Reader, output io.Writer, p *plugin) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxBody)
	var writeMu sync.Mutex
	write := func(value any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = json.NewEncoder(output).Encode(value)
	}
	prompter := newHostPrompter(write)
	var workers sync.WaitGroup
	slots := make(chan struct{}, 8)
	respond := func(id json.RawMessage, result any, err error, code int) {
		message := object{"jsonrpc": "2.0", "id": id}
		if err != nil {
			message["error"] = object{"code": code, "message": err.Error()}
		} else {
			message["result"] = result
		}
		write(message)
	}
	initialized := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if prompter.accept(line) {
			continue
		}
		var req rpcRequest
		if json.Unmarshal(line, &req) != nil {
			respond(nil, nil, errors.New("无效 JSON"), -32700)
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			respond(req.ID, nil, errors.New("无效 JSON-RPC 请求"), -32600)
			continue
		}
		if req.Method == "plugin/initialize" {
			if initialized {
				respond(req.ID, nil, errors.New("插件已初始化"), -32001)
				continue
			}
			var params struct {
				Host struct {
					Versions []int    `json:"protocolVersions"`
					Features []string `json:"features"`
				} `json:"host"`
			}
			_ = json.Unmarshal(req.Params, &params)
			compatible := false
			for _, v := range params.Host.Versions {
				if v == 1 {
					compatible = true
				}
			}
			if !compatible {
				respond(req.ID, nil, errors.New("DBX 未提供协议 v1"), -32001)
				continue
			}
			initialized = true
			for _, feature := range params.Host.Features {
				if feature == "host.requestUserInput" {
					p.prompt = prompter.prompt
				}
				if feature == "host.requestUserInput.image" {
					p.promptImages = true
				}
			}
			respond(req.ID, object{"protocolVersion": 1, "capabilities": []string{"connections", "mcp"}, "plugin": object{"id": pluginID, "version": version}}, nil, 0)
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		if !initialized {
			respond(req.ID, nil, errors.New("插件尚未初始化"), -32001)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			respond(req.ID, nil, errors.New("JumpServer 请求过多，请稍后重试"), -32000)
			continue
		}
		workers.Add(1)
		go func(req rpcRequest) {
			defer workers.Done()
			defer func() { <-slots }()
			defer func() {
				if recover() != nil {
					respond(req.ID, nil, errors.New("插件内部错误"), -32603)
				}
			}()
			timeout := 110 * time.Second
			if req.Method == "jumpserver/ssh/files/prepare-download" {
				timeout = 250 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			result, err := p.handle(ctx, req.Method, req.Params)
			code := -32000
			if errors.Is(err, errMethod) {
				code = -32601
			}
			respond(req.ID, result, err, code)
		}(req)
	}
	workers.Wait()
	for id := range p.transfers {
		p.removeTransfer(id)
	}
	return scanner.Err()
}
func main() {
	enterNativeCaptchaMode()
	if err := serve(os.Stdin, os.Stdout, newPlugin()); err != nil {
		fmt.Fprintln(os.Stderr, "JumpServer 插件协议流已停止")
		os.Exit(1)
	}
}
