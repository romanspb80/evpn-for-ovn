// type2rib collects a read-only EVPN Type-2 snapshot from a local GoBGP socket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func processStartTicks(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("incomplete process stat")
	}
	return fields[19], nil
}

func run() error {
	socket := flag.String("socket", "", "GoBGP Unix socket")
	routerID := flag.String("router-id", "", "expected speaker router ID")
	pid := flag.Int("pid", 0, "attested speaker PID")
	ticks := flag.String("start-ticks", "", "attested speaker process start ticks")
	flag.Parse()
	if *socket == "" || *routerID == "" || *pid <= 0 || *ticks == "" {
		return fmt.Errorf("socket, router-id, pid and start-ticks are required")
	}
	checkProcess := func() error {
		actual, err := processStartTicks(*pid)
		if err != nil {
			return err
		}
		if actual != *ticks {
			return fmt.Errorf("speaker PID %s start ticks changed", strconv.Itoa(*pid))
		}
		return nil
	}
	if err := checkProcess(); err != nil {
		return err
	}
	conn, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "unix", *socket)
			if err != nil {
				return nil, err
			}
			unixConn, ok := raw.(*net.UnixConn)
			if !ok {
				raw.Close()
				return nil, fmt.Errorf("speaker socket is not Unix")
			}
			sc, err := unixConn.SyscallConn()
			if err != nil {
				raw.Close()
				return nil, err
			}
			var peer *unix.Ucred
			var peerErr error
			err = sc.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
			if err != nil || peerErr != nil || peer == nil || int(peer.Pid) != *pid {
				raw.Close()
				return nil, fmt.Errorf("socket peer is not the attested speaker: control=%v peer=%v credentials=%v", err, peerErr, peer)
			}
			if err := checkProcess(); err != nil {
				raw.Close()
				return nil, err
			}
			return raw, nil
		}))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	gate := evpn.GoBGPIdentityReadiness{Client: client, ASN: 64512, RouterID: *routerID}
	adapter := evpn.GoBGPType2Adapter{Client: client, Gate: gate}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	paths, err := adapter.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err := checkProcess(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(paths)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
