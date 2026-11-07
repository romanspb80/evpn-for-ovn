package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The checks apply to the actual gRPC connection, including reconnects. A
// separate probe cannot establish the identity of a later runtime transport.
func dialTrustedCRI(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
	if !strings.HasPrefix(endpoint, "unix:///") {
		return nil, errors.New("explicit local unix CRI endpoint required")
	}
	path := strings.TrimPrefix(endpoint, "unix://")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("clean absolute CRI socket path required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return grpc.DialContext(ctx, "passthrough:///"+path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialTrustedCRIUnix(ctx, path)
		}),
		grpc.WithBlock(), grpc.WithReturnConnectionError(),
	)
}

func validateCRISocket(st unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != 0 || st.Mode&07777 != 0600 || st.Nlink != 1 {
		return errors.New("CRI socket must be root-owned single-link Unix socket mode 0600")
	}
	return nil
}

func validateCRIPeer(cred *unix.Ucred) error {
	if cred == nil || cred.Pid <= 0 || cred.Uid != 0 || cred.Gid != 0 {
		return errors.New("actual CRI Unix peer must be a visible root:root process")
	}
	return nil
}

func validateCRIPathContinuity(before, after, heldParent, currentParent unix.Stat_t) error {
	if err := validateCRISocket(before); err != nil {
		return err
	}
	if err := validateCRISocket(after); err != nil {
		return err
	}
	if heldParent.Dev != currentParent.Dev || heldParent.Ino != currentParent.Ino ||
		before.Dev != after.Dev || before.Ino != after.Ino {
		return errors.New("CRI socket or trusted parent replaced during connection")
	}
	return nil
}

func dialTrustedCRIUnix(ctx context.Context, path string) (net.Conn, error) {
	// Reuse the provider's no-symlink, root-owned, non-writable ancestor walk.
	parent, err := openAdministratorDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(path), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if err := validateCRISocket(before); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		return nil, errors.Join(err, conn.Close())
	}
	current, err := openAdministratorDirectory(filepath.Dir(path))
	if err != nil {
		return fail(err)
	}
	var heldParent, currentParent, after unix.Stat_t
	checkErr := errors.Join(unix.Fstat(int(parent.Fd()), &heldParent), unix.Fstat(int(current.Fd()), &currentParent),
		unix.Fstatat(int(current.Fd()), filepath.Base(path), &after, unix.AT_SYMLINK_NOFOLLOW))
	checkErr = errors.Join(checkErr, current.Close())
	if checkErr != nil {
		return fail(checkErr)
	}
	if err := validateCRIPathContinuity(before, after, heldParent, currentParent); err != nil {
		return fail(err)
	}
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		return fail(errors.New("CRI transport is not Unix"))
	}
	if err := checkActualCRIPeer(socket); err != nil {
		return fail(err)
	}
	return conn, nil
}

func checkActualCRIPeer(socket *net.UnixConn) error {
	raw, err := socket.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return fmt.Errorf("actual CRI Unix peer credentials: %w", peerErr)
	}
	return validateCRIPeer(cred)
}
