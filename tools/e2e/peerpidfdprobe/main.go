package main

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type result struct {
	Operation  string `json:"operation"`
	Socket     string `json:"socket"`
	Level      int    `json:"level"`
	Option     int    `json:"option"`
	PIDFD      int    `json:"peerPidFD"`
	Descriptor string `json:"descriptor"`
}

func run() error {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socketpair(AF_UNIX, SOCK_STREAM): %w", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	pidfd, err := unix.GetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	if err != nil {
		return fmt.Errorf("getsockopt(SOL_SOCKET, SO_PEERPIDFD): %w", err)
	}
	defer unix.Close(pidfd)
	descriptor, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", pidfd))
	if err != nil {
		return fmt.Errorf("inspect SO_PEERPIDFD result: %w", err)
	}
	if descriptor != "anon_inode:[pidfd]" {
		return fmt.Errorf("SO_PEERPIDFD returned unexpected descriptor %q", descriptor)
	}
	return json.NewEncoder(os.Stdout).Encode(result{
		Operation:  "getsockopt(SOL_SOCKET, SO_PEERPIDFD)",
		Socket:     "AF_UNIX/SOCK_STREAM",
		Level:      unix.SOL_SOCKET,
		Option:     unix.SO_PEERPIDFD,
		PIDFD:      pidfd,
		Descriptor: descriptor,
	})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
