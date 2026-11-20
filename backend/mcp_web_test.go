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
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMCPRelaysTerminalInputOnlyForEnabledSavedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Connection string         `json:"connection_id"`
			Plugin     string         `json:"plugin_id"`
			Tool       string         `json:"tool"`
			Arguments  operationsArgs `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.URL.Path != "/call-plugin-tool" ||
			req.Connection != "saved" || req.Plugin != pluginID || req.Tool != "ssh_terminal_input" ||
			!req.Arguments.DesktopRelay || req.Arguments.Connection != "" || req.Arguments.Input != "fixture-input" {
			t.Errorf("unexpected desktop request %+v: %v", req, err)
			http.Error(w, "invalid test request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"fixture reply"}],"isError":false}`)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	t.Setenv("DBX_PLUGIN_DATA_DIR", filepath.Join(dataDir, "plugin-data", pluginID))
	if err := os.WriteFile(filepath.Join(dataDir, "mcp-bridge-port"), []byte(port), 0600); err != nil {
		t.Fatal(err)
	}
	p := newPlugin()
	for _, enabled := range []bool{false, true} {
		const input = `{"tool":"ssh_terminal_input","lifecycle":{"connection":{"id":"saved","host":"https://fixture.example","external_config":{"mcp_operations":%s},"connection_secrets":{"access_key":"fixture-key","secret_key":"fixture-secret"}}},"arguments":{"connectionId":"untrusted","orgId":"org","assetId":"asset","accountId":"fixture","input":"fixture-input"}}`
		result, err := p.callMCP(context.Background(), json.RawMessage(fmt.Sprintf(input, strconv.FormatBool(enabled))))
		if err != nil {
			t.Fatal(err)
		}
		if enabled {
			if result, ok := result.(json.RawMessage); !ok || !bytes.Contains(result, []byte("fixture reply")) {
				t.Fatalf("desktop result was not relayed: %v", result)
			}
		} else if result, ok := result.(object); !ok || result["isError"] != true {
			t.Fatalf("disabled connection reached the desktop bridge: %v", result)
		}
	}
}

func TestWebFilesUseAuthorizedKoKoChannel(t *testing.T) {
	cookie := &http.Cookie{Name: "jms_sessionid", Value: "web-test", Path: "/"}
	payload := bytes.Repeat([]byte("a"), (512<<10)+13)
	upgrader := websocket.Upgrader{Subprotocols: []string{"JMS-KOKO"}}
	var transferred atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, _ := r.Cookie(cookie.Name); got == nil || got.Value != cookie.Value {
			t.Error("KoKo request did not use the authenticated web session")
			http.Error(w, "test session required", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/perms/users/self/assets/asset/":
			fmt.Fprint(w, `{"permed_protocols":["ssh"],"permed_accounts":[{"alias":"svc","username":"svc"}]}`)
		case "/api/v1/authentication/connection-token/":
			fmt.Fprint(w, `{"id":"test-token","is_active":true}`)
		case "/koko/ws/sftp/":
			if r.URL.Query().Get("token") != "test-token" {
				t.Error("missing asset-bound file token")
			}
			socket, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer socket.Close()
			_ = socket.WriteJSON(fileFrame{ID: "connection", Type: "CONNECT"})
			for {
				var request fileFrame
				if socket.ReadJSON(&request) != nil {
					return
				}
				response := fileFrame{ID: request.ID, Cmd: request.Cmd, Type: "SFTP_DATA"}
				var arguments object
				if json.Unmarshal([]byte(request.Data), &arguments) != nil {
					t.Error("file request had invalid arguments")
					return
				}
				switch request.Cmd {
				case "list":
					response.CurrentPath = text(arguments["path"])
					if response.CurrentPath == "/" {
						response.CurrentPath = "/home/svc/"
					}
					response.Data = `[{"name":"fixture.txt","is_dir":false,"size":"4"},{"name":"large.jar","is_dir":false,"size":"9437184"},{"name":"truncated.jar","is_dir":false,"size":"6"},{"name":"folder","is_dir":true}]`
					if response.CurrentPath == "/home/svc/wrong" {
						response.CurrentPath = "/other"
					}
				case "rm":
					switch arguments["path"] {
					case "/home/svc/fixture.txt", "/home/svc/folder":
						response.Data = "ok"
					case "/home/svc/large.jar":
						response.Err = "Permission denied"
					default:
						t.Errorf("unexpected deletion target %v", arguments["path"])
						response.Err = "Unexpected test target"
					}
				case "download":
					binary := response
					remote := text(arguments["path"])
					if filepath.Dir(remote) != "/home/svc" {
						t.Error("download did not use the authorized root")
					}
					binary.Type, binary.Raw = "SFTP_BINARY", []byte("test")
					if filepath.Base(remote) == "large.jar" {
						binary.Raw = make([]byte, 1<<20)
						for i := 0; i < 9; i++ {
							_ = socket.WriteJSON(binary)
						}
					} else {
						_ = socket.WriteJSON(binary)
					}
					response.Data = filepath.Base(remote)
				case "upload":
					if arguments["merge"] == true {
						if transferred.Load() != int64(len(payload)) {
							t.Error("upload merged without all chunks")
						}
						response.Data = "ok"
					} else if arguments["chunk"] != true {
						if string(request.Raw) != "ab" {
							t.Error("invalid unchunked upload")
						}
						response.Data = "ok"
					} else {
						if number(arguments["offset"]) != int(transferred.Load()) {
							t.Error("incorrect file chunk offset")
						}
						transferred.Add(int64(len(request.Raw)))
						response.Data = "ack"
					}
				default:
					t.Error("unexpected KoKo file command")
				}
				_ = socket.WriteJSON(response)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := newClientWithAuth(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Jar, _ = cookiejar.New(nil)
	client.http.Jar.SetCookies(client.base, []*http.Cookie{cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	files, err := client.openWebFiles(ctx, sshSelection{OrgID: "org", AssetID: "asset", AccountID: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	defer files.socket.Close()
	rows, err := files.list("/")
	if err != nil || len(rows) != 4 {
		t.Fatalf("list: %v %v", rows, err)
	}
	data, err := files.readFile("/fixture.txt", 8)
	if err != nil || string(data) != "test" {
		t.Fatalf("download: %q %v", data, err)
	}
	if _, err := files.list("/wrong"); err == nil {
		t.Fatal("directory outside the SFTP root was accepted")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	download := func(name string) (int64, error) {
		return saveDownload(root, name, func(writer io.Writer) (int64, error) {
			return files.copyFile("/"+name, writer, downloadFileLimit)
		})
	}
	if count, err := download("large.jar"); err != nil || count != 9<<20 {
		t.Fatalf("large download: %d %v", count, err)
	}
	if info, err := root.Stat("large.jar"); err != nil || info.Size() != 9<<20 {
		t.Fatalf("invalid resulting file: %v %v", info, err)
	}
	if _, err := download("large.jar"); err == nil {
		t.Fatal("same-name download was overwritten")
	}
	if _, err := download("truncated.jar"); err == nil {
		t.Fatal("truncated file was published")
	}
	if _, err := root.Stat("truncated.jar"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("truncated download left a destination file")
	}
	if err := files.writeFile("/new.txt", payload, false); err != nil {
		t.Fatal(err)
	}
	if err := files.writeFile("/fixture.txt", payload, false); err == nil {
		t.Fatal("upload unexpectedly allowed overwriting a listed file")
	}
	t.Setenv("DBX_PLUGIN_DATA_DIR", t.TempDir())
	plugin := newPlugin()
	plugin.sessions["workbench"] = &session{client: client, terminalWeb: client}
	call := func(method string, args object) (object, error) {
		args["connectionId"], args["orgId"], args["assetId"], args["accountId"] = "workbench", "org", "asset", "svc"
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		result, err := plugin.handle(ctx, "jumpserver/ssh/files/"+method, raw)
		if err != nil {
			return nil, err
		}
		return result.(object), nil
	}
	staged, err := call("prepare-download", object{"remotePath": "/fixture.txt"})
	if err != nil {
		t.Fatal(err)
	}
	downloadID := staged["transferId"]
	response, err := call("read-download", object{"transferId": downloadID})
	if err != nil || response["data"] != "dGVzdA==" {
		t.Fatalf("workbench download payload: %v %v", response, err)
	}
	if _, err := call("close-transfer", object{"transferId": downloadID}); err != nil {
		t.Fatal(err)
	}

	staged, err = call("prepare-upload", object{"remotePath": "/new.txt", "size": 2})
	if err != nil {
		t.Fatal(err)
	}
	uploadID := staged["transferId"]
	if _, err := call("commit-upload", object{"transferId": uploadID}); err == nil {
		t.Fatal("incomplete upload passed the size check")
	}
	if _, err := call("write-upload", object{"transferId": uploadID, "data": "YWI="}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("commit-upload", object{"transferId": uploadID}); err != nil {
		t.Fatal(err)
	}
	result, err := call("delete", object{"remotePath": "/", "entries": []fileDeleteEntry{
		{Name: "fixture.txt"}, {Name: "large.jar"}, {Name: "folder", IsDir: true}, {Name: "truncated.jar", IsDir: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	deleted := result["items"].([]object)
	if deleted[0]["error"] != nil || deleted[1]["error"] == nil || deleted[2]["error"] != nil || deleted[3]["error"] == nil {
		t.Fatalf("unexpected partial failure response: %v", deleted)
	}
	if _, err := call("delete", object{"remotePath": "/", "entries": []fileDeleteEntry{{Name: "../fixture.txt"}}}); err == nil {
		t.Fatal("deletion accepted path traversal")
	}
	plugin.sessions["workbench"].readOnly = true
	if _, err := call("delete", object{"remotePath": "/", "entries": []fileDeleteEntry{{Name: "fixture.txt"}}}); err == nil {
		t.Fatal("deletion bypassed read-only connection")
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("DBX_PLUGIN_DATA_DIR"), "transfers"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging was not cleaned: %v %v", entries, err)
	}

}

func TestAccessKeyWebModeRequiresSameAccount(t *testing.T) {
	var passwordPosts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/profile/":
			if r.Header.Get("Authorization") == "" {
				if cookie, _ := r.Cookie("jms_sessionid"); cookie == nil {
					t.Error("web profile omitted logged-in session")
				}
			}
			fmt.Fprint(w, `{"username":"operator"}`)
		case "/core/auth/login/":
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `<input name="csrfmiddlewaretoken" value="masked">`)
				return
			}
			passwordPosts.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "jms_sessionid", Value: "web-test", Path: "/"})
			http.Redirect(w, r, "/ui/", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	signed, err := newClient(server.URL, "ak", "sk")
	if err != nil {
		t.Fatal(err)
	}
	web, err := newClientWithAuth(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{client: signed, terminalWeb: web, loginUsername: "other", loginPassword: "test"}
	if err := s.authenticate(context.Background(), nil); err == nil || passwordPosts.Load() != 0 {
		t.Fatal("different username reached web password authentication")
	}
	s.loginUsername = "operator"
	if err := s.authenticate(context.Background(), nil); err != nil || passwordPosts.Load() != 1 {
		t.Fatalf("expected separate signed connection and web login: %v", err)
	}
	if signed.http.Jar != nil || s.loginPassword != "" {
		t.Fatal("signed API session changed or plaintext password remained in the plugin session")
	}
}

func TestReadOnlyGrantDoesNotEnableExpandedOperations(t *testing.T) {
	request := object{
		"tool":      "ssh_exec",
		"arguments": object{"command": "hostname"},
		"lifecycle": object{"connection": object{
			"id": "saved", "host": "http://127.0.0.1:1",
			"external_config":    object{"mcp_read": true},
			"connection_secrets": object{"access_key": "test-ak", "secret_key": "test-sk"},
		}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := newPlugin().callMCP(context.Background(), raw)
	if err != nil || asObject(result)["isError"] != true {
		t.Fatalf("expanded tool was not blocked by the connection gate: %v %v", result, err)
	}
}
