package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type terminalSocket interface {
	send(string, string) error
	resize(string, int, int) error
	read() ([]byte, string, error)
	close()
}

func (s *session) openTerminal(ctx context.Context, in sshSelection, cols, rows int, prompt userPrompt) (terminalSocket, error) {
	if s.terminalWeb != nil {
		return s.terminalWeb.openKoko(ctx, in, cols, rows)
	}
	client, err := s.connectNative(ctx, in, prompt)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	channel, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, errors.New("KoKo 拒绝打开 SSH 会话")
	}
	reader, writer := io.Pipe()
	t := &nativeTerminal{client: client, session: channel, reader: reader, writer: writer}
	t.input, err = channel.StdinPipe()
	if err != nil {
		t.close()
		return nil, errors.New("无法打开 SSH 终端输入")
	}
	channel.Stdout, channel.Stderr = writer, writer
	if channel.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1}) != nil || channel.Shell() != nil {
		t.close()
		return nil, errors.New("KoKo 拒绝请求交互式 SSH 终端")
	}
	go func() {
		_ = channel.Wait()
		_ = writer.Close()
	}()
	return t, nil
}

type nativeTerminal struct {
	client  *ssh.Client
	session *ssh.Session
	input   io.WriteCloser
	reader  *io.PipeReader
	writer  *io.PipeWriter
	once    sync.Once
}

func (t *nativeTerminal) send(kind, data string) error {
	if kind == "PING" {
		return nil
	}
	_, err := io.WriteString(t.input, data)
	return err
}

func (t *nativeTerminal) resize(_ string, cols, rows int) error {
	if cols < 10 || cols > 500 || rows < 3 || rows > 250 {
		return errors.New("终端尺寸超出范围")
	}
	return t.session.WindowChange(rows, cols)
}

func (t *nativeTerminal) read() ([]byte, string, error) {
	data := make([]byte, 8192)
	n, err := t.reader.Read(data)
	return data[:n], "", err
}

func (t *nativeTerminal) close() {
	t.once.Do(func() {
		_ = t.reader.Close()
		_ = t.writer.Close()
		_ = t.client.Close()
	})
}

func (s *session) gatewayEndpoint(ctx context.Context, in sshSelection) (string, error) {
	if !validOrg(in.OrgID) || !safeID.MatchString(in.AssetID) {
		return "", errors.New("请选择有效组织和 SSH 资产")
	}
	query := url.Values{"protocol": {"ssh"}, "asset_id": {in.AssetID}}
	value, err := s.client.request(ctx, "GET", "/terminal/endpoints/smart/", in.OrgID, query, nil)
	if err != nil {
		return "", fmt.Errorf("读取 KoKo SSH 网关失败：%w", err)
	}
	endpoint := asObject(value)
	host, port := strings.TrimSpace(text(endpoint["host"])), number(endpoint["ssh_port"])
	if s.sshHost != "" {
		host = s.sshHost
	}
	if s.sshPort != 0 {
		port = s.sshPort
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" || strings.ContainsAny(host, "/\\@?#[]\r\n\t ") ||
		(strings.Contains(host, ":") && net.ParseIP(host) == nil) || port < 1 || port > 65535 {
		return "", errors.New("JumpServer 未返回有效的 KoKo SSH 主机或端口")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (s *session) connectNative(ctx context.Context, in sshSelection, prompt userPrompt) (*ssh.Client, error) {
	return s.connectNativeMethod(ctx, in, prompt, "ssh_client")
}

func (s *session) connectNativeMethod(ctx context.Context, in sshSelection, prompt userPrompt, method string) (*ssh.Client, error) {
	endpoint, err := s.gatewayEndpoint(ctx, in)
	if err != nil {
		return nil, err
	}
	token, err := s.client.mintSSH(ctx, in, method)
	if err != nil {
		return nil, fmt.Errorf("申请 KoKo SSH 客户端令牌失败：%w", err)
	}
	transport, err := (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("无法连接 KoKo SSH 网关 %s，请核对网关端口和网络可达性", endpoint)
	}
	stop := context.AfterFunc(ctx, func() { _ = transport.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = transport.Close()
		}
	}()
	_ = transport.SetDeadline(time.Now().Add(7 * time.Second))
	verified := false
	cfg := &ssh.ClientConfig{
		User: "JMS-" + token.id,
		Auth: []ssh.AuthMethod{ssh.Password(token.secret)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			_ = transport.SetDeadline(time.Time{})
			err := s.verifyHostKey(ctx, endpoint, ssh.FingerprintSHA256(key), prompt)
			_ = transport.SetDeadline(time.Now().Add(7 * time.Second))
			verified = err == nil
			return err
		},
	}
	// Authenticate only after the hostname/fingerprint check succeeds.
	conn, channels, requests, err := ssh.NewClientConn(transport, endpoint, cfg)
	if err != nil {
		var trustError *hostKeyError
		if errors.As(err, &trustError) {
			return nil, trustError
		}
		if !verified {
			return nil, fmt.Errorf("KoKo 网关 %s 未完成 SSH 握手，请检查该端口是否提供原生 SSH，或在连接设置启用网页终端", endpoint)
		}
		return nil, errors.New("KoKo 拒绝 SSH 客户端令牌，请检查 JumpServer SSH 客户端功能、账号授权和登录策略")
	}
	_ = transport.SetDeadline(time.Time{})
	return ssh.NewClient(conn, channels, requests), nil
}

type hostKeyError struct{ message string }

func (e *hostKeyError) Error() string { return e.message }

func (s *session) verifyHostKey(ctx context.Context, endpoint, fingerprint string, prompt userPrompt) error {
	if s.sshFingerprint != "" {
		if s.sshFingerprint != fingerprint {
			return &hostKeyError{"KoKo 网关主机密钥与连接配置不一致：" + endpoint}
		}
		return nil
	}
	filename, err := gatewayKeyPath(endpoint)
	if err != nil {
		return &hostKeyError{"请先在 DBX 连接设置中填写可信的 KoKo SSH 主机密钥指纹"}
	}
	dir := filepath.Dir(filename)
	stored, err := os.ReadFile(filename)
	if err == nil {
		if strings.TrimSpace(string(stored)) != fingerprint {
			return &hostKeyError{"KoKo 网关主机密钥已变化，请向管理员核对后更新连接配置：" + endpoint}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return &hostKeyError{"无法读取已信任的 KoKo 网关主机密钥"}
	}
	if prompt == nil {
		return &hostKeyError{"请先在 DBX 桌面连接中核对 KoKo 网关主机密钥：" + endpoint + " " + fingerprint}
	}
	answer, err := prompt(ctx, userQuestion{
		title: "核对 KoKo SSH 网关主机密钥",
		text:  endpoint + " 的主机密钥指纹为 " + fingerprint + "。请和服务器管理员提供的指纹核对，再选择信任。",
		options: []object{
			{"value": "trust", "label": "指纹一致，信任此网关"},
			{"value": "reject", "label": "不信任"},
		},
	})
	if err != nil || answer != "trust" {
		return &hostKeyError{"KoKo 网关主机密钥未获得确认"}
	}
	if os.MkdirAll(dir, 0700) != nil {
		return &hostKeyError{"无法创建 KoKo 网关主机密钥目录"}
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return s.verifyHostKey(ctx, endpoint, fingerprint, nil)
	}
	if err != nil {
		return &hostKeyError{"无法保存 KoKo 网关主机密钥"}
	}
	_, writeErr := io.WriteString(file, fingerprint+"\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return &hostKeyError{"保存 KoKo 网关主机密钥失败"}
	}
	return nil
}

func gatewayKeyPath(endpoint string) (string, error) {
	dir := os.Getenv("DBX_PLUGIN_DATA_DIR")
	if dir == "" {
		return "", errors.New("DBX 未提供插件数据目录")
	}
	return filepath.Join(dir, "ssh-host-keys", fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint)))), nil
}

type limitedSSHOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *limitedSSHOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buffer.Len()+len(data) > 512<<10 {
		return 0, errors.New("巡检结果超过 512 KiB")
	}
	return b.buffer.Write(data)
}

func (s *session) inspectNative(ctx context.Context, in sshSelection, command string) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, 14*time.Second)
	defer cancel()
	result, err := s.execNative(ctx, in, command)
	if err == nil && result["output"] == "" {
		return nil, errors.New("KoKo SSH 巡检未返回输出，请检查目标和命令授权")
	}
	return result, err
}

func (s *session) execNative(ctx context.Context, in sshSelection, command string) (object, error) {
	client, err := s.connectNative(ctx, in, nil)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	channel, err := client.NewSession()
	if err != nil {
		return nil, errors.New("KoKo 拒绝打开 SSH 巡检会话")
	}
	output := &limitedSSHOutput{}
	channel.Stdout, channel.Stderr = output, output
	exitCode := 0
	if err := channel.Run(command); err != nil {
		var remote *ssh.ExitError
		if !errors.As(err, &remote) {
			return nil, errors.New("KoKo SSH 巡检未返回完整结果")
		}
		exitCode = remote.ExitStatus()
	}
	clean := strings.TrimSpace(terminalControl.ReplaceAllString(output.buffer.String(), ""))
	return object{"command": command, "output": clean, "exitCode": exitCode}, nil
}
