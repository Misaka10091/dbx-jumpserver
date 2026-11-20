package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSignatureUsesExactEncodedRequestAndGMT(t *testing.T) {
	u := "https://example.test/api/v1/perms/users/applications/?" + url.Values{"search": {"数据库 + /"}, "offset": {"100"}}.Encode()
	req, _ := http.NewRequest("GET", u, nil)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.FixedZone("CST", 8*3600))
	signHeaders(req, "ak", "secret", "org", now)
	canonical := "(request-target): get " + req.URL.RequestURI() + "\naccept: application/json\ndate: Tue, 17 Nov 2026 07:00:00 GMT"
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(canonical))
	want := `Signature keyId="ak",algorithm="hmac-sha256",headers="(request-target) accept date",signature="` + base64.StdEncoding.EncodeToString(mac.Sum(nil)) + `"`
	if req.Header.Get("Authorization") != want {
		t.Fatal("signature does not cover the exact URI and RFC 1123 UTC date")
	}
	if req.Header.Get("Accept") != "application/json" || req.Header.Get("X-JMS-ORG") != "org" {
		t.Fatal("missing signed accept or organization")
	}
}

func TestClientPathNormalizationAndRedirectRefusal(t *testing.T) {
	for _, path := range []string{"", "/api", "/api/v1", "/api/v1/"} {
		c, err := newClient("https://example.test"+path, "ak", "sk")
		if err != nil {
			t.Fatal(err)
		}
		if c.base.Path != "/api/v1" {
			t.Fatal(c.base.Path)
		}
	}
	for _, endpoint := range []string{"http://example.test", "https://ak:sk@example.test", "https://example.test?secret=x", "file:///tmp/api"} {
		if _, err := newClient(endpoint, "ak", "sk"); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	followed := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true; fmt.Fprint(w, `{}`) }))
	defer dest.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer source.Close()
	c, _ := newClient(source.URL, "ak", "sk")
	_, err := c.profile(context.Background(), "")
	if err == nil || followed {
		t.Fatal("redirect was followed with signed credentials")
	}
}

func TestPaginationSupportsArraysWrappedAssetsAndUntrustedNext(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-JMS-ORG") != "org" {
			t.Error("organization omitted")
		}
		if r.URL.Query().Get("offset") == "0" {
			fmt.Fprint(w, `{"count":101,"next":"https://attacker.invalid/steal","results":[{"asset":{"id":"first","name":"db"}}]}`)
		} else {
			fmt.Fprint(w, `{"count":101,"next":null,"results":[{"asset":{"id":"last"}}]}`)
		}
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "ak", "sk")
	items, err := c.list(context.Background(), "/perms/users/assets/", "org", nil)
	if err != nil || len(items) != 2 || items[0]["id"] != "first" || requests != 2 {
		t.Fatalf("unexpected list: %#v %v requests=%d", items, err, requests)
	}
	array := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[{"id":"account"}]`) }))
	defer array.Close()
	c, _ = newClient(array.URL, "ak", "sk")
	items, err = c.list(context.Background(), "/accounts/", "org", nil)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
}

func TestRepeatedPaginationFailsInsteadOfLooping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"count":1000,"results":[{"id":"same"}]}`)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "ak", "sk")
	if _, err := c.list(context.Background(), "/assets/", "org", nil); err == nil {
		t.Fatal("duplicate pages accepted")
	}
}

type apiScenario struct {
	expiry      string
	endpoint    object
	smartStatus int
}

func testSession(t *testing.T, accountProtocol string, scenarios ...apiScenario) (*session, *[]object) {
	t.Helper()
	var scenario apiScenario
	if len(scenarios) != 0 {
		scenario = scenarios[0]
	}
	bodies := []object{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-JMS-ORG") != "org-1" {
			t.Error("wrong organization")
		}
		switch r.URL.Path {
		case "/api/v1/perms/users/applications/":
			fmt.Fprint(w, `{"count":1,"results":[{"id":"db-1","name":"订单数据库","type":"mysql"}]}`)
		case "/api/v1/perms/users/applications/db-1/system-users/":
			fmt.Fprintf(w, `[{"id":"account-1","username":"readonly","protocol":%q}]`, accountProtocol)
		case "/api/v1/terminal/endpoints/smart/":
			if r.URL.Query().Get("protocol") != accountProtocol || r.URL.Query().Get("app_id") != "db-1" || r.URL.Query().Has("asset_id") {
				t.Error("smart endpoint must use the selected application's ID and account protocol")
			}
			if scenario.smartStatus != 0 {
				w.WriteHeader(scenario.smartStatus)
				return
			}
			endpoint := scenario.endpoint
			if endpoint == nil {
				endpoint = object{"host": "proxy.test", "magnus_listen_db_port": "33061", "ssh_port": 2222}
			}
			_ = json.NewEncoder(w).Encode(endpoint)
		case "/api/v1/authentication/connection-token/":
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
				t.Error("invalid token method or content type")
			}
			var body object
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodies = append(bodies, body)
			expiry := body["date_expired"]
			if scenario.expiry != "" {
				expiry = scenario.expiry
			}
			_ = json.NewEncoder(w).Encode(object{"id": body["id"], "date_expired": expiry, "is_valid": true})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	c, _ := newClient(server.URL, "ak", "sk")
	c.now = func() time.Time { return time.Date(2026, 11, 17, 15, 0, 0, 0, time.FixedZone("CST", 8*3600)) }
	return &session{client: c, username: "alice", expireMinutes: 60}, &bodies
}
func validSelection() selection {
	return selection{OrgID: "org-1", Kind: "application", ResourceID: "db-1", AccountID: "account-1"}
}

func TestMintVerifiesPermissionsAndUsesUTCWithRandomSecrets(t *testing.T) {
	s, bodies := testSession(t, "mysql")
	c, err := s.mint(context.Background(), validSelection())
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiresAt != "2026-11-17T08:00:00Z" {
		t.Fatal(c.ExpiresAt)
	}
	body := (*bodies)[0]
	if body["type"] != "application" || body["application"] != "db-1" || body["system_user"] != "account-1" || body["asset"] != nil {
		t.Fatal("incorrect resource type", body)
	}
	if c.Username != text(body["id"]) || len(c.Password) < 32 || c.Host != "proxy.test" {
		t.Fatal("incorrect proxy credentials")
	}
	next, err := s.mint(context.Background(), validSelection())
	if err != nil {
		t.Fatal(err)
	}
	if next.Username == c.Username || next.Password == c.Password {
		t.Fatal("credentials were reused")
	}
	in := validSelection()
	in.AccountID = "revoked"
	if _, err = s.mint(context.Background(), in); err == nil {
		t.Fatal("revoked account accepted")
	}
	if len(*bodies) != 2 {
		t.Fatal("token was minted for a revoked account")
	}
}

func TestProtocolMismatchDoesNotMintToken(t *testing.T) {
	s, bodies := testSession(t, "mysql")
	in := validSelection()
	in.TargetID = "saved"
	in.TargetType = "postgres"
	if _, err := s.mint(context.Background(), in); err == nil {
		t.Fatal("mismatched database type accepted")
	}
	if len(*bodies) != 0 {
		t.Fatal("token created before target validation")
	}
}

func TestNewConnectionLinkEncodesFields(t *testing.T) {
	c := credentials{Host: "proxy.test", Port: 33061, Username: "token id +&", Password: " secret+&= ", Type: "mysql", Name: "Orders"}
	link, err := newConnectionLink(c)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "dbx" || u.Host != "connection" || u.Path != "/new" || q.Has("id") {
		t.Fatal("new-connection link invalid")
	}
	if q.Get("type") != c.Type || q.Get("host") != c.Host || q.Get("port") != "33061" || q.Get("name") != "Orders · JumpServer" || q.Has("database") {
		t.Fatal("new-connection fields were not encoded correctly")
	}
	if q.Get("password") != c.Password || q.Get("user") != c.Username {
		t.Fatal("credentials were trimmed or incorrectly encoded")
	}
}

func TestErrorsNeverEchoServerSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"secret":"DO_NOT_LEAK"}`)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "ak", "sk")
	_, err := c.profile(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "DO_NOT_LEAK") {
		t.Fatal("server error leaked secrets")
	}
}

func TestRPCInitializationAndMissingSession(t *testing.T) {
	input := strings.NewReader("{bad}\n" + `{"jsonrpc":"2.0","id":1,"method":"plugin/initialize","params":{"host":{"protocolVersions":[1]}}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"jumpserver/profile","params":{"connectionId":"missing"}}` + "\n")
	var output bytes.Buffer
	if err := serve(input, &output, newPlugin()); err != nil {
		t.Fatal(err)
	}
	scanner := json.NewDecoder(&output)
	results := map[string]object{}
	for scanner.More() {
		var row object
		if err := scanner.Decode(&row); err != nil {
			t.Fatal(err)
		}
		results[fmt.Sprint(row["id"])] = row
	}
	result := asObject(results["1"]["result"])
	if text(asObject(result["plugin"])["id"]) != pluginID {
		t.Fatal("sidecar identity mismatch")
	}
	if result["capabilities"] == nil {
		t.Fatal("capabilities must be an array")
	}
	if asObject(results["2"]["error"]) == nil {
		t.Fatal("disconnected session accepted")
	}
}

func TestFillOpensOnlyGeneratedLocalDBXURL(t *testing.T) {
	s, bodies := testSession(t, "mysql")
	p := newPlugin()
	p.sessions["profile"] = s
	opened := ""
	p.open = func(link string) error { opened = link; return nil }
	in := validSelection()
	in.ConnectionID = "profile"
	in.TargetID = "saved-id"
	in.TargetType = "mysql"
	raw, _ := json.Marshal(in)
	if _, err := p.handle(context.Background(), "jumpserver/fill", raw); err == nil || opened != "" || len(*bodies) != 0 {
		t.Fatal("existing connection must use the host editor")
	}
	in.TargetID = ""
	raw, _ = json.Marshal(in)
	result, err := p.handle(context.Background(), "jumpserver/fill", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(opened, "dbx://connection/new?") {
		t.Fatal("non DBX URL opened")
	}
	openedURL, err := url.Parse(opened)
	if err != nil {
		t.Fatal(err)
	}
	query := openedURL.Query()
	if len(*bodies) != 1 || query.Get("user") != text((*bodies)[0]["id"]) {
		t.Fatal("DBX must receive the raw ConnectionToken ID as its username")
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("password")) || bytes.Contains(encoded, []byte(query.Get("user"))) || bytes.Contains(encoded, []byte(query.Get("password"))) {
		t.Fatal("fill result exposed credentials")
	}
}
