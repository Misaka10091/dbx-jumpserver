package main

import (
	"context"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebLoginUsesEncryptedFormAndCSRFSession(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&private.PublicKey)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/core/auth/login/":
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: "jms_public_key", Value: base64.StdEncoding.EncodeToString(public), Path: "/"})
				http.SetCookie(w, &http.Cookie{Name: "jms_csrftoken", Value: "csrf-test", Path: "/"})
				fmt.Fprint(w, `<form><input value="masked-csrf" name="csrfmiddlewaretoken" type="hidden"></form>`)
				return
			}
			if r.FormValue("csrfmiddlewaretoken") != "masked-csrf" || r.FormValue("username") != "tester" {
				t.Error("login form did not include the expected fields")
			}
			value := r.FormValue("password")
			keyPart, passwordPart, ok := strings.Cut(value, ":")
			if !ok {
				t.Error("password was not encrypted")
				http.Error(w, "invalid envelope", http.StatusBadRequest)
				return
			}
			keyCipher, _ := base64.StdEncoding.DecodeString(keyPart)
			key, err := rsa.DecryptPKCS1v15(rand.Reader, private, keyCipher)
			if err != nil {
				t.Error("cannot decrypt the login envelope key")
				http.Error(w, "invalid key", http.StatusBadRequest)
				return
			}
			block, err := aes.NewCipher(key)
			password, _ := base64.StdEncoding.DecodeString(passwordPart)
			if err != nil || len(password) != aes.BlockSize {
				t.Error("invalid password envelope")
				http.Error(w, "invalid password envelope", http.StatusBadRequest)
				return
			}
			block.Decrypt(password, password)
			if strings.TrimRight(string(password), "\x00") != "测试pwd" {
				t.Error("password envelope did not round-trip")
			}
			http.Redirect(w, r, "/core/auth/login/guard/", http.StatusFound)
		case "/core/auth/login/guard/":
			http.SetCookie(w, &http.Cookie{Name: "jms_sessionid", Value: "web-test", Path: "/", HttpOnly: true})
			http.Redirect(w, r, "/ui/", http.StatusFound)
		case "/api/v1/users/profile/":
			if cookie, _ := r.Cookie("jms_sessionid"); cookie == nil || cookie.Value != "web-test" {
				t.Error("session cookie not bound to the API")
			}
			fmt.Fprint(w, `{"username":"tester"}`)
		case "/api/v1/authentication/connection-token/":
			if r.Header.Get("X-CSRFToken") != "csrf-test" || r.Header.Get("Origin") == "" {
				t.Error("token creation missing CSRF protection")
			}
			fmt.Fprint(w, `{"id":"ssh-token"}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newClientWithAuth(server.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{client: client, terminalWeb: client, loginUsername: "tester", loginPassword: "测试pwd"}
	if err := s.authenticate(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if s.loginPassword != "" {
		t.Fatal("authenticated session kept password in memory")
	}
	if _, err := client.request(context.Background(), http.MethodPost, "/authentication/connection-token/", "org", nil, object{}); err != nil {
		t.Fatal(err)
	}
}

func TestWebLoginRejectsRedirectToAnotherSite(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("login followed a redirect to another origin")
	}))
	defer target.Close()
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<form><input name="csrfmiddlewaretoken" value="csrf-test"></form>`)
		} else {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}
	}))
	defer login.Close()

	client, err := newClientWithAuth(login.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.webLogin(context.Background(), "tester", "example", nil); err == nil {
		t.Fatal("cross-origin login was accepted")
	}
}

func TestMCPRequiresOptInOnTheBoundConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("MCP made a request for a connection without opt-in")
		http.Error(w, "unexpected request", http.StatusForbidden)
	}))
	defer server.Close()
	request, err := json.Marshal(object{
		"tool":      "ssh_list_organizations",
		"arguments": object{"connectionId": "some-other-connection", "mcp_read": true},
		"lifecycle": object{"connection": object{
			"id": "bound-connection", "host": server.URL,
			"connection_secrets": object{"access_key": "test-id", "secret_key": "test-key"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := newPlugin().callMCP(context.Background(), request)
	if err != nil || asObject(result)["isError"] != true {
		t.Fatalf("unexpected MCP authorization response: %v, %v", result, err)
	}
}
