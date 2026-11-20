package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"time"
)

const workbenchChunk = 512 << 10

type workbenchTransfer struct {
	connection string
	session    *session
	args       operationsArgs
	download   bool
	busy       bool
	size       int64
	offset     int64
	touched    time.Time
}

type workbenchFileRequest struct {
	sshSelection
	RemotePath string            `json:"remotePath"`
	ID         string            `json:"transferId"`
	Data       string            `json:"data"`
	Offset     int64             `json:"offset"`
	Size       int64             `json:"size"`
	Entries    []fileDeleteEntry `json:"entries"`
}

func (p *plugin) handleWorkbenchFiles(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in workbenchFileRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("文件请求无效")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[in.Connection]
	if s == nil {
		return nil, errors.New("请先连接 JumpServer")
	}
	if method == "jumpserver/ssh/files/list" {
		if !validTarget(in.sshSelection) {
			return nil, errors.New("请选择授权的组织、资产和账号")
		}
		p.mu.Unlock()
		defer p.mu.Lock()
		return s.runSFTP(ctx, "sftp_list_dir", operationsArgs{sshSelection: in.sshSelection, RemotePath: in.RemotePath}, p.prompt)
	}
	if s.readOnly {
		return nil, errors.New("此 DBX 连接启用了只读保护")
	}
	if method == "jumpserver/ssh/files/delete" {
		if !validTarget(in.sshSelection) || !validDeleteEntries(in.RemotePath, in.Entries) {
			return nil, errors.New("请选择当前 SFTP 目录中的待删除项目")
		}
		args := operationsArgs{sshSelection: in.sshSelection, RemotePath: in.RemotePath, deleteEntries: in.Entries}
		p.mu.Unlock()
		defer p.mu.Lock()
		return s.runSFTP(ctx, "workbench_delete", args, p.prompt)
	}
	for id, transfer := range p.transfers {
		if !transfer.busy && time.Since(transfer.touched) > 10*time.Minute {
			p.removeTransfer(id)
		}
	}
	switch method {
	case "jumpserver/ssh/files/prepare-download", "jumpserver/ssh/files/prepare-upload":
		download := method == "jumpserver/ssh/files/prepare-download"
		if !validTarget(in.sshSelection) || !validSFTPPath(in.RemotePath) || in.RemotePath == "/" {
			return nil, errors.New("请选择授权的资产、账号和文件路径")
		}
		if !download && (in.Size < 0 || in.Size > transferFileLimit) {
			return nil, errors.New("单个上传文件上限为 8 MiB")
		}
		if len(p.transfers) >= 4 {
			return nil, errors.New("文件传输数量已达上限")
		}
		id, err := randomUUID()
		if err != nil {
			return nil, err
		}
		transfer := &workbenchTransfer{
			connection: in.Connection, session: s, download: download, busy: download, size: in.Size, touched: time.Now(),
			args: operationsArgs{sshSelection: in.sshSelection, RemotePath: in.RemotePath, LocalPath: ".workbench-" + id},
		}
		if !download {
			if err := createUploadStage(transfer.args.LocalPath); err != nil {
				return nil, err
			}
			p.transfers[id] = transfer
			return object{"transferId": id, "size": transfer.size}, nil
		}
		p.transfers[id] = transfer
		p.mu.Unlock()
		result, err := s.runSFTP(ctx, "sftp_download", transfer.args, p.prompt)
		p.mu.Lock()
		if err != nil || p.sessions[in.Connection] != s {
			p.removeTransfer(id)
			if err != nil {
				return nil, err
			}
			return nil, errors.New("JumpServer 连接已切换")
		}
		transfer.size = result.(object)["bytes"].(int64)
		transfer.busy = false
		transfer.touched = time.Now()
		return object{"transferId": id, "size": transfer.size}, nil
	}

	transfer := p.transfers[in.ID]
	if transfer == nil || transfer.connection != in.Connection || transfer.session != s || transfer.busy {
		return nil, errors.New("传输已过期或正在处理")
	}
	transfer.touched = time.Now()
	switch method {
	case "jumpserver/ssh/files/read-download":
		return transfer.read(in.Offset)
	case "jumpserver/ssh/files/write-upload":
		return transfer.write(in.Offset, in.Data)
	case "jumpserver/ssh/files/commit-upload":
		if transfer.download || transfer.offset != transfer.size {
			return nil, errors.New("本机文件尚未完整读取")
		}
		transfer.busy = true
		p.mu.Unlock()
		result, err := s.runSFTP(ctx, "sftp_upload", transfer.args, p.prompt)
		p.mu.Lock()
		p.removeTransfer(in.ID)
		return result, err
	case "jumpserver/ssh/files/close-transfer":
		p.removeTransfer(in.ID)
		return object{"closed": true}, nil
	default:
		return nil, errMethod
	}
}

func createUploadStage(name string) error {
	root, err := openTransfers(name)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("无法创建上传临时文件")
	}
	return file.Close()
}

func (t *workbenchTransfer) read(offset int64) (object, error) {
	if !t.download || offset < 0 || offset >= t.size {
		return nil, errors.New("下载偏移无效")
	}
	root, err := openTransfers(t.args.LocalPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(t.args.LocalPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data := make([]byte, min(workbenchChunk, t.size-offset))
	n, err := file.ReadAt(data, offset)
	if n != len(data) || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, errors.New("读取下载分块失败")
	}
	return object{"data": base64.StdEncoding.EncodeToString(data), "nextOffset": offset + int64(n)}, nil
}

func (t *workbenchTransfer) write(offset int64, encoded string) (object, error) {
	if t.download || offset != t.offset || len(encoded) > base64.StdEncoding.EncodedLen(workbenchChunk) {
		return nil, errors.New("上传分块无效")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > workbenchChunk || int64(len(data)) > t.size-offset {
		return nil, errors.New("上传分块格式或大小不正确")
	}
	root, err := openTransfers(t.args.LocalPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(t.args.LocalPath, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	n, err := file.WriteAt(data, offset)
	closeErr := file.Close()
	if err != nil || n != len(data) || closeErr != nil {
		return nil, errors.New("无法保存上传分块")
	}
	t.offset += int64(n)
	return object{"nextOffset": t.offset}, nil
}

// The caller holds p.mu, or is shutting down after all workers have exited.
func (p *plugin) removeTransfer(id string) {
	transfer := p.transfers[id]
	delete(p.transfers, id)
	if root, err := openTransfers(transfer.args.LocalPath); err == nil {
		defer root.Close()
		_ = root.Remove(transfer.args.LocalPath)
	}
}

func (p *plugin) closeConnectionTransfers(connection string) {
	for id, transfer := range p.transfers {
		if transfer.connection == connection && !transfer.busy {
			p.removeTransfer(id)
		}
	}
}

type fileDeleteEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

func validDeleteEntries(directory string, entries []fileDeleteEntry) bool {
	if !validSFTPPath(directory) || len(entries) == 0 || len(entries) > 1000 {
		return false
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		parent := path.Dir(path.Join(directory, entry.Name))
		if entry.Name == "" || entry.Name == "." || entry.Name == ".." || path.Base(entry.Name) != entry.Name ||
			parent != directory || !validSFTPPath(path.Join(directory, entry.Name)) || seen[entry.Name] {
			return false
		}
		seen[entry.Name] = true
	}
	return true
}

func deleteWorkbenchEntries(ctx context.Context, files remoteFiles, directory string, entries []fileDeleteEntry) (object, error) {
	// Read the current directory once, both to bind the KoKo root and reject
	// stale selections whose file/directory type has changed.
	rows, err := files.list(directory)
	if err != nil {
		return nil, err
	}
	current := make(map[string]bool, len(rows))
	for _, row := range rows {
		if kind, ok := row["is_dir"].(bool); ok {
			current[text(row["name"])] = kind
		}
	}
	results := make([]object, 0, len(entries))
	for _, entry := range entries {
		result := object{"name": entry.Name}
		kind, exists := current[entry.Name]
		switch {
		case ctx.Err() != nil:
			result["error"] = "批量删除超时"
		case !exists || kind != entry.IsDir:
			result["error"] = "项目已不存在或类型已变化，请刷新后重新选择"
		default:
			if err := files.remove(path.Join(directory, entry.Name), entry.IsDir); err != nil {
				result["error"] = err.Error()
			}
		}
		results = append(results, result)
	}
	return object{"items": results}, nil
}
