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
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
)

type userQuestion struct {
	title, text string
	options     []object
	image       string
	echo        bool
}

type userPrompt func(context.Context, userQuestion) (string, error)

type hostPrompter struct {
	mu      sync.Mutex
	pending map[string]chan promptReply
	write   func(any)
}

type promptReply struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

func newHostPrompter(write func(any)) *hostPrompter {
	return &hostPrompter{pending: make(map[string]chan promptReply), write: write}
}

func (h *hostPrompter) accept(line []byte) bool {
	var response struct {
		ID     string      `json:"id"`
		Method string      `json:"method"`
		Result promptReply `json:"result"`
	}
	if json.Unmarshal(line, &response) != nil || response.Method != "" || !strings.HasPrefix(response.ID, "jms-input-") {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch := h.pending[response.ID]; ch != nil {
		select {
		case ch <- response.Result:
		default:
		}
	}
	return true
}

func (h *hostPrompter) prompt(ctx context.Context, question userQuestion) (string, error) {
	id, err := randomUUID()
	if err != nil {
		return "", errors.New("无法创建 JumpServer 认证提示")
	}
	id = "jms-input-" + id
	answer := make(chan promptReply, 1)
	h.mu.Lock()
	h.pending[id] = answer
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()
	params := object{"title": question.title, "prompt": question.text, "echo": question.echo, "timeoutSecs": 80}
	if question.image != "" {
		params["image"] = question.image
	}
	if len(question.options) > 0 {
		params["options"] = question.options
	}
	h.write(object{"jsonrpc": "2.0", "id": id, "method": "host/requestUserInput", "params": params})
	select {
	case result := <-answer:
		value := strings.TrimSpace(result.Value)
		if result.Action == "submit" && value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00") {
			return value, nil
		}
	case <-ctx.Done():
	}
	return "", errors.New("JumpServer 认证输入已取消、不可用或超时")
}

var otpCode = regexp.MustCompile(`^[0-9]{6,8}$`)

func formFields(body io.Reader) (map[string]string, error) {
	tokenizer := html.NewTokenizer(body)
	tokenizer.SetMaxBuf(1 << 20)
	fields := map[string]string{}
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if tokenizer.Err() != io.EOF {
				return nil, errors.New("无法读取 JumpServer 登录表单")
			}
			return fields, nil
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		node := tokenizer.Token()
		if node.Data == "img" {
			var src, class string
			for _, attr := range node.Attr {
				switch attr.Key {
				case "src":
					src = attr.Val
				case "class":
					class = attr.Val
				}
			}
			for _, className := range strings.Fields(class) {
				if className == "captcha" && fields["captcha_image"] == "" {
					fields["captcha_image"] = src
				}
			}
			continue
		}
		if node.Data != "input" && node.Data != "option" && node.Data != "select" {
			continue
		}
		var name, value string
		for _, attr := range node.Attr {
			switch attr.Key {
			case "name":
				name = attr.Val
			case "value":
				value = attr.Val
			}
		}
		if name != "" {
			fields[name] = value
		}
		if node.Data == "option" && value == "otp" {
			fields["otp_option"] = value
		}
	}
}

func jumpserverPassword(password, cookie string) (string, error) {
	if cookie == "" {
		// The upstream login page also submits over HTTPS directly when
		// this deployment does not supply its optional envelope key.
		return password, nil
	}
	cookie, err := url.PathUnescape(strings.Trim(cookie, "\""))
	if err != nil {
		return "", errors.New("JumpServer 登录公钥格式无效")
	}
	encoded, err := base64.StdEncoding.DecodeString(cookie)
	if err != nil {
		return "", errors.New("JumpServer 登录公钥格式无效")
	}
	if block, _ := pem.Decode(encoded); block != nil {
		encoded = block.Bytes
	}
	publicKey, err := x509.ParsePKCS1PublicKey(encoded)
	if err != nil {
		parsed, parseErr := x509.ParsePKIXPublicKey(encoded)
		if parseErr == nil {
			publicKey, _ = parsed.(*rsa.PublicKey)
		}
	}
	if publicKey == nil {
		return "", errors.New("JumpServer 登录公钥不是受支持的 RSA 公钥")
	}
	var randomKey [12]byte
	if _, err := rand.Read(randomKey[:]); err != nil {
		return "", errors.New("无法初始化 JumpServer 登录加密")
	}
	key := base64.RawURLEncoding.EncodeToString(randomKey[:])
	encryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, publicKey, []byte(key))
	if err != nil {
		return "", errors.New("无法用 JumpServer 公钥加密登录数据")
	}
	block, _ := aes.NewCipher([]byte(key))
	padded := make([]byte, (len(password)+aes.BlockSize-1)/aes.BlockSize*aes.BlockSize)
	copy(padded, password)
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(padded[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(encryptedKey) + ":" + base64.StdEncoding.EncodeToString(padded), nil
}

func (c *jmsClient) webLogin(ctx context.Context, username, password string, prompt userPrompt) error {
	if len(password) > 4096 {
		return errors.New("JumpServer 登录密码超出长度限制")
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return errors.New("无法初始化 JumpServer 登录会话")
	}
	c.http.Jar = jar
	base := *c.base
	base.Path = strings.TrimSuffix(base.Path, "/api/v1")
	authPrefix := base.Path + "/core/auth/"
	login := base
	login.Path = authPrefix + "login/"
	response, err := c.authForm(ctx, &login, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return errors.New("JumpServer 登录入口要求网页认证，请检查该账号是否允许密码登录")
	}
	fields, err := formFields(io.LimitReader(response.Body, 2<<20))
	response.Body.Close()
	if err != nil {
		return err
	}
	response, err = c.submitLogin(ctx, &login, fields, username, password, prompt)
	if err != nil {
		return err
	}
	// A deployment can show a newly required challenge after the first
	// submission. Only retry once, and only after the user supplies it.
	if response.StatusCode == http.StatusOK && !loginHasChallenge(fields) {
		nextFields, readErr := formFields(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if !loginHasChallenge(nextFields) {
			return errors.New("JumpServer 未接受登录表单，请检查账号密码或网页登录限制")
		}
		response, err = c.submitLogin(ctx, &login, nextFields, username, password, prompt)
		if err != nil {
			return err
		}
	}
	mfaDone := false
	for i := 0; i < 6; i++ {
		next, redirectErr := response.Location()
		status := response.StatusCode
		response.Body.Close()
		if status != http.StatusFound && status != http.StatusSeeOther {
			return errors.New("JumpServer 未接受登录表单，请核对账号密码和验证码后重新测试")
		}
		if redirectErr != nil || next.Host != login.Host || next.Scheme != login.Scheme || next.User != nil {
			return errors.New("JumpServer 要求通过其他站点认证，请使用网页登录")
		}
		switch next.Path {
		case authPrefix + "login/guard/":
			response, err = c.authForm(ctx, next, nil)
		case authPrefix + "login/mfa/":
			if prompt == nil || mfaDone {
				return errors.New("JumpServer 要求动态验证码，请在支持交互验证的 DBX 桌面连接中完成登录")
			}
			mfaDone = true
			response, err = c.submitOTP(ctx, next, prompt)
		case authPrefix + "login/wait-confirm/":
			return errors.New("JumpServer 要求登录审批，请在网页完成审批")
		case login.Path:
			return errors.New("JumpServer 未建立登录会话")
		default:
			// The profile request immediately after this verifies the cookie
			// and the user. Never follow arbitrary redirects from login.
			return nil
		}
		if err != nil {
			return err
		}
	}
	response.Body.Close()
	return errors.New("JumpServer 登录认证步骤超出限制")
}

func loginHasChallenge(fields map[string]string) bool {
	_, captcha := fields["captcha_0"]
	_, challenge := fields["challenge"]
	_, code := fields["code"]
	return captcha || challenge || code
}

func promptOTP(ctx context.Context, prompt userPrompt) (string, error) {
	if prompt == nil {
		return "", errors.New("JumpServer 要求动态验证码，请先在 DBX 桌面端登录")
	}
	answer, err := prompt(ctx, userQuestion{title: "JumpServer 双因素认证", text: "请输入身份验证器中的 JumpServer 动态验证码。"})
	if err != nil {
		return "", err
	}
	if !otpCode.MatchString(answer) {
		return "", errors.New("JumpServer 动态验证码格式无效")
	}
	return answer, nil
}

func (c *jmsClient) submitLogin(ctx context.Context, login *url.URL, fields map[string]string, username, password string, prompt userPrompt) (*http.Response, error) {
	token := fields["csrfmiddlewaretoken"]
	if token == "" {
		return nil, errors.New("JumpServer 未返回密码登录表单")
	}
	values := url.Values{"csrfmiddlewaretoken": {token}, "username": {username}}
	if _, ok := fields["captcha_0"]; ok {
		answer, err := c.promptCaptcha(ctx, login, fields, prompt)
		if err != nil {
			return nil, err
		}
		values.Set("captcha_0", fields["captcha_0"])
		values.Set("captcha_1", answer)
	}
	if _, ok := fields["challenge"]; ok {
		answer, err := promptOTP(ctx, prompt)
		if err != nil {
			return nil, err
		}
		values.Set("challenge", answer)
	}
	if _, ok := fields["code"]; ok {
		mfaType := fields["mfa_type"]
		if mfaType != "otp" && fields["otp_option"] != "otp" {
			return nil, errors.New("当前双因素认证方式需要在 JumpServer 网页完成")
		}
		answer, err := promptOTP(ctx, prompt)
		if err != nil {
			return nil, err
		}
		values.Set("mfa_type", "otp")
		values.Set("code", answer)
	}
	publicKey := ""
	for _, cookie := range c.http.Jar.Cookies(login) {
		if cookie.Name == "jms_public_key" {
			publicKey = cookie.Value
			break
		}
	}
	encrypted, err := jumpserverPassword(password, publicKey)
	if err != nil {
		return nil, err
	}
	values.Set("password", encrypted)
	return c.authForm(ctx, login, values)
}

func (c *jmsClient) submitOTP(ctx context.Context, endpoint *url.URL, prompt userPrompt) (*http.Response, error) {
	response, err := c.authForm(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("无法读取 JumpServer 双因素认证表单")
	}
	fields, err := formFields(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	token := fields["csrfmiddlewaretoken"]
	if token == "" || fields["captcha_0"] != "" {
		return nil, errors.New("JumpServer 双因素认证需要在网页完成")
	}
	mfaType := fields["mfa_type"]
	if mfaType != "" && mfaType != "otp" {
		return nil, errors.New("当前双因素认证方式需要在网页完成")
	}
	code, err := promptOTP(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return c.authForm(ctx, endpoint, url.Values{"csrfmiddlewaretoken": {token}, "mfa_type": {"otp"}, "code": {code}})
}

func (c *jmsClient) authForm(ctx context.Context, endpoint *url.URL, fields url.Values) (*http.Response, error) {
	method := http.MethodGet
	var data io.Reader
	if fields != nil {
		method = http.MethodPost
		data = strings.NewReader(fields.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), data)
	if err != nil {
		return nil, errors.New("无法建立 JumpServer 登录请求")
	}
	req.Header.Set("Accept", "text/html")
	if fields != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", endpoint.Scheme+"://"+endpoint.Host)
		req.Header.Set("Referer", endpoint.String())
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("无法访问 JumpServer 登录服务，请检查网络和 HTTPS 证书")
	}
	return response, nil
}
