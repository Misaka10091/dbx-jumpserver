package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type terminal struct {
	target  sshSelection
	id      string
	socket  terminalSocket
	output  chan []byte
	done    chan struct{}
	visited chan struct{}
	stop    context.CancelFunc
}

func startTerminal(id string, target sshSelection, socket terminalSocket) *terminal {
	ctx, stop := context.WithCancel(context.Background())
	t := &terminal{id: id, target: target, socket: socket, output: make(chan []byte, 8), done: make(chan struct{}), visited: make(chan struct{}, 1), stop: stop}
	go func() {
		defer socket.close()
		idle := time.NewTimer(45 * time.Second)
		defer idle.Stop()
		heartbeat := time.NewTicker(25 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-idle.C:
				return
			case <-t.visited:
				idle.Reset(45 * time.Second)
			case <-heartbeat.C:
				if socket.send("PING", "") != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer close(t.done)
		defer t.stop()
		for {
			data, _, err := socket.read()
			if len(data) != 0 {
				select {
				case t.output <- data:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return t
}

func (t *terminal) touch() {
	select {
	case t.visited <- struct{}{}:
	default:
	}
}

func isSSHMethod(method string) bool {
	return strings.HasPrefix(method, "jumpserver/ssh/")
}

func (p *plugin) handleSSH(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		sshSelection
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"`
		Cols       int    `json:"cols"`
		Rows       int    `json:"rows"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil, errors.New("SSH 请求参数无效")
	}
	p.mu.RLock()
	s := p.sessions[in.Connection]
	t := p.terminals[in.Connection]
	p.mu.RUnlock()
	if s == nil {
		return nil, errors.New("凭证服务未连接，请先在 DBX 连接树中连接 JumpServer")
	}
	switch method {
	case "jumpserver/ssh/assets":
		items, err := s.client.sshAssets(ctx, in.sshSelection)
		return object{"items": items}, err
	case "jumpserver/ssh/accounts":
		items, _, err := s.client.sshAccounts(ctx, in.sshSelection)
		return object{"items": items}, err
	case "jumpserver/ssh/open":
		if in.Cols < 10 || in.Cols > 500 || in.Rows < 3 || in.Rows > 250 {
			return nil, errors.New("终端尺寸超出范围")
		}
		id, err := randomUUID()
		if err != nil {
			return nil, errors.New("无法生成终端会话 ID")
		}
		socket, err := s.openTerminal(ctx, in.sshSelection, in.Cols, in.Rows, p.prompt)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.sessions[in.Connection] != s || p.terminals[in.Connection] != t {
			socket.close()
			return nil, errors.New("连接或终端已切换，请重新打开")
		}
		if t != nil {
			t.stop()
		}
		p.terminals[in.Connection] = startTerminal(id, in.sshSelection, socket)
		return object{"terminalId": id}, nil
	}
	if t == nil || in.TerminalID != t.id {
		return nil, errors.New("SSH 终端已断开，请重新打开")
	}
	t.touch()
	switch method {
	case "jumpserver/ssh/read":
		timer := time.NewTimer(650 * time.Millisecond)
		defer timer.Stop()
		select {
		case data := <-t.output:
			return object{"data": data}, nil
		case <-timer.C:
			return object{}, nil
		case <-t.done:
			select {
			case data := <-t.output:
				return object{"data": data}, nil
			default:
				return object{"closed": true}, nil
			}
		}
	case "jumpserver/ssh/write":
		if len(in.Data) > 8192 || strings.ContainsRune(in.Data, 0) {
			return nil, errors.New("终端输入过长或包含无效字符")
		}
		return object{}, t.socket.send("TERMINAL_DATA", in.Data)
	case "jumpserver/ssh/resize":
		return object{}, t.socket.resize("TERMINAL_RESIZE", in.Cols, in.Rows)
	case "jumpserver/ssh/close":
		p.mu.Lock()
		if p.terminals[in.Connection] == t {
			delete(p.terminals, in.Connection)
		}
		p.mu.Unlock()
		t.stop()
		return object{}, nil
	default:
		return nil, errMethod
	}
}

var sshChecks = map[string]string{
	"hostname": "hostname",
	"whoami":   "whoami",
	"uptime":   "uptime",
	"kernel":   "uname -sr",
	"disk":     "df -h",
}

func sshTools() []object {
	tool := func(name, description string, properties object, required ...string) object {
		if required == nil {
			required = []string{}
		}
		return object{
			"name": name, "description": description,
			"inputSchema": object{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			"annotations": object{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": true},
		}
	}
	str := func(description string) object { return object{"type": "string", "description": description} }
	tools := []object{
		tool("ssh_list_organizations", "List JumpServer organizations available to the saved DBX connection.",
			object{}),
		tool("ssh_list_assets", "List authorized SSH assets within one JumpServer organization.",
			object{"orgId": str("Organization ID"), "search": str("Optional asset name or address")}, "orgId"),
		tool("ssh_list_accounts", "List the authorized SSH accounts for a JumpServer asset.",
			object{"orgId": str("Organization ID"), "assetId": str("SSH asset ID")}, "orgId", "assetId"),
		tool("ssh_inspect", "Open a temporary audited JumpServer KoKo session and run one fixed read-only SSH inspection. Only use an explicitly selected asset and account.",
			object{"orgId": str("Organization ID"), "assetId": str("SSH asset ID"), "accountId": str("Authorized account ID"),
				"check": object{"type": "string", "enum": []string{"hostname", "whoami", "uptime", "kernel", "disk"}}},
			"orgId", "assetId", "accountId", "check"),
	}
	return append(tools, operationTools()...)
}

func mcpResult(value any, err error) (any, error) {
	if err != nil {
		return object{"content": []object{{"type": "text", "text": err.Error()}}, "isError": true}, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("无法编码 JumpServer 工具结果")
	}
	return object{"content": []object{{"type": "text", "text": string(data)}}, "isError": false}, nil
}

func (p *plugin) callMCP(ctx context.Context, raw json.RawMessage) (any, error) {
	var call struct {
		Tool      string          `json:"tool"`
		Lifecycle json.RawMessage `json:"lifecycle"`
		Arguments operationsArgs  `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return mcpResult(nil, errors.New("MCP 参数无效"))
	}
	// Always use the lifecycle selected and authorized by DBX. Model arguments
	// cannot supply credentials, override the connection or choose the gateway.
	id, s, err := parseSession(call.Lifecycle)
	if err != nil {
		return mcpResult(nil, err)
	}
	if operation, advanced := operations[call.Tool]; advanced {
		if !s.mcpOperations {
			return mcpResult(nil, errors.New("此 JumpServer 连接未开启 SSH 和 SFTP 的 MCP 工具"))
		}
		if s.readOnly && operation.access != "read" {
			return mcpResult(nil, errors.New("此 DBX 连接启用了只读保护"))
		}
	} else if !s.mcpRead && !s.mcpOperations {
		return mcpResult(nil, errors.New("此 JumpServer 连接未开启 MCP 只读巡检"))
	}
	defer s.closeIdle()
	p.mu.RLock()
	connected := p.sessions[id]
	job := p.jobs[call.Arguments.JobID]
	p.mu.RUnlock()
	useSession := toolNeedsSession(call.Tool)
	needDesktop := false
	switch call.Tool {
	case "ssh_terminal_input", "ssh_close":
		needDesktop = connected == nil
	case "ssh_task_status":
		needDesktop = job == nil
	default:
		needDesktop = useSession && s.terminalWeb != nil && !connected.matches(s)
	}
	if needDesktop && !call.Arguments.DesktopRelay {
		if port := desktopBridgePort(); port != 0 {
			result, err := callDesktopTool(ctx, port, id, call.Tool, call.Arguments)
			if err != nil {
				return mcpResult(nil, err)
			}
			return result, nil
		}
	}
	if useSession && s.terminalWeb != nil {
		if connected.matches(s) {
			s = connected
		} else if err := s.authenticate(ctx, nil); err != nil {
			return mcpResult(nil, err)
		}
	}
	switch call.Tool {
	case "ssh_list_organizations":
		items, err := s.organizations(ctx)
		return mcpResult(object{"items": items}, err)
	case "ssh_list_assets":
		items, err := s.client.sshAssets(ctx, call.Arguments.sshSelection)
		return mcpResult(object{"items": items}, err)
	case "ssh_list_accounts":
		items, _, err := s.client.sshAccounts(ctx, call.Arguments.sshSelection)
		return mcpResult(object{"items": items}, err)
	case "ssh_inspect":
		command, ok := sshChecks[call.Arguments.Check]
		if !ok {
			return mcpResult(nil, errors.New("未知巡检项"))
		}
		if s.terminalWeb == nil {
			result, err := s.inspectNative(ctx, call.Arguments.sshSelection, command)
			return mcpResult(result, err)
		}
		result, err := s.terminalWeb.inspectSSH(ctx, call.Arguments.sshSelection, command)
		return mcpResult(result, err)
	default:
		result, err := p.runOperation(ctx, id, s, call.Tool, call.Arguments)
		return mcpResult(result, err)
	}
}

func (s *session) matches(other *session) bool {
	return s != nil && s.client.base.String() == other.client.base.String() &&
		s.loginUsername == other.loginUsername && (s.terminalWeb == s.client) == (other.terminalWeb == other.client) &&
		s.client.ak == other.client.ak && s.client.sk == other.client.sk
}

var terminalControl = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[()][A-Za-z0-9]|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

func (c *jmsClient) inspectSSH(ctx context.Context, in sshSelection, command string) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	result, err := c.execWeb(ctx, in, command)
	if err == nil && result["output"] == "" {
		return nil, errors.New("巡检命令没有返回输出，请检查 JumpServer 命令审计或资产 shell")
	}
	return result, err
}

func (c *jmsClient) execWeb(ctx context.Context, in sshSelection, command string) (object, error) {
	socket, err := c.openKoko(ctx, in, 240, 24)
	if err != nil {
		return nil, err
	}
	defer socket.conn.Close()
	deadline, _ := ctx.Deadline()
	_ = socket.conn.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { socket.conn.Close() })
	defer stop()
	tag, err := randomUUID()
	if err != nil {
		return nil, errors.New("无法生成巡检标记")
	}
	start, end := "DBX_BEGIN_"+tag, "DBX_END_"+tag
	// Split each tag in the request so a shell's input echo cannot match it.
	format := func(tag string, status bool) string {
		arg := fmt.Sprintf("'%s' '%s'", tag[:len(tag)/2], tag[len(tag)/2:])
		if status {
			return "printf '\\n%s%s:%s\\n' " + arg + " \"$?\""
		}
		return "printf '\\n%s%s\\n' " + arg
	}
	footer := format(end, true)
	var output bytes.Buffer
	var once sync.Once
	running := false
	for {
		data, event, err := socket.read()
		if err != nil {
			return nil, errors.New("KoKo 在巡检完成前断开或超时，请在工作台确认目标 shell 可用及命令权限")
		}
		if event == "TERMINAL_SESSION" {
			var sendErr error
			once.Do(func() { sendErr = socket.send("TERMINAL_DATA", format(start, false)+"\r") })
			if sendErr != nil {
				return nil, sendErr
			}
		}
		if len(data) == 0 {
			continue
		}
		output.Write(data)
		if output.Len() > 512<<10 {
			return nil, errors.New("巡检输出超过 512 KiB")
		}
		clean := terminalControl.ReplaceAllString(output.String(), "")
		clean = strings.ReplaceAll(clean, "\r", "")
		if !running {
			if !strings.Contains(clean, start+"\n") {
				continue
			}
			output.Reset()
			running = true
			if err := socket.send("TERMINAL_DATA", command+"\r"); err != nil {
				return nil, err
			}
			if err := socket.send("TERMINAL_DATA", footer+"\r"); err != nil {
				return nil, err
			}
			continue
		}
		before, after, found := strings.Cut(clean, end+":")
		if !found {
			continue
		}
		statusText, _, complete := strings.Cut(after, "\n")
		if !complete {
			continue
		}
		status, err := strconv.Atoi(statusText)
		if err != nil || status < 0 || status > 255 {
			return nil, errors.New("无法读取巡检命令退出状态")
		}
		lines := strings.Split(before, "\n")
		for i, line := range lines {
			if strings.HasSuffix(line, command) || strings.Contains(line, footer) {
				lines[i] = ""
			}
		}
		result := strings.TrimSpace(strings.Join(lines, "\n"))
		return object{"command": command, "output": result, "exitCode": status}, nil
	}
}
