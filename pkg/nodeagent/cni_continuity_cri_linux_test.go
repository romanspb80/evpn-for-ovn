package nodeagent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCRITrustedSocketMetadata(t *testing.T) {
	valid := unix.Stat_t{Mode: unix.S_IFSOCK | 0600, Uid: 0, Nlink: 1, Dev: 1, Ino: 2}
	parent := unix.Stat_t{Dev: 1, Ino: 3}
	if err := validateCRIPathContinuity(valid, valid, parent, parent); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foreign-owner", "world-access", "group-access", "symlink", "regular-file", "multiple-links", "socket-replaced", "parent-replaced", "device-changed"} {
		t.Run(name, func(t *testing.T) {
			after, currentParent := valid, parent
			switch name {
			case "foreign-owner":
				after.Uid = 1000
			case "world-access":
				after.Mode |= 0004
			case "group-access":
				after.Mode |= 0040
			case "symlink":
				after.Mode = unix.S_IFLNK | 0600
			case "regular-file":
				after.Mode = unix.S_IFREG | 0600
			case "multiple-links":
				after.Nlink = 2
			case "socket-replaced":
				after.Ino++
			case "parent-replaced":
				currentParent.Ino++
			case "device-changed":
				after.Dev++
			}
			if err := validateCRIPathContinuity(valid, after, parent, currentParent); err == nil {
				t.Fatal("untrusted socket continuity accepted")
			}
		})
	}
}

func TestCRITrustedActualPeerCredentials(t *testing.T) {
	for _, cred := range []*unix.Ucred{nil, {Pid: 0}, {Pid: 1, Uid: 1000}, {Pid: 1, Gid: 1000}} {
		if err := validateCRIPeer(cred); err == nil {
			t.Fatal("untrusted peer accepted")
		}
	}
	if err := validateCRIPeer(&unix.Ucred{Pid: 1}); err != nil {
		t.Fatal(err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	f := os.NewFile(uintptr(fds[0]), "cri-peer")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = checkActualCRIPeer(conn.(*net.UnixConn))
	if os.Geteuid() == 0 && os.Getegid() == 0 {
		if err != nil {
			t.Fatalf("kernel root Unix peer denied: %v", err)
		}
	} else if err == nil {
		t.Fatal("actual nonroot Unix peer accepted")
	}
}

func TestCRITrustedDialRejectsUntrustedEndpoint(t *testing.T) {
	for _, endpoint := range []string{"tcp://127.0.0.1:1234", "unix://relative", "unix:///run/../run/containerd.sock", "unix:///"} {
		if conn, err := dialTrustedCRI(context.Background(), endpoint); err == nil {
			conn.Close()
			t.Fatal("unsafe runtime endpoint accepted")
		}
	}
	if os.Geteuid() != 0 {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if conn, err := dialTrustedCRIUnix(context.Background(), filepath.Join(cwd, "missing.sock")); err == nil {
			conn.Close()
			t.Fatal("user-controlled runtime ancestor accepted")
		}
	}
}
