//go:build windows

package main

import "unsafe"

func uintptrFromUTF16(value *uint16) uintptr { return uintptr(unsafe.Pointer(value)) }
