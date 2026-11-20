package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"
)

const ownAssets = "/perms/users/self/assets/"
const legacyAssets = "/perms/users/assets/"

type sshSelection struct {
	OrgID      string `json:"orgId"`
	AssetID    string `json:"assetId"`
	AccountID  string `json:"accountId"`
	Connection string `json:"connectionId"`
	Search     string `json:"search"`
}

type sshAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

func missingEndpoint(err error) bool {
	var status *httpStatusError
	return errors.As(err, &status) && (status.status == 404 || status.status == 405)
}

func validOrg(org string) bool {
	return safeID.MatchString(org) && org != allOrgs
}

func profileOrganizations(profile object) []object {
	for _, key := range []string{"workbench_orgs", "organizations"} {
		var organizations []object
		list, _ := profile[key].([]any)
		for _, row := range list {
			org := asObject(row)
			if id := text(org["id"]); validOrg(id) {
				organizations = append(organizations, object{"id": id, "name": text(org["name"])})
			}
		}
		if len(organizations) > 0 {
			return organizations
		}
	}
	return []object{}
}

func (s *session) organizations(ctx context.Context) ([]object, error) {
	profile, err := s.client.profile(ctx, "")
	if err != nil {
		return nil, err
	}
	orgs := profileOrganizations(profile)
	if len(orgs) != 0 {
		return orgs, nil
	}
	permissions, err := s.client.request(ctx, "GET", "/users/profile/permissions/", "", nil, nil)
	if err == nil {
		orgs = profileOrganizations(asObject(permissions))
		if len(orgs) != 0 {
			return orgs, nil
		}
	}
	if validOrg(s.orgID) {
		return []object{{"id": s.orgID, "name": "连接配置的组织"}}, nil
	}
	if err != nil && !missingEndpoint(err) {
		return nil, err
	}
	return orgs, nil
}

func supportsSSH(protocols any) bool {
	list, _ := protocols.([]any)
	if single, ok := protocols.(string); ok {
		list = []any{single}
	}
	for _, value := range list {
		name := text(value)
		if name == "" {
			name = text(asObject(value)["name"])
		}
		if name == "" {
			name = text(asObject(value)["protocol"])
		}
		if strings.EqualFold(strings.SplitN(name, "/", 2)[0], "ssh") {
			return true
		}
	}
	return false
}

func (c *jmsClient) sshAssets(ctx context.Context, in sshSelection) ([]object, error) {
	if !validOrg(in.OrgID) || len(in.Search) > 256 {
		return nil, errors.New("请选择有效组织，搜索词最多 256 字符")
	}
	query := urlValues(in.Search)
	rows, err := c.list(ctx, ownAssets, in.OrgID, query)
	if missingEndpoint(err) {
		rows, err = c.list(ctx, legacyAssets, in.OrgID, query)
	}
	if err != nil {
		return nil, err
	}
	items := []object{}
	for _, row := range rows {
		id := text(row["id"])
		category := text(row["category"])
		if category == "" {
			category = text(asObject(row["category"])["value"])
		}
		// Current list responses omit protocol metadata. Check permission in
		// the asset detail before listing accounts or minting a token.
		hasSSH := supportsSSH(row["protocols"]) || supportsSSH(row["protocol"]) || supportsSSH(row["permed_protocols"])
		if !safeID.MatchString(id) || (!hasSSH && category != "host") || row["is_active"] == false {
			continue
		}
		name, address := text(row["name"]), text(row["address"])
		if name == "" {
			name = text(row["hostname"])
		}
		if address == "" {
			address = text(row["ip"])
		}
		items = append(items, object{"id": id, "name": name, "address": address})
	}
	return items, nil
}

func (c *jmsClient) sshAccounts(ctx context.Context, in sshSelection) ([]sshAccount, bool, error) {
	if !validOrg(in.OrgID) || !safeID.MatchString(in.AssetID) {
		return nil, false, errors.New("请选择有效组织和 SSH 资产")
	}
	value, err := c.request(ctx, "GET", ownAssets+in.AssetID+"/", in.OrgID, nil, nil)
	legacy := missingEndpoint(err)
	var rows []object
	if legacy {
		rows, err = c.list(ctx, legacyAssets+in.AssetID+"/system-users/", in.OrgID, nil)
	} else if err == nil {
		detail := asObject(value)
		accounts, ok := detail["permed_accounts"].([]any)
		if !ok {
			return nil, false, errors.New("JumpServer 资产详情未返回授权账号")
		}
		if protocols, exists := detail["permed_protocols"]; exists && !supportsSSH(protocols) {
			return []sshAccount{}, false, nil
		}
		for _, account := range accounts {
			rows = append(rows, asObject(account))
		}
	}
	if err != nil {
		return nil, legacy, err
	}
	items := []sshAccount{}
	for _, row := range rows {
		id := text(row["alias"])
		if legacy {
			if !supportsSSH(row["protocol"]) {
				continue
			}
			id = text(row["id"])
		}
		username := text(row["username"])
		if id == "" || strings.HasPrefix(id, "@") || strings.HasPrefix(username, "@") || len(id) > 256 {
			continue
		}
		items = append(items, sshAccount{ID: id, Name: text(row["name"]), Username: username})
	}
	return items, legacy, nil
}

type sshToken struct {
	id     string
	secret string
	legacy bool
}

func (c *jmsClient) mintSSH(ctx context.Context, in sshSelection, connectMethod string) (sshToken, error) {
	accounts, legacy, err := c.sshAccounts(ctx, in)
	if err != nil {
		return sshToken{}, err
	}
	found := false
	for _, account := range accounts {
		if account.ID == in.AccountID {
			found = true
			break
		}
	}
	if !found {
		return sshToken{}, errors.New("该资产未授权所选 SSH 账号，请重新选择")
	}
	protocol := "ssh"
	if connectMethod == "sftp_client" {
		protocol = "sftp"
	}
	body := object{"asset": in.AssetID, "account": in.AccountID, "protocol": protocol, "connect_method": connectMethod}
	legacySecret := ""
	if legacy {
		id, err := randomUUID()
		if err != nil {
			return sshToken{}, err
		}
		var bytes [32]byte
		if _, err = rand.Read(bytes[:]); err != nil {
			return sshToken{}, err
		}
		body = object{
			"id": id, "type": "asset", "asset": in.AssetID, "system_user": in.AccountID,
			"secret":       base64.RawURLEncoding.EncodeToString(bytes[:]),
			"date_expired": c.now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
		}
	}
	value, err := c.request(ctx, "POST", "/authentication/connection-token/", in.OrgID, nil, body)
	if err != nil {
		return sshToken{}, err
	}
	token := asObject(value)
	id := text(token["id"])
	if !safeID.MatchString(id) {
		return sshToken{}, errors.New("JumpServer 未返回有效的 SSH 连接令牌")
	}
	for _, key := range []string{"is_active", "is_valid"} {
		if active, ok := token[key].(bool); ok && !active {
			return sshToken{}, errors.New("JumpServer 未激活该令牌，请检查资产登录审批及权限")
		}
	}
	secret := text(token["value"])
	if legacy {
		if returned := text(token["secret"]); returned != "" {
			secret = returned
		} else {
			secret = legacySecret
		}
	}
	if connectMethod != "web_cli" && (secret == "" || len(secret) > 4096 || strings.ContainsAny(secret, "\r\n\x00")) {
		return sshToken{}, errors.New("JumpServer 未返回有效的 SSH 客户端令牌密钥")
	}
	return sshToken{id: id, secret: secret, legacy: legacy}, nil
}

func (c *jmsClient) kokoURL(token sshToken) (*url.URL, string) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/api/v1")
	origin := u.Scheme + "://" + u.Host
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	if token.legacy {
		u.Path += "/koko/ws/token/"
		u.RawQuery = url.Values{"type": {"token"}, "target_id": {token.id}}.Encode()
	} else {
		u.Path += "/koko/ws/terminal/"
		u.RawQuery = url.Values{"token": {token.id}}.Encode()
	}
	return &u, origin
}
