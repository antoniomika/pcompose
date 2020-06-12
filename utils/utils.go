// Package utils implements utilities for the pcompose application
package utils

import (
	"encoding/binary"
	"log"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/crypto/ssh"
)

const (
	// HooksDirName is the directory that contains git hooks
	HooksDirName = "hooks/"

	// HooksConfigFile is the config file used for hooks
	HooksConfigFile = "hooks.yml"

	// UploadPackServiceName is the command name for uploading a git pack
	UploadPackServiceName = "git-upload-pack"

	// ReceivePackServiceName is the command name for receiving a git pack
	ReceivePackServiceName = "git-receive-pack"
)

// SSHConnHolder is the ssh connection we hold onto.
type SSHConnHolder struct {
	MainConn *ssh.ServerConn
	W        uint32
	H        uint32
	Mu       sync.Mutex
	Term     *os.File
}

type winsize struct {
	Height uint16
	Width  uint16
}

// ParseDims parses dimensions into width and height.
func ParseDims(b []byte) (uint32, uint32) {
	w := binary.BigEndian.Uint32(b)
	h := binary.BigEndian.Uint32(b[4:])
	return w, h
}

// SetWinSize sends a syscall to change the window size.
func SetWinSize(fd uintptr, w, h uint32) {
	ws := &winsize{Width: uint16(w), Height: uint16(h)}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(ws)))
	if err != 0 {
		log.Println("Error sending syscall to pty:", err)
	}
}
