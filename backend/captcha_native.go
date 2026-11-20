package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os/exec"
	"strings"
	"time"
)

func showNativeCaptcha(ctx context.Context, data []byte) (string, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 1024 {
		return "", errors.New("无法显示 JumpServer 验证码图片")
	}
	ctx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	cmd, err := nativeCaptchaCommand(ctx)
	if err != nil {
		return "", err
	}
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	defer clear(out)
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("JumpServer 验证码输入已超时或取消")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return "", errors.New("已取消 JumpServer 验证码输入")
		}
		return "", errors.New("无法打开 JumpServer 验证码输入弹窗")
	}
	answer := strings.TrimSpace(string(out))
	if answer == "" || len(answer) > 128 || strings.ContainsAny(answer, "\r\n\x00") {
		return "", errors.New("请输入有效的 JumpServer 验证码")
	}
	return answer, nil
}
