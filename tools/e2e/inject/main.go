// A test-only route owner; withdrawal is possible only through its durable UUID receipt.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/romanspb80/evpn-for-ovn/pkg/evpn"
	"github.com/romanspb80/evpn-for-ovn/pkg/nodeagent"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type config struct {
	Socket, Attestation, State, InstallationUID, NodeUID, RouterID string
	ASN                                                         uint32
	Endpoint                                                    evpn.LocalEndpoint
}

func run(path, action string) (err error) {
	if action != "advertise" && action != "withdraw" {
		return fmt.Errorf("action must be advertise or withdraw")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if err := c.Endpoint.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(c.State) {
		return fmt.Errorf("explicit absolute injection journal path required")
	}
	guard, err := nodeagent.NewAttestedSpeakerContinuity(c.Attestation)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, guard.Close()) }()
	conn, err := grpc.NewClient(c.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(guard.DialContext))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := api.NewGoBgpServiceClient(conn)
	if err := (evpn.SpeakerReadiness{Client: client, ASN: c.ASN, RouterID: c.RouterID}).Check(ctx); err != nil {
		return err
	}
	if err := guard.Check(ctx); err != nil {
		return err
	}
	evidence := guard.Evidence()
	if evidence.Standalone == nil || evidence.Standalone.InstallationUID != c.InstallationUID ||
		evidence.Standalone.NodeUID != c.NodeUID {
		return fmt.Errorf("injection journal identity differs from attested speaker")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	ns, err := netns.Get()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ns.Close()) }()
	sum := sha256.Sum256(data)
	adapter := &evpn.GoBGPType2Adapter{Client: client, Gate: guard}
	recovery, err := evpn.OpenExportRecovery(evpn.ExportRecoveryConfig{
		StateDirectory: c.State,
		Domain: ownership.Domain{InstallationUID: c.InstallationUID, NodeUID: c.NodeUID,
			BootID: strings.TrimSpace(string(boot)), NetworkNamespace: ns.UniqueId(),
			ConfigDigest: hex.EncodeToString(sum[:])},
	}, adapter, adapter, guard)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, recovery.Close()) }()
	owned, err := recovery.Recover(ctx)
	if err != nil {
		return err
	}
	if len(owned) > 1 {
		return fmt.Errorf("injection journal has multiple routes")
	}
	var id string
	if action == "advertise" {
		if len(owned) != 0 {
			return fmt.Errorf("injection already owns a route")
		}
		id, err = recovery.Add(ctx, evpn.Type2Route{Endpoint: c.Endpoint})
	} else {
		if len(owned) != 1 {
			return fmt.Errorf("no acknowledged current UUID available for withdrawal")
		}
		id = owned[0].PathUUID
		err = recovery.Delete(ctx, owned[0])
	}
	if err != nil {
		return err
	}
	incarnation, err := guard.Incarnation()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Action, PathUUID, Incarnation string
		Speaker                      nodeagent.SpeakerProcessEvidence
	}{action, id, incarnation, guard.Evidence()})
}

func main() {
	path := flag.String("config", "", "root-owned fixture config")
	action := flag.String("action", "", "advertise or withdraw")
	flag.Parse()
	if err := run(*path, *action); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
