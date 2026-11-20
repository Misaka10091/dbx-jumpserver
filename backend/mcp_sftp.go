package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pkg/sftp"
)

const inlineFileLimit = 512 << 10
const transferFileLimit = 8 << 20
const downloadFileLimit = 512 << 20

type remoteFiles interface {
	list(string) ([]object, error)
	stat(string) (object, error)
	readFile(string, int) ([]byte, error)
	copyFile(string, io.Writer, int64) (int64, error)
	writeFile(string, []byte, bool) error
	remove(string, bool) error
}

func validSFTPPath(remote string) bool {
	return strings.HasPrefix(remote, "/") && path.Clean(remote) == remote && len(remote) <= 2048 &&
		!strings.Contains(remote, "\\") && !strings.ContainsFunc(remote, unicode.IsControl)
}

func (s *session) runSFTP(ctx context.Context, tool string, args operationsArgs, prompt userPrompt) (any, error) {
	remote := args.RemotePath
	if !validSFTPPath(remote) {
		return nil, errors.New("请选择有效的远程 SFTP 绝对路径")
	}
	if args.Encoding != "" && args.Encoding != "utf8" && args.Encoding != "base64" {
		return nil, errors.New("文件编码必须为 utf8 或 base64")
	}
	timeout := 45 * time.Second
	if tool == "sftp_download" {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var files remoteFiles
	if s.terminalWeb != nil {
		web, err := s.terminalWeb.openWebFiles(ctx, args.sshSelection)
		if err != nil {
			return nil, err
		}
		defer web.socket.Close()
		stop := context.AfterFunc(ctx, func() { _ = web.socket.Close() })
		defer stop()
		files = web
	} else {
		conn, err := s.connectNativeMethod(ctx, args.sshSelection, prompt, "sftp_client")
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		client, err := sftp.NewClient(conn)
		if err != nil {
			return nil, errors.New("KoKo 未开启或拒绝 SFTP 子系统")
		}
		defer client.Close()
		files = &nativeFiles{client}
	}
	switch tool {
	case "workbench_delete":
		return deleteWorkbenchEntries(ctx, files, remote, args.deleteEntries)
	case "sftp_list_dir":
		items, err := files.list(remote)
		result := object{"items": items}
		if web, ok := files.(*webFiles); ok && err == nil {
			result["sftpRoot"], result["resolvedPath"] = web.root, web.resolvedPath(remote)
		}
		return result, err
	case "sftp_stat":
		return files.stat(remote)
	case "sftp_read_file":
		data, err := files.readFile(remote, inlineFileLimit)
		if err != nil {
			return nil, err
		}
		if args.Encoding == "base64" {
			return object{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64", "bytes": len(data)}, nil
		}
		if !utf8.Valid(data) {
			return nil, errors.New("文件不是 UTF-8 文本，请选择 base64")
		}
		return object{"content": string(data), "encoding": "utf8", "bytes": len(data)}, nil
	case "sftp_write_file":
		data := []byte(args.Content)
		if args.Encoding == "base64" {
			var err error
			data, err = base64.StdEncoding.DecodeString(args.Content)
			if err != nil {
				return nil, errors.New("文件内容的 base64 编码无效")
			}
		}
		if remote == "/" || len(data) > inlineFileLimit {
			return nil, errors.New("请选择文件路径，并将单次写入控制在 512 KiB 以内")
		}
		return object{"bytes": len(data)}, files.writeFile(remote, data, args.Overwrite)
	case "sftp_upload", "sftp_download":
		if remote == "/" {
			return nil, errors.New("请选择文件路径")
		}
		root, err := openTransfers(args.LocalPath)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if tool == "sftp_upload" {
			file, err := root.Open(args.LocalPath)
			if err != nil {
				return nil, errors.New("无法读取插件传输目录中的指定文件")
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				return nil, errors.New("只能上传普通文件")
			}
			data, err := readBounded(file, transferFileLimit)
			if err != nil {
				return nil, err
			}
			return object{"bytes": len(data)}, files.writeFile(remote, data, args.Overwrite)
		}
		count, err := saveDownload(root, args.LocalPath, func(writer io.Writer) (int64, error) {
			return files.copyFile(remote, writer, downloadFileLimit)
		})
		if err != nil {
			return nil, err
		}
		return object{"localPath": filepath.Join(root.Name(), args.LocalPath), "bytes": count}, nil
	}
	return nil, errors.New("未知 SFTP 工具")
}

func openTransfers(name string) (*os.Root, error) {
	dir := os.Getenv("DBX_PLUGIN_DATA_DIR")
	if dir == "" || !filepath.IsLocal(name) || filepath.Base(name) != name ||
		strings.ContainsAny(name, "/\\:") || strings.ContainsFunc(name, unicode.IsControl) {
		return nil, errors.New("请填写插件传输目录下的单个文件名")
	}
	path := filepath.Join(dir, "transfers")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, errors.New("无法打开插件传输目录")
	}
	return os.OpenRoot(path)
}

func saveDownload(root *os.Root, name string, download func(io.Writer) (int64, error)) (int64, error) {
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("本机下载文件已存在或路径不可用")
	}
	id, err := randomUUID()
	if err != nil {
		return 0, err
	}
	temporary := ".download-" + id
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, errors.New("无法创建本机下载临时文件")
	}
	defer root.Remove(temporary)
	count, copyErr := download(file)
	closeErr := file.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, errors.New("无法保存下载文件")
	}
	if err := root.Link(temporary, name); err != nil {
		return 0, errors.New("无法提交下载文件，目标已存在或本机文件系统不支持")
	}
	return count, nil
}

func readBounded(reader io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, errors.New("读取文件失败")
	}
	if len(data) > limit {
		return nil, errors.New("文件超过此工具的传输上限")
	}
	return data, nil
}

type nativeFiles struct{ client *sftp.Client }

func fileMetadata(info os.FileInfo) object {
	return object{"name": info.Name(), "size": info.Size(), "is_dir": info.IsDir(), "perm": info.Mode().String(), "mod_time": info.ModTime()}
}

func (f *nativeFiles) list(remote string) ([]object, error) {
	rows, err := f.client.ReadDir(remote)
	if err != nil {
		return nil, errors.New("SFTP 列出目录失败")
	}
	items := make([]object, 0, len(rows))
	if len(rows) > 1000 {
		return nil, errors.New("目录超过 1000 项，请缩小查询范围")
	}
	for _, row := range rows {
		items = append(items, fileMetadata(row))
	}
	return items, nil
}

func (f *nativeFiles) stat(remote string) (object, error) {
	info, err := f.client.Lstat(remote)
	if err != nil {
		return nil, errors.New("无法查看 SFTP 路径")
	}
	return fileMetadata(info), nil
}

func (f *nativeFiles) readFile(remote string, limit int) ([]byte, error) {
	var data bytes.Buffer
	_, err := f.copyFile(remote, &data, int64(limit))
	return data.Bytes(), err
}

func (f *nativeFiles) copyFile(remote string, writer io.Writer, limit int64) (int64, error) {
	file, err := f.client.Open(remote)
	if err != nil {
		return 0, errors.New("无法打开 SFTP 文件")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return 0, errors.New("文件不可读、类型不支持或超出大小上限")
	}
	count, err := io.Copy(writer, io.LimitReader(file, info.Size()+1))
	if err != nil || count != info.Size() {
		return 0, errors.New("SFTP 下载未完成或文件大小已变化")
	}
	return count, nil
}

func (f *nativeFiles) writeFile(remote string, data []byte, overwrite bool) error {
	key, err := randomUUID()
	if err != nil {
		return errors.New("无法创建上传标识")
	}
	tmp := path.Join(path.Dir(remote), ".dbx-upload-"+key)
	file, err := f.client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return errors.New("SFTP 无法创建临时上传文件")
	}
	defer func() { _ = f.client.Remove(tmp) }()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("SFTP 写入未完成")
	}
	if overwrite {
		err = f.client.PosixRename(tmp, remote)
	} else {
		err = f.client.Rename(tmp, remote)
	}
	if err != nil {
		return errors.New("SFTP 无法提交上传，请检查目标是否存在及写入权限")
	}
	return nil
}

func (f *nativeFiles) remove(remote string, directory bool) error {
	info, err := f.client.Lstat(remote)
	if err != nil || info.IsDir() != directory {
		return errors.New("项目已不存在或类型已变化")
	}
	if directory {
		err = f.client.RemoveAll(remote)
	} else {
		err = f.client.Remove(remote)
	}
	if err != nil {
		return errors.New("SFTP 删除未完成，请检查 JumpServer 权限及目录状态")
	}
	return nil
}
