package attachment

import (
	"context"
	"testing"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"
)

type fakeOVSStore struct {
	bridgesRows []vswitch.Bridge
	ifaces      []vswitch.Interface
	ports       []vswitch.Port
	bridges     map[string][]string
}

func (f *fakeOVSStore) ListBridges(_ context.Context) ([]vswitch.Bridge, error) {
	if len(f.bridgesRows) > 0 {
		return f.bridgesRows, nil
	}
	if len(f.bridges) == 0 {
		return nil, nil
	}
	rows := make([]vswitch.Bridge, 0, len(f.bridges))
	for name, ports := range f.bridges {
		rows = append(rows, vswitch.Bridge{Name: name, Ports: append([]string(nil), ports...)})
	}
	return rows, nil
}
func (f *fakeOVSStore) ListInterfaces(_ context.Context) ([]vswitch.Interface, error) {
	return f.ifaces, nil
}
func (f *fakeOVSStore) ListPorts(_ context.Context) ([]vswitch.Port, error) { return f.ports, nil }
func (f *fakeOVSStore) CreateInterface(_ context.Context, iface *vswitch.Interface) error {
	if iface.UUID == "" {
		iface.UUID = "if-" + iface.Name
	}
	f.ifaces = append(f.ifaces, *iface)
	return nil
}
func (f *fakeOVSStore) CreateInterfacePortAndAttachBridge(ctx context.Context, bridgeName string, iface *vswitch.Interface, port *vswitch.Port) error {
	if err := f.CreateInterface(ctx, iface); err != nil {
		return err
	}
	if len(port.Interfaces) == 0 || port.Interfaces[0] == "" {
		port.Interfaces = []string{iface.UUID}
	}
	if err := f.CreatePort(ctx, port); err != nil {
		return err
	}
	return f.EnsureBridgePort(ctx, bridgeName, port.UUID)
}
func (f *fakeOVSStore) UpdateInterface(_ context.Context, iface *vswitch.Interface) error {
	for i := range f.ifaces {
		if f.ifaces[i].UUID == iface.UUID {
			f.ifaces[i].ExternalIDs = iface.ExternalIDs
			f.ifaces[i].Type = iface.Type
			return nil
		}
	}
	return nil
}
func (f *fakeOVSStore) DeleteInterface(_ context.Context, iface *vswitch.Interface) error {
	for i := range f.ifaces {
		if f.ifaces[i].UUID == iface.UUID {
			f.ifaces = append(f.ifaces[:i], f.ifaces[i+1:]...)
			return nil
		}
	}
	return nil
}
func (f *fakeOVSStore) DeleteInterfaceIfExternalIDsMatch(_ context.Context, uuid string, expectedExternalIDs map[string]string) (bool, error) {
	for i := range f.ifaces {
		if f.ifaces[i].UUID != uuid {
			continue
		}
		if !sameMap(f.ifaces[i].ExternalIDs, expectedExternalIDs) {
			return false, nil
		}
		f.ifaces = append(f.ifaces[:i], f.ifaces[i+1:]...)
		return true, nil
	}
	return false, nil
}
func (f *fakeOVSStore) CreatePort(_ context.Context, port *vswitch.Port) error {
	if port.UUID == "" {
		port.UUID = "p-" + port.Name
	}
	f.ports = append(f.ports, *port)
	return nil
}
func (f *fakeOVSStore) CreatePortAndAttachBridge(ctx context.Context, bridgeName string, port *vswitch.Port) error {
	if err := f.CreatePort(ctx, port); err != nil {
		return err
	}
	return f.EnsureBridgePort(ctx, bridgeName, port.UUID)
}
func (f *fakeOVSStore) UpdatePort(_ context.Context, _ *vswitch.Port) error { return nil }
func (f *fakeOVSStore) DeletePort(_ context.Context, port *vswitch.Port) error {
	for i := range f.ports {
		if f.ports[i].UUID == port.UUID {
			f.ports = append(f.ports[:i], f.ports[i+1:]...)
			return nil
		}
	}
	return nil
}
func (f *fakeOVSStore) EnsureBridgePort(_ context.Context, bridgeName, portUUID string) error {
	if f.bridges == nil {
		f.bridges = map[string][]string{}
	}
	if !contains(f.bridges[bridgeName], portUUID) {
		f.bridges[bridgeName] = append(f.bridges[bridgeName], portUUID)
	}
	return nil
}

func (f *fakeOVSStore) DetachPortFromBridgeIfInterfaceOwned(_ context.Context, bridgeName, portUUID, ifaceUUID string, expectedExternalIDs map[string]string) (bool, error) {
	for i := range f.ifaces {
		if f.ifaces[i].UUID != ifaceUUID {
			continue
		}
		if !sameMap(f.ifaces[i].ExternalIDs, expectedExternalIDs) {
			return false, nil
		}
		ports := f.bridges[bridgeName]
		next := make([]string, 0, len(ports))
		for _, id := range ports {
			if id != portUUID {
				next = append(next, id)
			}
		}
		f.bridges[bridgeName] = next
		for p := range f.ports {
			if f.ports[p].UUID == portUUID {
				f.ports = append(f.ports[:p], f.ports[p+1:]...)
				break
			}
		}
		f.ifaces = append(f.ifaces[:i], f.ifaces[i+1:]...)
		return true, nil
	}
	return false, nil
}

func TestEnsureAttachmentCreatesAndIdempotent(t *testing.T) {
	store := &fakeOVSStore{}
	mgr := NewOVSAttachmentManager(store)
	spec := endpoint.AttachmentSpec{
		EndpointID:        "ep-1",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
		MACAddress:        "aa:bb:cc:dd:ee:ff",
	}
	if _, err := mgr.EnsureAttachment(context.Background(), spec); err != nil {
		t.Fatalf("first ensure failed: %v", err)
	}
	if _, err := mgr.EnsureAttachment(context.Background(), spec); err != nil {
		t.Fatalf("second ensure failed: %v", err)
	}
	if len(store.ifaces) != 1 {
		t.Fatalf("expected one interface, got %d", len(store.ifaces))
	}
}

func TestEnsureAttachmentOwnershipConflict(t *testing.T) {
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", ExternalIDs: map[string]string{
				ExternalIDManagedBy: "external-controller",
			},
		}},
	}
	mgr := NewOVSAttachmentManager(store)
	_, err := mgr.EnsureAttachment(context.Background(), endpoint.AttachmentSpec{
		EndpointID:        "ep-1",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
	})
	if err == nil {
		t.Fatalf("expected ownership conflict")
	}
}

func TestEnsureAttachmentLegacyUnmanagedMismatchRejected(t *testing.T) {
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", ExternalIDs: map[string]string{
				ExternalIDIfaceID: "other-lsp",
			},
		}},
	}
	mgr := NewOVSAttachmentManager(store)
	_, err := mgr.EnsureAttachment(context.Background(), endpoint.AttachmentSpec{
		EndpointID:        "ep-1",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
	})
	if err == nil {
		t.Fatalf("expected legacy unmanaged conflict")
	}
}

func TestDeleteAttachmentLegacyUnmanagedRejected(t *testing.T) {
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", ExternalIDs: map[string]string{
				ExternalIDIfaceID: "lsp-a",
			},
		}},
	}
	mgr := NewOVSAttachmentManager(store)
	if err := mgr.DeleteAttachment(context.Background(), "ep-1", "lsp-a"); err == nil {
		t.Fatalf("expected unmanaged delete conflict")
	}
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestDeleteAttachmentIdempotentMissing(t *testing.T) {
	store := &fakeOVSStore{}
	mgr := NewOVSAttachmentManager(store)
	if err := mgr.DeleteAttachment(context.Background(), "ep-1", "lsp-a"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestDeleteAttachmentDeletesOwnedBridgePort(t *testing.T) {
	store := &fakeOVSStore{
		ports: []vswitch.Port{{UUID: "p-1", Name: "evp-abc", Interfaces: []string{"if-1"}}},
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", ExternalIDs: ExternalIDsForIdentity(Identity{
				EndpointID:   "ep-1",
				NetworkID:    "net-a",
				AttachmentID: "att-1",
				WorkloadUID:  "wl-1",
				Incarnation:  "1",
			}, "lsp-a", "aa:bb:cc:dd:ee:ff"),
		}},
		bridges: map[string][]string{"br-int": []string{"p-1"}},
	}
	mgr := NewOVSAttachmentManager(store)
	outcome, err := mgr.DeleteAttachmentWithIdentity(context.Background(), Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}, "lsp-a")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if outcome != DeleteOutcomeDeleted {
		t.Fatalf("unexpected outcome: %s", outcome)
	}
	if len(store.ifaces) != 0 || len(store.ports) != 0 || len(store.bridges["br-int"]) != 0 {
		t.Fatalf("expected attachment rows to be removed from bridge/port/interface")
	}
}

func TestDeleteAttachmentUnexpectedBridgeConflict(t *testing.T) {
	store := &fakeOVSStore{
		ports: []vswitch.Port{{UUID: "p-1", Name: "evp-abc", Interfaces: []string{"if-1"}}},
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", ExternalIDs: ExternalIDsForIdentity(Identity{
				EndpointID:   "ep-1",
				NetworkID:    "net-a",
				AttachmentID: "att-1",
				WorkloadUID:  "wl-1",
				Incarnation:  "1",
			}, "lsp-a", "aa:bb:cc:dd:ee:ff"),
		}},
		bridges: map[string][]string{"br-ext": []string{"p-1"}},
	}
	mgr := NewOVSAttachmentManager(store)
	_, err := mgr.DeleteAttachmentWithIdentity(context.Background(), Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}, "lsp-a")
	if err == nil {
		t.Fatalf("expected delete conflict for unexpected bridge")
	}
}

func TestEnsureAttachmentCreatesPortForExistingInterface(t *testing.T) {
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", Type: "system", ExternalIDs: ExternalIDsForIdentity(Identity{
				EndpointID:   "ep-1",
				NetworkID:    "net-a",
				AttachmentID: "att-1",
				WorkloadUID:  "wl-1",
				Incarnation:  "1",
			}, "lsp-a", "aa:bb:cc:dd:ee:ff"),
		}},
	}
	mgr := NewOVSAttachmentManager(store)
	_, err := mgr.EnsureAttachmentWithIdentity(context.Background(), Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}, endpoint.AttachmentSpec{
		EndpointID:        "ep-1",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
		MACAddress:        "aa:bb:cc:dd:ee:ff",
	})
	if err != nil {
		t.Fatalf("expected missing port to be repaired, got %v", err)
	}
	if len(store.ports) != 1 {
		t.Fatalf("expected one port, got %d", len(store.ports))
	}
}

func TestEnsureAttachmentRejectsExistingPortWithoutInterface(t *testing.T) {
	store := &fakeOVSStore{
		ports: []vswitch.Port{{
			UUID: "p-1", Name: "evp-abc", Interfaces: []string{"if-missing"},
		}},
	}
	mgr := NewOVSAttachmentManager(store)
	_, err := mgr.EnsureAttachmentWithIdentity(context.Background(), Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}, endpoint.AttachmentSpec{
		EndpointID:        "ep-1",
		LogicalSwitchPort: "lsp-a",
		OVSInterfaceName:  "evp-abc",
	})
	if err == nil {
		t.Fatalf("expected conflict for existing port without interface")
	}
}

func TestCheckAttachmentWithIdentityValidatesBridgeAndIfaceID(t *testing.T) {
	identity := Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", Type: "system",
			ExternalIDs: ExternalIDsForIdentity(identity, "lsp-a", "aa:bb:cc:dd:ee:ff"),
		}},
		ports:   []vswitch.Port{{UUID: "p-1", Name: "evp-abc", Interfaces: []string{"if-1"}}},
		bridges: map[string][]string{"br-int": []string{"p-1"}},
	}
	mgr := NewOVSAttachmentManager(store)
	if _, err := mgr.CheckAttachmentWithIdentity(context.Background(), identity, "lsp-a", "evp-abc"); err != nil {
		t.Fatalf("check attachment failed: %v", err)
	}
}

func TestCheckAttachmentWithIdentityRejectsWrongBridge(t *testing.T) {
	identity := Identity{
		EndpointID:   "ep-1",
		NetworkID:    "net-a",
		AttachmentID: "att-1",
		WorkloadUID:  "wl-1",
		Incarnation:  "1",
	}
	store := &fakeOVSStore{
		ifaces: []vswitch.Interface{{
			UUID: "if-1", Name: "evp-abc", Type: "system",
			ExternalIDs: ExternalIDsForIdentity(identity, "lsp-a", "aa:bb:cc:dd:ee:ff"),
		}},
		ports:   []vswitch.Port{{UUID: "p-1", Name: "evp-abc", Interfaces: []string{"if-1"}}},
		bridges: map[string][]string{"br-ext": []string{"p-1"}},
	}
	mgr := NewOVSAttachmentManager(store)
	if _, err := mgr.CheckAttachmentWithIdentity(context.Background(), identity, "lsp-a", "evp-abc"); err == nil {
		t.Fatalf("expected bridge validation error")
	}
}
