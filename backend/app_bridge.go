package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func desktopBridgePort() uint64 {
	dir := os.Getenv("DBX_PLUGIN_DATA_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(dir, "..", "..", "mcp-bridge-port"))
	if err != nil {
		return 0
	}
	port, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 16)
	if err != nil {
		return 0
	}
	return port
}

func callDesktopTool(ctx context.Context, port uint64, id, tool string, args operationsArgs) (json.RawMessage, error) {
	args.DesktopRelay = true
	args.Connection = ""
	for i := range args.Targets {
		args.Targets[i].Connection = ""
	}
	body, err := json.Marshal(object{
		"connection_id": id, "plugin_id": pluginID, "tool": tool, "arguments": args, "timeout_ms": 295_000,
	})
	if err != nil {
		return nil, err
	}
	if len(body) > 60<<10 {
		return nil, errors.New("文件内容超出 DBX 桌面转发大小，请将文件放入插件 transfers 目录并使用 sftp_upload")
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/call-plugin-tool", port)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true},
		Timeout:   295 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("无法连接 DBX 桌面端，请确认 DBX 已打开后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DBX 桌面端未接受 JumpServer 工具调用（HTTP %d）", response.StatusCode)
	}
	var result struct {
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil || len(result.Content) == 0 || result.Content[0] != '[' {
		return nil, errors.New("DBX 桌面端返回了无效的 MCP 结果")
	}
	return json.Marshal(result)
}
