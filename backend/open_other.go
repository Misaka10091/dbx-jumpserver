//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

func openDBX(link string) error {
	if !strings.HasPrefix(link, "dbx://connection/new?") {
		return errors.New("invalid DBX URL")
	}
	return openLocal(link)
}

func openLocal(link string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	// The URL is one argument; no shell expansion or downloaded program is used.
	return exec.Command(command, link).Run()
}
