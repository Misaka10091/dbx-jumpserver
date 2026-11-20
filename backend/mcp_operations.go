package main

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
)

type operationsArgs struct {
	sshSelection
	Check         string         `json:"check"`
	Command       string         `json:"command"`
	Input         string         `json:"input"`
	Enter         bool           `json:"appendNewline"`
	Timeout       int            `json:"timeoutSecs"`
	Targets       []sshSelection `json:"targets"`
	JobID         string         `json:"taskId"`
	Fingerprint   string         `json:"fingerprint"`
	RemotePath    string         `json:"remotePath"`
	LocalPath     string         `json:"localPath"`
	Content       string         `json:"content"`
	Encoding      string         `json:"encoding"`
	Overwrite     bool           `json:"overwrite"`
	DesktopRelay  bool           `json:"_dbxDesktopRelay,omitempty"`
	deleteEntries []fileDeleteEntry
}

type operationSpec struct {
	access, description string
	targetRequired      bool
	fields              []string
	required            []string
}

var operations = map[string]operationSpec{
	"ssh_exec":              {"full", "Execute one audited shell command on a selected asset.", true, []string{"command", "timeoutSecs"}, []string{"command"}},
	"ssh_exec_sudo":         {"full", "Run one program through non-interactive sudo on the selected asset.", true, []string{"command", "timeoutSecs"}, []string{"command"}},
	"ssh_multi_exec":        {"full", "Run one audited shell command on up to ten selected JumpServer assets.", false, []string{"command", "targets", "timeoutSecs"}, []string{"command", "targets"}},
	"ssh_terminal_input":    {"full", "Send input to the currently open DBX terminal for the selected asset and account.", true, []string{"input", "appendNewline"}, []string{"input"}},
	"ssh_run_bg":            {"full", "Start an audited job in a temporary JumpServer session managed by the desktop plugin process.", true, []string{"command", "timeoutSecs"}, []string{"command"}},
	"ssh_task_status":       {"read", "Read a job state and captured output for this saved connection.", false, []string{"taskId"}, []string{"taskId"}},
	"ssh_close":             {"write", "Close the current interactive terminal for the selected asset and account.", true, nil, nil},
	"ssh_list_known_hosts":  {"read", "Show the saved native SSH gateway fingerprint for this asset.", true, nil, nil},
	"ssh_list_connections":  {"read", "Show metadata for the DBX connection authorized for this call.", false, nil, nil},
	"ssh_remove_known_host": {"write", "Remove the locally saved native SSH fingerprint after checking its exact value.", true, []string{"fingerprint"}, []string{"fingerprint"}},
	"sftp_list_dir":         {"read", "List files in an authorized SFTP directory.", true, []string{"remotePath"}, []string{"remotePath"}},
	"sftp_stat":             {"read", "Read metadata for an authorized SFTP path.", true, []string{"remotePath"}, []string{"remotePath"}},
	"sftp_read_file":        {"read", "Read a small authorized SFTP file. Binary content can be returned as base64.", true, []string{"remotePath", "encoding"}, []string{"remotePath"}},
	"sftp_upload":           {"write", "Upload a file from the DBX plugin transfers folder to the selected asset.", true, []string{"remotePath", "localPath", "overwrite"}, []string{"remotePath", "localPath"}},
	"sftp_download":         {"write", "Download an authorized remote file to the DBX plugin transfers folder.", true, []string{"remotePath", "localPath"}, []string{"remotePath", "localPath"}},
	"sftp_write_file":       {"write", "Write bounded text or base64 content to an authorized SFTP path.", true, []string{"remotePath", "content", "encoding", "overwrite"}, []string{"remotePath", "content"}},
}

func operationTools() []object {
	targetFields := object{
		"orgId": object{"type": "string"}, "assetId": object{"type": "string"}, "accountId": object{"type": "string"},
	}
	targetRequired := []string{"orgId", "assetId", "accountId"}
	fields := object{
		"command":       object{"type": "string", "maxLength": 4096},
		"input":         object{"type": "string", "maxLength": 8192},
		"appendNewline": object{"type": "boolean"},
		"timeoutSecs":   object{"type": "integer", "minimum": 1, "maximum": 120},
		"targets": object{"type": "array", "minItems": 1, "maxItems": 10, "items": object{
			"type": "object", "properties": targetFields, "required": targetRequired, "additionalProperties": false,
		}},
		"taskId":      object{"type": "string"},
		"fingerprint": object{"type": "string"},
		"remotePath":  object{"type": "string", "description": "Absolute path within the authorized SFTP root"},
		"localPath":   object{"type": "string", "description": "File name within the DBX plugin transfers folder"},
		"content":     object{"type": "string"},
		"encoding":    object{"type": "string", "enum": []string{"utf8", "base64"}},
		"overwrite":   object{"type": "boolean"},
	}
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]object, 0, len(names))
	for _, name := range names {
		spec := operations[name]
		properties, required := object{}, []string{}
		if spec.targetRequired {
			for key, field := range targetFields {
				properties[key] = field
			}
			required = append(required, targetRequired...)
		}
		for _, key := range spec.fields {
			properties[key] = fields[key]
		}
		required = append(required, spec.required...)
		tools = append(tools, object{
			"name": name, "description": spec.description,
			"inputSchema": object{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			"annotations": object{"readOnlyHint": spec.access == "read", "destructiveHint": spec.access == "full", "openWorldHint": true},
		})
	}
	return tools
}

func toolNeedsSession(tool string) bool {
	switch tool {
	case "ssh_list_connections", "ssh_task_status", "ssh_close", "ssh_terminal_input":
		return false
	default:
		return true
	}
}

func sameTarget(a, b sshSelection) bool {
	return a.OrgID == b.OrgID && a.AssetID == b.AssetID && a.AccountID == b.AccountID
}

func validTarget(in sshSelection) bool {
	return validOrg(in.OrgID) && safeID.MatchString(in.AssetID) && in.AccountID != "" && len(in.AccountID) <= 256
}

func (s *session) exec(ctx context.Context, in sshSelection, command string, seconds int) (object, error) {
	if !validTarget(in) {
		return nil, errors.New("请选择授权的组织、资产和账号")
	}
	if seconds == 0 {
		seconds = 25
	}
	if seconds < 1 || seconds > 120 {
		return nil, errors.New("任务超时必须在 1–120 秒之间")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	if s.terminalWeb != nil {
		return s.terminalWeb.execWeb(ctx, in, command)
	}
	return s.execNative(ctx, in, command)
}

func checkedCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 4096 || strings.ContainsFunc(command, unicode.IsControl) {
		return "", errors.New("请输入不超过 4096 字节的单行命令")
	}
	return command, nil
}

func (p *plugin) runOperation(ctx context.Context, id string, s *session, tool string, args operationsArgs) (any, error) {
	spec, known := operations[tool]
	if !known {
		return nil, errors.New("未知 JumpServer MCP 工具")
	}
	if spec.targetRequired && !validTarget(args.sshSelection) {
		return nil, errors.New("请选择授权的组织、资产和账号")
	}
	switch tool {
	case "ssh_exec", "ssh_exec_sudo", "ssh_multi_exec", "ssh_run_bg":
		command, err := checkedCommand(args.Command)
		if err != nil {
			return nil, err
		}
		switch tool {
		case "ssh_exec_sudo":
			if strings.ContainsAny(command, ";&|<>`$") {
				return nil, errors.New("非交互式 sudo 只接受单个程序和参数")
			}
			command = "sudo -n -- " + command
		case "ssh_multi_exec":
			if len(args.Targets) < 1 || len(args.Targets) > 10 {
				return nil, errors.New("请选择 1–10 个目标")
			}
			for _, target := range args.Targets {
				if !validTarget(target) {
					return nil, errors.New("每个目标都必须提供组织、资产和授权账号")
				}
			}
			rows := []object{}
			for _, target := range args.Targets {
				item := object{"assetId": target.AssetID, "accountId": target.AccountID}
				result, err := s.exec(ctx, target, command, args.Timeout)
				if err != nil {
					item["error"] = err.Error()
				} else {
					item["result"] = result
				}
				rows = append(rows, item)
			}
			return object{"items": rows}, nil
		case "ssh_run_bg":
			return p.startSSHJob(id, s, args, command)
		}
		return s.exec(ctx, args.sshSelection, command, args.Timeout)
	case "ssh_task_status":
		p.mu.RLock()
		defer p.mu.RUnlock()
		job := p.jobs[args.JobID]
		if job == nil || job.owner != id {
			return nil, errors.New("此连接下没有该后台任务")
		}
		return object{"taskId": args.JobID, "state": job.state, "result": job.result, "error": job.failure}, nil
	case "ssh_terminal_input", "ssh_close":
		p.mu.Lock()
		terminal := p.terminals[id]
		if terminal == nil || !sameTarget(args.sshSelection, terminal.target) {
			p.mu.Unlock()
			return nil, errors.New("请先在 DBX 工作台为同一资产和账号打开终端")
		}
		if tool == "ssh_close" {
			delete(p.terminals, id)
			p.mu.Unlock()
			terminal.stop()
			return object{"closed": true}, nil
		}
		p.mu.Unlock()
		if args.Input == "" || len(args.Input) > 8192 || strings.ContainsRune(args.Input, 0) {
			return nil, errors.New("终端输入为空或超出范围")
		}
		input := strings.ReplaceAll(strings.ReplaceAll(args.Input, "\r\n", "\r"), "\n", "\r")
		if args.Enter {
			input += "\r"
		}
		terminal.touch()
		return object{"sent": true}, terminal.socket.send("TERMINAL_DATA", input)
	case "ssh_list_connections":
		mode := "native_ssh"
		if s.terminalWeb != nil {
			mode = "web_terminal"
		}
		return object{"connectionId": id, "url": s.client.base.Host, "transport": mode}, nil
	case "ssh_list_known_hosts", "ssh_remove_known_host":
		return s.manageKnownHost(ctx, args, tool == "ssh_remove_known_host")
	default:
		return s.runSFTP(ctx, tool, args, nil)
	}
}

type sshJob struct {
	owner, state, failure string
	finished              time.Time
	result                object
}

func (p *plugin) startSSHJob(owner string, s *session, args operationsArgs, command string) (object, error) {
	if args.Timeout < 0 || args.Timeout > 120 {
		return nil, errors.New("任务超时必须在 1–120 秒之间")
	}
	id, err := randomUUID()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	for oldID, job := range p.jobs {
		if !job.finished.IsZero() && time.Since(job.finished) > 15*time.Minute {
			delete(p.jobs, oldID)
		}
	}
	if len(p.jobs) >= 16 {
		p.mu.Unlock()
		return nil, errors.New("后台任务已达上限")
	}
	job := &sshJob{owner: owner, state: "running"}
	p.jobs[id] = job
	p.mu.Unlock()
	go func() {
		result, err := s.exec(context.Background(), args.sshSelection, command, args.Timeout)
		p.mu.Lock()
		defer p.mu.Unlock()
		job.finished, job.result, job.state = time.Now(), result, "done"
		if err != nil {
			job.state, job.failure = "failed", err.Error()
		} else if result["exitCode"] != 0 {
			job.state = "failed"
		}
	}()
	return object{"taskId": id, "state": "running"}, nil
}

func (s *session) manageKnownHost(ctx context.Context, args operationsArgs, remove bool) (object, error) {
	if _, _, err := s.client.sshAccounts(ctx, args.sshSelection); err != nil {
		return nil, err
	}
	host, err := s.gatewayEndpoint(ctx, args.sshSelection)
	if err != nil {
		return nil, err
	}
	file, err := gatewayKeyPath(host)
	if err != nil {
		return nil, err
	}
	stored, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) && !remove {
		return object{"endpoint": host, "fingerprint": s.sshFingerprint, "stored": false}, nil
	}
	if err != nil {
		return nil, errors.New("无法读取网关主机密钥")
	}
	fingerprint := strings.TrimSpace(string(stored))
	if remove {
		if args.Fingerprint == "" || args.Fingerprint != fingerprint {
			return nil, errors.New("请填写当前保存的准确指纹")
		}
		if err := os.Remove(file); err != nil {
			return nil, errors.New("无法移除保存的网关指纹")
		}
	}
	return object{"endpoint": host, "fingerprint": fingerprint, "stored": !remove}, nil
}
