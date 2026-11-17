package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenExpirySupportsJumpServerDateFormats(t *testing.T) {
	for _, value := range []string{
		"2026-11-17T08:00:00Z",
		"2026-11-17T16:00:00+08:00",
		"2026/11/17 16:00:00 +0800",
		"2026/11/17 16:00:00 +08:00",
		"2026-11-17 16:00:00 +0800",
		"2026-11-17 16:00:00 +08:00",
		"2026-11-17T16:00:00+0800",
		"2026-11-17 16:00:00+08:00",
		" 2026/11/17 16:00:00 +0800 ",
	} {
		t.Run(value, func(t *testing.T) {
			parsed, err := parseTokenExpiry(value)
			if err != nil || parsed.UTC().Format(time.RFC3339) != "2026-11-17T08:00:00Z" {
				t.Fatal("incorrect timezone conversion", err)
			}
		})
	}
	parsed, err := parseTokenExpiry("2026/11/17 16:00:00.123456 +0800")
	if err != nil || parsed.Nanosecond() != 123456000 {
		t.Fatal("fractional seconds were lost", err)
	}
	for _, value := range []string{"2026/11/17 16:00:00", "invalid", "2026/99/99 16:00:00 +0800"} {
		if _, err := parseTokenExpiry(value); err == nil {
			t.Fatal("accepted invalid or ambiguous expiry")
		}
	}
}

func TestMintAcceptsLocalizedExpiryAndNormalizesForUI(t *testing.T) {
	s, _ := testSession(t, "mysql", apiScenario{expiry: "2026/11/17 16:00:00 +0800"})
	credentials, err := s.mint(context.Background(), validSelection())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.ExpiresAt != "2026-11-17T08:00:00Z" {
		t.Fatal("UI must receive an unambiguous ISO timestamp")
	}
	if credentials.Host != "proxy.test" || credentials.Port != 33061 {
		t.Fatal("connection endpoint did not come from smart API")
	}
}

func TestExpiredAndUnparseableTokensHaveDifferentErrors(t *testing.T) {
	for _, tc := range []struct{ value, contains string }{
		{"2026/11/17 14:00:00 +0800", "已过期"},
		{"unrecognized-date", "格式不受支持"},
	} {
		s, _ := testSession(t, "mysql", apiScenario{expiry: tc.value})
		if _, err := s.mint(context.Background(), validSelection()); err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatal("wrong expiry failure classification", err)
		}
	}
}

func TestSmartEndpointUsesApplicationAndAssetQueryContracts(t *testing.T) {
	for _, tc := range []struct{ kind, key, protocol string }{
		{"application", "app_id", "mysql"},
		{"asset", "asset_id", "postgresql"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				if r.Method != "GET" || r.URL.Path != "/api/v1/terminal/endpoints/smart/" || query.Get(tc.key) != "resource-id" || query.Get("protocol") != tc.protocol || len(query) != 2 {
					t.Error("wrong smart endpoint request")
				}
				if r.Header.Get("X-JMS-ORG") != "org-id" || r.Header.Get("Authorization") == "" {
					t.Error("smart endpoint must be signed and scoped to the selected organization")
				}
				_ = json.NewEncoder(w).Encode(object{"host": "[2001:db8::1]", "magnus_listen_db_port": 33061, "http_port": 80, "ssh_port": 2222})
			}))
			defer server.Close()
			client, _ := newClient(server.URL, "ak", "sk")
			host, port, err := client.databaseEndpoint(context.Background(), "org-id", tc.kind, "resource-id", tc.protocol)
			if err != nil || host != "2001:db8::1" || port != 33061 {
				t.Fatal("wrong database endpoint; HTTP/SSH ports must not be used", err)
			}
		})
	}
}

func TestInvalidSmartEndpointDoesNotCreateToken(t *testing.T) {
	for _, scenario := range []apiScenario{
		{endpoint: object{"host": "proxy.test", "magnus_listen_db_port": 0, "ssh_port": 2222}},
		{endpoint: object{"host": "", "magnus_listen_db_port": 33061}},
		{endpoint: object{"host": "proxy.test", "magnus_listen_db_port": -1}},
		{smartStatus: 403},
	} {
		s, bodies := testSession(t, "mysql", scenario)
		if _, err := s.mint(context.Background(), validSelection()); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
		if len(*bodies) != 0 {
			t.Fatal("token created before smart endpoint validation")
		}
	}
}

func TestLegacyUsernameTemplateDoesNotAlterDatabaseTokenID(t *testing.T) {
	raw := json.RawMessage(`{"connection":{"id":"service","host":"https://jumpserver.example.com","external_config":{"username_template":"JMS-{id}"},"connection_secrets":{"access_key":"ak","secret_key":"sk"}}}`)
	_, saved, err := parseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	mock, bodies := testSession(t, "mysql")
	saved.client = mock.client
	credentials, err := saved.mint(context.Background(), validSelection())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != text((*bodies)[0]["id"]) || strings.HasPrefix(credentials.Username, "JMS-") {
		t.Fatal("an old saved username template must not prefix the database token ID")
	}
}

func TestCredentialServiceDoesNotRequireGatewaySettings(t *testing.T) {
	raw := json.RawMessage(`{"connection":{"id":"service","host":"https://jumpserver.example.com","external_config":{},"connection_secrets":{"access_key":"ak","secret_key":"sk"}}}`)
	id, _, err := parseSession(raw)
	if err != nil || id != "service" {
		t.Fatal("API profile must not require a manual database endpoint", err)
	}
}
