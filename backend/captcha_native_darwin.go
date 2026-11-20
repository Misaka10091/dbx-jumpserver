//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
char *dbx_show_captcha(const void *image, size_t len, int *status);
*/
import "C"

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
	"unsafe"
)

func init() {
	// Cocoa requires the initial thread, including the code that creates
	// NSApplication. The ordinary plugin process releases it in main.
	runtime.LockOSThread()
}

func nativeCaptchaCommand(ctx context.Context) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, executable, "--captcha-dialog"), nil
}

func enterNativeCaptchaMode() {
	if len(os.Args) != 2 || os.Args[1] != "--captcha-dialog" {
		runtime.UnlockOSThread()
		return
	}
	stop := time.AfterFunc(80*time.Second, func() { os.Exit(3) })
	defer stop.Stop()
	data, err := io.ReadAll(io.LimitReader(os.Stdin, (192<<10)+1))
	if err != nil || len(data) == 0 || len(data) > 192<<10 {
		os.Exit(4)
	}
	var status C.int
	answer := C.dbx_show_captcha(unsafe.Pointer(&data[0]), C.size_t(len(data)), &status)
	clear(data)
	if answer != nil {
		fmt.Fprint(os.Stdout, C.GoString(answer))
		C.free(unsafe.Pointer(answer))
	}
	os.Exit(int(status))
}
