package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

type webFiles struct {
	socket *websocket.Conn
	root   string
}

type fileFrame struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Cmd         string `json:"cmd,omitempty"`
	Data        string `json:"data,omitempty"`
	Raw         []byte `json:"raw,omitempty"`
	Err         string `json:"err,omitempty"`
	CurrentPath string `json:"current_path,omitempty"`
}

func (c *jmsClient) openWebFiles(ctx context.Context, in sshSelection) (*webFiles, error) {
	token, err := c.mintSSH(ctx, in, "web_cli")
	if err != nil {
		return nil, err
	}
	u, origin := c.kokoURL(token)
	if token.legacy {
		return nil, errors.New("此 KoKo 版本未提供当前 Web SFTP 协议，请使用可达的原生 SFTP 网关")
	}
	u.Path = strings.TrimSuffix(u.Path, "/terminal/") + "/sftp/"
	ws, resp, err := c.dialKoko(ctx, u, origin)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, errors.New("KoKo 未建立 Web SFTP 会话，请检查文件管理权限和部署版本")
	}
	ws.SetReadLimit(4 << 20)
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.SetReadDeadline(deadline)
		_ = ws.SetWriteDeadline(deadline)
	}
	files := &webFiles{socket: ws}
	var response fileFrame
	if ws.ReadJSON(&response) != nil || response.Type != "CONNECT" {
		ws.Close()
		return nil, errors.New("KoKo 未确认 Web SFTP 会话")
	}
	return files, nil
}

func (f *webFiles) exchange(id, command string, args object, raw []byte, writer io.Writer, limit int64) (fileFrame, int64, error) {
	request, _ := json.Marshal(args)
	if err := f.socket.WriteJSON(fileFrame{ID: id, Type: "SFTP_DATA", Cmd: command, Data: string(request), Raw: raw}); err != nil {
		return fileFrame{}, 0, errors.New("发送 KoKo SFTP 请求失败")
	}
	var written int64
	for {
		var frame fileFrame
		if err := f.socket.ReadJSON(&frame); err != nil {
			return fileFrame{}, 0, errors.New("KoKo SFTP 会话已断开或返回的协议不受支持")
		}
		if frame.Type == "ERROR" || frame.Type == "CLOSE" {
			return fileFrame{}, 0, errors.New("KoKo SFTP 会话被关闭")
		}
		if frame.ID != id || frame.Cmd != command {
			continue
		}
		if frame.Err != "" {
			return fileFrame{}, 0, errors.New("KoKo 拒绝 SFTP 操作，请检查 JumpServer 账号和文件路径权限")
		}
		switch frame.Type {
		case "SFTP_BINARY":
			if int64(len(frame.Raw)) > limit-written || writer == nil {
				return fileFrame{}, 0, errors.New("文件超过此工具的传输上限")
			}
			n, err := writer.Write(frame.Raw)
			if err != nil || n != len(frame.Raw) {
				return fileFrame{}, 0, errors.New("无法写入下载文件")
			}
			written += int64(n)
		case "SFTP_DATA":
			return frame, written, nil
		}
	}
}

func (f *webFiles) list(remote string) ([]object, error) {
	if f.root == "" && remote != "/" {
		if _, err := f.list("/"); err != nil {
			return nil, err
		}
	}
	id, err := randomUUID()
	if err != nil {
		return nil, err
	}
	wirePath := remote
	if f.root != "" {
		wirePath = f.resolvedPath(remote)
	}
	result, _, err := f.exchange(id, "list", object{"path": wirePath}, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	var rows []object
	if json.Unmarshal([]byte(result.Data), &rows) != nil || rows == nil {
		return nil, errors.New("KoKo 返回的 SFTP 目录格式异常")
	}
	if len(rows) > 1000 {
		return nil, errors.New("目录超过 1000 项，请缩小查询范围")
	}
	current := result.CurrentPath
	if current != "" && !validSFTPPath(strings.TrimRight(current, "/")) && current != "/" {
		return nil, errors.New("KoKo 返回了无效的 SFTP 路径")
	}
	if current != "" {
		current = path.Clean(current)
	}
	if f.root == "" {
		f.root = current
		if f.root == "" {
			f.root = "/"
		}
	} else if current != "" && current != f.resolvedPath(remote) {
		return nil, errors.New("KoKo 返回的路径与授权 SFTP 根目录下的目标不一致")
	}
	return rows, nil
}

func (f *webFiles) resolvedPath(remote string) string {
	// Legacy KoKo also accepts the physical form when it is inside the assigned root.
	if remote == f.root || strings.HasPrefix(remote, strings.TrimRight(f.root, "/")+"/") {
		return remote
	}
	return path.Join(f.root, remote)
}

func (f *webFiles) stat(remote string) (object, error) {
	if remote == "/" {
		if _, err := f.list(remote); err != nil {
			return nil, err
		}
		return object{"name": "/", "is_dir": true}, nil
	}
	rows, err := f.list(path.Dir(remote))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if text(row["name"]) == path.Base(remote) {
			return row, nil
		}
	}
	return nil, os.ErrNotExist
}

func (f *webFiles) readFile(remote string, limit int) ([]byte, error) {
	var data bytes.Buffer
	_, err := f.copyFile(remote, &data, int64(limit))
	return data.Bytes(), err
}

func (f *webFiles) copyFile(remote string, writer io.Writer, limit int64) (int64, error) {
	info, err := f.stat(remote)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(fmt.Sprint(info["size"]), 10, 64)
	if info["is_dir"] != false || err != nil || size < 0 || size > limit {
		return 0, errors.New("文件类型或大小超出下载限制")
	}
	id, err := randomUUID()
	if err != nil {
		return 0, err
	}
	result, count, err := f.exchange(id, "download", object{"path": f.resolvedPath(remote), "is_dir": false}, nil, writer, size)
	if err == nil && (result.Data != path.Base(remote) || count != size) {
		err = errors.New("KoKo 下载完成信息与文件名或大小不一致")
	}
	return count, err
}

func (f *webFiles) writeFile(remote string, data []byte, overwrite bool) error {
	_, err := f.stat(remote)
	if err == nil && !overwrite {
		return errors.New("SFTP 目标文件已存在")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var idBytes [4]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return err
	}
	id := strconv.FormatUint(uint64(binary.BigEndian.Uint32(idBytes[:])&0x7fffffff)+1, 10)
	const chunkSize = 512 << 10
	chunked := len(data) > chunkSize
	if !chunked {
		result, _, err := f.exchange(id, "upload", object{"path": f.resolvedPath(remote), "size": len(data), "chunk": false}, data, nil, 0)
		if err == nil && result.Data != "ok" {
			err = errors.New("KoKo 未确认文件上传")
		}
		return err
	}
	for offset := 0; offset < len(data); offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		if _, _, err := f.exchange(id, "upload", object{"path": f.resolvedPath(remote), "size": len(data), "offset": offset, "chunk": true}, data[offset:end], nil, 0); err != nil {
			return err
		}
	}
	result, _, err := f.exchange(id, "upload", object{"path": f.resolvedPath(remote), "merge": true}, nil, nil, 0)
	if err == nil && result.Data != "ok" {
		err = errors.New("KoKo 未确认上传分片合并")
	}
	return err
}

func (f *webFiles) remove(remote string, _ bool) error {
	target := f.resolvedPath(remote)
	if f.root == "" || target == f.root || !strings.HasPrefix(target, strings.TrimRight(f.root, "/")+"/") {
		return errors.New("不能删除 SFTP 根目录或授权范围之外的路径")
	}
	id, err := randomUUID()
	if err != nil {
		return err
	}
	response, _, err := f.exchange(id, "rm", object{"path": target}, nil, nil, 0)
	if err == nil && response.Data != "ok" {
		err = errors.New("KoKo 未确认删除完成")
	}
	return err
}
