package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	captchaOK      = 1 // IDOK
	captchaCancel  = 2 // IDCANCEL
	captchaEdit    = 1001
	captchaPreview = 1002

	wmClose      = 0x0010
	wmInitDialog = 0x0110
	wmCommand    = 0x0111
	emSetLimit   = 0x00c5
	stmSetImage  = 0x0172
)

var (
	captchaUser32 = windows.NewLazySystemDLL("user32.dll")
	captchaGDI32  = windows.NewLazySystemDLL("gdi32.dll")

	captchaDialogBox      = captchaUser32.NewProc("DialogBoxIndirectParamW")
	captchaEndDialog      = captchaUser32.NewProc("EndDialog")
	captchaSendDlgItem    = captchaUser32.NewProc("SendDlgItemMessageW")
	captchaGetDlgItem     = captchaUser32.NewProc("GetDlgItem")
	captchaGetDlgItemText = captchaUser32.NewProc("GetDlgItemTextW")
	captchaGetClientRect  = captchaUser32.NewProc("GetClientRect")
	captchaSetForeground  = captchaUser32.NewProc("SetForegroundWindow")
	captchaCreateDIB      = captchaGDI32.NewProc("CreateDIBSection")
	captchaDeleteBitmap   = captchaGDI32.NewProc("DeleteObject")
)

func nativeCaptchaCommand(ctx context.Context) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "--captcha-dialog")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd, nil
}

func enterNativeCaptchaMode() {
	if len(os.Args) == 2 && os.Args[1] == "--captcha-dialog" {
		os.Exit(runWindowsCaptchaDialog())
	}
}

func runWindowsCaptchaDialog() int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	timeout := time.AfterFunc(80*time.Second, func() { os.Exit(3) })
	defer timeout.Stop()

	const maxImage = 192 << 10
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxImage+1))
	defer clear(raw)
	if err != nil || len(raw) > maxImage {
		return 4
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 1024 {
		return 4
	}
	picture, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return 4
	}
	var instance windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &instance); err != nil {
		return 5
	}

	var bitmap uintptr
	var pixels []byte
	var answer [129]uint16
	defer clear(answer[:])
	defer func() {
		if bitmap != 0 {
			clear(pixels)
			captchaDeleteBitmap.Call(bitmap)
		}
	}()
	callback := syscall.NewCallback(func(hwnd, message, wParam, _ uintptr) uintptr {
		switch message {
		case wmInitDialog:
			preview, _, _ := captchaGetDlgItem.Call(hwnd, captchaPreview)
			bitmap, pixels = createWindowsCaptchaBitmap(preview, picture)
			if bitmap == 0 {
				captchaEndDialog.Call(hwnd, 5)
				return 1
			}
			// IMAGE_BITMAP is zero. The static control centers the in-memory bitmap.
			captchaSendDlgItem.Call(hwnd, captchaPreview, stmSetImage, 0, bitmap)
			captchaSendDlgItem.Call(hwnd, captchaEdit, emSetLimit, 128, 0)
			captchaSetForeground.Call(hwnd)
			return 1 // Let Windows focus the first tab stop, the edit control.
		case wmCommand:
			switch uint16(wParam) {
			case captchaOK:
				captchaGetDlgItemText.Call(hwnd, captchaEdit, uintptr(unsafe.Pointer(&answer[0])), uintptr(len(answer)))
				if strings.TrimSpace(windows.UTF16ToString(answer[:])) != "" {
					captchaEndDialog.Call(hwnd, captchaOK)
				}
				return 1
			case captchaCancel:
				captchaEndDialog.Call(hwnd, captchaCancel)
				return 1
			}
		case wmClose:
			captchaEndDialog.Call(hwnd, captchaCancel)
			return 1
		}
		return 0
	})
	template := windowsCaptchaTemplate()
	result, _, _ := captchaDialogBox.Call(uintptr(instance), uintptr(unsafe.Pointer(&template[0])), 0, callback, 0)
	switch result {
	case captchaOK:
		_, err := fmt.Fprint(os.Stdout, windows.UTF16ToString(answer[:]))
		if err != nil {
			return 5
		}
		return 0
	case captchaCancel:
		return 3
	default:
		return 5
	}
}

func windowsCaptchaTemplate() []uint16 {
	const (
		wsChildVisible = 0x50000000
		wsTabStop      = 0x00010000
		wsBorder       = 0x00800000
		buttonClass    = 0x0080
		editClass      = 0x0081
		staticClass    = 0x0082
	)
	// Standard DLGTEMPLATE words: style, extended style, item count and bounds.
	// WS_POPUP | WS_CAPTION | WS_SYSMENU | DS_CENTER | DS_SETFONT | DS_MODALFRAME.
	// WS_EX_TOPMOST keeps the prompt visible over the DBX window.
	template := []uint16{0x08c0, 0x80c8, 8, 0, 5, 0, 0, 238, 103, 0, 0}
	template = append(template, windows.StringToUTF16("JumpServer 图形验证码")...)
	template = append(template, 10)
	template = append(template, windows.StringToUTF16("Segoe UI")...)
	add := func(style uint32, x, y, width, height, id, class uint16, title string) {
		// Each DLGITEMTEMPLATE starts on a DWORD boundary.
		if len(template)%2 != 0 {
			template = append(template, 0)
		}
		template = append(template, uint16(style), uint16(style>>16), 0, 0, x, y, width, height, id, 0xffff, class)
		template = append(template, windows.StringToUTF16(title)...)
		template = append(template, 0)
	}
	add(wsChildVisible, 10, 8, 218, 12, 0xffff, staticClass, "请输入下方图形验证码")
	// SS_BITMAP | SS_CENTERIMAGE keeps the bitmap centered in the preview control.
	add(wsChildVisible|0x020e, 10, 22, 218, 33, captchaPreview, staticClass, "")
	// ES_AUTOHSCROLL with no password style shows the typed characters.
	add(wsChildVisible|wsTabStop|wsBorder|0x0080, 10, 60, 218, 15, captchaEdit, editClass, "")
	add(wsChildVisible|wsTabStop|1, 121, 82, 51, 15, captchaOK, buttonClass, "提交")
	add(wsChildVisible|wsTabStop, 177, 82, 51, 15, captchaCancel, buttonClass, "取消")
	return template
}

type captchaBitmapHeader struct {
	size, width, height              int32
	planes, bitsPerPixel             uint16
	compression, sizeImage           uint32
	xPixelsPerMeter, yPixelsPerMeter int32
	colorsUsed, importantColors      uint32
}

func createWindowsCaptchaBitmap(preview uintptr, picture image.Image) (uintptr, []byte) {
	var rect struct{ left, top, right, bottom int32 }
	ok, _, _ := captchaGetClientRect.Call(preview, uintptr(unsafe.Pointer(&rect)))
	bounds := picture.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if ok == 0 || rect.right < 1 || rect.bottom < 1 || width < 1 || height < 1 {
		return 0, nil
	}
	scale := min(1, float64(rect.right)/float64(width), float64(rect.bottom)/float64(height))
	outWidth := max(1, int(float64(width)*scale))
	outHeight := max(1, int(float64(height)*scale))
	header := captchaBitmapHeader{
		size: 40, width: int32(outWidth), height: -int32(outHeight), planes: 1, bitsPerPixel: 32,
	}
	var bits *byte
	bitmap, _, _ := captchaCreateDIB.Call(0, uintptr(unsafe.Pointer(&header)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bitmap == 0 {
		return 0, nil
	}
	pixels := unsafe.Slice(bits, outWidth*outHeight*4)
	for y := 0; y < outHeight; y++ {
		for x := 0; x < outWidth; x++ {
			r, g, b, a := picture.At(bounds.Min.X+x*width/outWidth, bounds.Min.Y+y*height/outHeight).RGBA()
			white := uint32(65535) - a
			i := (y*outWidth + x) * 4
			pixels[i] = byte((b + white) >> 8)
			pixels[i+1] = byte((g + white) >> 8)
			pixels[i+2] = byte((r + white) >> 8)
			pixels[i+3] = 0
		}
	}
	return bitmap, pixels
}
