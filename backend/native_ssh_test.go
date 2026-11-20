package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestAccessKeyUsesAuditedSSHGatewayAndVerifiesKey(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	var passwordAttempts atomic.Int32
	sshConfig := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		passwordAttempts.Add(1)
		if meta.User() != "JMS-test-token" || string(password) != "temporary-test-password" {
			return nil, fmt.Errorf("bad test token")
		}
		return nil, nil
	}}
	sshConfig.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, sshConfig)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for request := range channels {
					if request.ChannelType() != "session" {
						_ = request.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					channel, incoming, err := request.Accept()
					if err != nil {
						return
					}
					for req := range incoming {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var args struct{ Command string }
						if ssh.Unmarshal(req.Payload, &args) != nil || args.Command != "hostname" {
							t.Error("unexpected remote command")
						}
						_ = req.Reply(true, nil)
						_, _ = channel.Write([]byte("test-host\n"))
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						_ = channel.Close()
						break
					}
				}
			}()
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("API request was not authenticated with AccessKey")
		}
		switch r.URL.Path {
		case "/api/v1/terminal/endpoints/smart/":
			if r.URL.Query().Get("protocol") != "ssh" || r.URL.Query().Get("asset_id") != "asset" {
				t.Error("SSH gateway lookup did not select the asset")
			}
			_ = json.NewEncoder(w).Encode(object{"host": address.IP.String(), "ssh_port": address.Port})
		case "/api/v1/perms/users/self/assets/asset/":
			fmt.Fprint(w, `{"permed_protocols":["ssh"],"permed_accounts":[{"alias":"svc","username":"svc"}]}`)
		case "/api/v1/authentication/connection-token/":
			var body object
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["connect_method"] != "ssh_client" || body["account"] != "svc" {
				t.Error("request did not mint a native SSH token for the selected account")
			}
			fmt.Fprint(w, `{"id":"test-token","value":"temporary-test-password","is_active":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer httpServer.Close()
	c, err := newClient(httpServer.URL, "test-ak", "test-sk")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{client: c, sshFingerprint: ssh.FingerprintSHA256(signer.PublicKey())}
	selection := sshSelection{OrgID: "org", AssetID: "asset", AccountID: "svc"}
	result, err := s.inspectNative(context.Background(), selection, "hostname")
	if err != nil || result["output"] != "test-host" || result["exitCode"] != 0 || passwordAttempts.Load() != 1 {
		t.Fatalf("unexpected native inspection response: %v, %v", result, err)
	}
	s.sshFingerprint = "SHA256:different"
	if _, err := s.inspectNative(context.Background(), selection, "hostname"); err == nil || passwordAttempts.Load() != 1 {
		t.Fatal("untrusted host reached SSH password authentication")
	}
}
