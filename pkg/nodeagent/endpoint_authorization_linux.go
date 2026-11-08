//go:build linux

package nodeagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"golang.org/x/sys/unix"
)

const MaxEndpointAuthorizationFileSize = endpointauth.MaxDocumentSize

// LoadEndpointAuthorization opens and validates a root-protected standalone
// endpoint authorization file. It does not observe OVN state or authorize a
// route mutation by itself.
func LoadEndpointAuthorization(path string) (snapshot *endpointauth.Snapshot, retErr error) {
	dirFD, name, err := openProtectedParent(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := unix.Close(dirFD); err != nil {
			snapshot = nil
			retErr = errors.Join(retErr, fmt.Errorf("close endpoint authorization directory: %w", err))
		}
	}()

	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open endpoint authorization file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() {
		if err := file.Close(); err != nil {
			snapshot = nil
			retErr = errors.Join(retErr, fmt.Errorf("close endpoint authorization file: %w", err))
		}
	}()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, fmt.Errorf("stat endpoint authorization file: %w", err)
	}
	if before.Uid != 0 || before.Mode&unix.S_IFMT != unix.S_IFREG ||
		before.Mode&0022 != 0 || before.Nlink > 1 {
		return nil, fmt.Errorf("endpoint authorization file must be a root-owned, single-link regular file not writable by group or others")
	}
	if before.Size <= 0 || before.Size > MaxEndpointAuthorizationFileSize {
		return nil, fmt.Errorf("endpoint authorization file size must be between 1 and %d bytes", MaxEndpointAuthorizationFileSize)
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxEndpointAuthorizationFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read endpoint authorization file: %w", err)
	}
	if len(data) == 0 || len(data) > MaxEndpointAuthorizationFileSize || int64(len(data)) != before.Size {
		return nil, fmt.Errorf("endpoint authorization file changed size or exceeds the %d-byte limit", MaxEndpointAuthorizationFileSize)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, fmt.Errorf("restat endpoint authorization file: %w", err)
	}
	if after.Nlink > 1 || !sameFileSnapshot(before, after) {
		return nil, fmt.Errorf("endpoint authorization file changed while being read")
	}

	snapshot, err = endpointauth.Decode(data)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func openProtectedParent(path string) (int, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return -1, "", fmt.Errorf("endpoint authorization path must be a clean absolute file path")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, "", fmt.Errorf("endpoint authorization path contains an unsafe component")
		}
	}

	current, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open filesystem root: %w", err)
	}
	var rootStat unix.Stat_t
	if err := unix.Fstat(current, &rootStat); err != nil {
		return -1, "", errors.Join(fmt.Errorf("stat filesystem root: %w", err), closeAuthorizationFD(current))
	}
	if rootStat.Uid != 0 || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Mode&0022 != 0 {
		return -1, "", errors.Join(
			fmt.Errorf("filesystem root must be root-owned and not writable by group or others"),
			closeAuthorizationFD(current),
		)
	}
	for _, component := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, component, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return -1, "", errors.Join(
				fmt.Errorf("open protected directory component %q: %w", component, openErr),
				closeAuthorizationFD(current),
			)
		}
		var stat unix.Stat_t
		statErr := unix.Fstat(next, &stat)
		if statErr == nil && (stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0022 != 0) {
			statErr = fmt.Errorf("directory must be root-owned and not writable by group or others")
		}
		if statErr != nil {
			return -1, "", errors.Join(
				fmt.Errorf("unsafe endpoint authorization parent %q: %w", component, statErr),
				closeAuthorizationFD(next), closeAuthorizationFD(current),
			)
		}
		if err := unix.Close(current); err != nil {
			return -1, "", errors.Join(
				fmt.Errorf("close endpoint authorization parent descriptor: %w", err),
				closeAuthorizationFD(next),
			)
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func closeAuthorizationFD(fd int) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close endpoint authorization descriptor: %w", err)
	}
	return nil
}

func sameFileSnapshot(a, b unix.Stat_t) bool {
	stableMetadata := a.Dev == b.Dev && a.Ino == b.Ino && a.Uid == b.Uid && a.Mode == b.Mode &&
		a.Size == b.Size &&
		a.Mtim.Sec == b.Mtim.Sec && a.Mtim.Nsec == b.Mtim.Nsec
	if !stableMetadata {
		return false
	}
	if a.Ctim.Sec == b.Ctim.Sec && a.Ctim.Nsec == b.Ctim.Nsec {
		return true
	}
	return a.Nlink == 1 && b.Nlink == 0
}
