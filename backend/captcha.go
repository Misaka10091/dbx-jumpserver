package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func (c *jmsClient) promptCaptcha(ctx context.Context, login *url.URL, fields map[string]string, prompt userPrompt) (string, error) {
	if prompt == nil {
		return "", errors.New("JumpServer 要求图形验证码，请在 DBX 桌面连接中输入")
	}
	if fields["captcha_0"] == "" || fields["captcha_image"] == "" {
		return "", errors.New("JumpServer 未返回完整的图形验证码，请重新测试连接")
	}
	image, err := c.captchaImage(ctx, login, fields["captcha_image"])
	if err != nil {
		return "", err
	}
	defer clear(image)
	mime := http.DetectContentType(image)
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
		if !c.inlineCaptcha {
			return showNativeCaptcha(ctx, image)
		}
		return prompt(ctx, userQuestion{
			title: "JumpServer 图形验证码",
			text:  "请输入下图中的字符。",
			echo:  true,
			image: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image),
		})
	default:
		return "", errors.New("不支持的 JumpServer 验证码图片格式")
	}
}

func (c *jmsClient) captchaImage(ctx context.Context, login *url.URL, source string) ([]byte, error) {
	imageURL, err := login.Parse(source)
	prefix := strings.TrimSuffix(login.Path, "login/") + "captcha/"
	if err != nil || imageURL.Host != login.Host || imageURL.Scheme != login.Scheme ||
		imageURL.User != nil || !strings.HasPrefix(path.Clean(imageURL.Path), prefix) {
		return nil, errors.New("JumpServer 验证码图片地址不在当前登录站点")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL.String(), nil)
	if err != nil {
		return nil, errors.New("无法创建验证码图片请求")
	}
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("无法读取 JumpServer 验证码图片")
	}
	defer resp.Body.Close()
	const maxImage = 192 << 10
	image, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(image) == 0 || len(image) > maxImage {
		return nil, errors.New("JumpServer 验证码图片不可用")
	}
	return image, nil
}
