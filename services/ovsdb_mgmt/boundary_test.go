package main

import (
	"context"
	"net"
	"reflect"
	"testing"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	pb "github.com/romanspb80/evpn-for-ovn/gen/ovsdb_mgmt"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type lookupClient struct {
	client.Client
	lsp     nbdb.LogicalSwitchPort
	iface   vswitch.Interface
	updates int
	writes  int
}

func (c *lookupClient) Where(rows ...model.Model) client.ConditionalAPI {
	return &lookupCondition{owner: c, row: rows[0]}
}

func (c *lookupClient) Transact(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.writes++
	return nil, nil
}

type lookupCondition struct {
	client.ConditionalAPI
	owner *lookupClient
	row   model.Model
}

func (c *lookupCondition) List(_ context.Context, result any) error {
	switch out := result.(type) {
	case *[]nbdb.LogicalSwitchPort:
		if c.row.(*nbdb.LogicalSwitchPort).Name == c.owner.lsp.Name {
			*out = []nbdb.LogicalSwitchPort{c.owner.lsp}
		}
	case *[]vswitch.Interface:
		query := c.row.(*vswitch.Interface)
		if query.Name == c.owner.iface.Name || query.Type == c.owner.iface.Type {
			*out = []vswitch.Interface{c.owner.iface}
		}
	}
	return nil
}

func (c *lookupCondition) Update(model.Model, ...any) ([]ovsdb.Operation, error) {
	c.owner.updates++
	return []ovsdb.Operation{{Op: "update"}}, nil
}

func TestManagementBoundary(t *testing.T) {
	nb := &lookupClient{lsp: nbdb.LogicalSwitchPort{Name: "vtep", UUID: "lsp-uuid"}}
	ovs := &lookupClient{iface: vswitch.Interface{
		Name: "vtep", UUID: "iface-uuid", Type: "vxlan",
		ExternalIDs: map[string]string{"other": "preserve"},
		Options:     map[string]string{"remote_ip": "192.0.2.1"},
	}}
	before := map[string]string{"other": "preserve"}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterOvsdbServiceServer(grpcServer, &OvsdbServer{nbClient: nb, ovsClient: ovs})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	conn, err := grpc.NewClient("passthrough:///ovsdb", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	svc := pb.NewOvsdbServiceClient(conn)
	ctx := context.Background()

	add, err := svc.AddLogicalSwitch(ctx, &pb.LogicalSwitch{Name: "test"})
	if add != nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("AddLogicalSwitch = %v, %v; want Unimplemented", add, err)
	}
	del, err := svc.DelLogicalSwitch(ctx, &pb.LogicalSwitchRequest{Name: "test"})
	if del != nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("DelLogicalSwitch = %v, %v; want Unimplemented", del, err)
	}
	result, err := svc.GetVtep(ctx, &pb.GetVtepRequest{LspName: "vtep"})
	if err != nil || result.GetLogicalSwitchPort().GetName() != "vtep" ||
		result.GetVxlanInterface().GetRemoteIp() != "192.0.2.1" {
		t.Fatalf("GetVtep = %v, %v", result, err)
	}
	if nb.updates != 0 || nb.writes != 0 || ovs.updates != 0 || ovs.writes != 0 || !reflect.DeepEqual(ovs.iface.ExternalIDs, before) {
		t.Fatalf("management call mutated database: nb=%+v ovs=%+v", nb, ovs)
	}
}
