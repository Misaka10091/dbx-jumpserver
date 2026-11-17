//go:build windows

package main

import (
	"errors"
	"runtime"
	"strings"
	"syscall"
)

// ShellExecuteW dispatches the local DBX URL without a shell or command line.
func openDBX(link string) error {
	if !strings.HasPrefix(link, "dbx://connection/new?") {
		return errors.New("invalid DBX URL")
	}
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	shell := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	result, _, _ := shell.Call(0, uintptrFromUTF16(verb), uintptrFromUTF16(target), 0, 0, 1)
	runtime.KeepAlive(verb)
	runtime.KeepAlive(target)
	if result <= 32 {
		return errors.New("DBX protocol handler unavailable")
	}
	return nil
}
