//go:build !windows && (!darwin || !cgo)

package main

import (
	"context"
	"errors"
	"os/exec"
)

func nativeCaptchaCommand(context.Context) (*exec.Cmd, error) {
	return nil, errors.New("此平台需要使用支持内嵌图形验证码的 DBX 客户端")
}

func enterNativeCaptchaMode() {}
