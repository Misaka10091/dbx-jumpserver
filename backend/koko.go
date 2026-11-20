package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type kokoMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

type kokoSocket struct {
	conn *websocket.Conn
	id   string
	mu   sync.Mutex
}

func (c *jmsClient) openKoko(ctx context.Context, in sshSelection, cols, rows int) (*kokoSocket, error) {
	token, err := c.mintSSH(ctx, in, "web_cli")
	if err != nil {
		return nil, err
	}
	u, origin := c.kokoURL(token)
	ws, resp, err := c.dialKoko(ctx, u, origin)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusFound {
				if c.http.Jar == nil {
					return nil, errors.New("KoKo 要求网页登录会话（HTTP 302），请将此 JumpServer 连接切换为账号密码认证")
				}
				return nil, errors.New("KoKo 未接受 JumpServer 网页会话（HTTP 302），请断开并重新连接 JumpServer 后重试")
			}
			return nil, fmt.Errorf("KoKo WebSocket 握手失败（HTTP %d），请检查 Web 终端访问权限及代理配置", resp.StatusCode)
		}
		return nil, errors.New("无法建立 KoKo WebSocket，请检查网络、证书和 JumpServer Web 终端")
	}
	ws.SetReadLimit(128 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(8 * time.Second))
	message := kokoMessage{}
	if err := ws.ReadJSON(&message); err != nil || message.Type != "CONNECT" || message.ID == "" {
		ws.Close()
		return nil, errors.New("KoKo 未确认终端初始化")
	}
	_ = ws.SetReadDeadline(time.Time{})
	socket := &kokoSocket{conn: ws, id: message.ID}
	if err := socket.resize("TERMINAL_INIT", cols, rows); err != nil {
		ws.Close()
		return nil, err
	}
	return socket, nil
}

func (c *jmsClient) dialKoko(ctx context.Context, u *url.URL, origin string) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 8 * time.Second,
		Subprotocols: []string{"JMS-KOKO"},
	}
	headers := http.Header{"Origin": {origin}}
	if c.http.Jar != nil {
		cookieURL := *u
		cookieURL.Scheme = c.base.Scheme
		cookies := make([]string, 0)
		for _, cookie := range c.http.Jar.Cookies(&cookieURL) {
			cookies = append(cookies, cookie.String())
		}
		headers.Set("Cookie", strings.Join(cookies, "; "))
	}
	return dialer.DialContext(ctx, u.String(), headers)
}

func (s *kokoSocket) send(kind string, data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := s.conn.WriteJSON(kokoMessage{ID: s.id, Type: kind, Data: data}); err != nil {
		return errors.New("KoKo 终端已断开或写入超时")
	}
	return nil
}

func (s *kokoSocket) resize(kind string, cols, rows int) error {
	if cols < 10 || cols > 500 || rows < 3 || rows > 250 {
		return errors.New("终端尺寸超出范围")
	}
	size, _ := json.Marshal(map[string]int{"cols": cols, "rows": rows})
	return s.send(kind, string(size))
}

func (s *kokoSocket) read() ([]byte, string, error) {
	kind, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, "", err
	}
	if kind == websocket.BinaryMessage {
		return data, "", nil
	}
	var message kokoMessage
	if err = json.Unmarshal(data, &message); err != nil {
		return nil, "", errors.New("KoKo 返回的终端消息格式异常")
	}
	switch message.Type {
	case "TERMINAL_DATA":
		return []byte(message.Data), "", nil
	case "CLOSE", "TERMINAL_ERROR", "ERROR", "TERMINAL_PERM_EXPIRED":
		return []byte(message.Data), "", errors.New("KoKo 终端已结束或 JumpServer 拒绝连接")
	default:
		return nil, message.Type, nil
	}
}

func (s *kokoSocket) close() { _ = s.conn.Close() }
