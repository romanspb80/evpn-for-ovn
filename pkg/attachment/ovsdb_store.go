package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type LibovsdbOVSStore struct {
	client client.Client
}

func NewLibovsdbOVSStore(c client.Client) *LibovsdbOVSStore {
	return &LibovsdbOVSStore{client: c}
}

func (s *LibovsdbOVSStore) ListBridges(ctx context.Context) ([]vswitch.Bridge, error) {
	var all []vswitch.Bridge
	if err := s.client.List(ctx, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func (s *LibovsdbOVSStore) ListInterfaces(ctx context.Context) ([]vswitch.Interface, error) {
	var all []vswitch.Interface
	if err := s.client.List(ctx, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func (s *LibovsdbOVSStore) ListPorts(ctx context.Context) ([]vswitch.Port, error) {
	var all []vswitch.Port
	if err := s.client.List(ctx, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func (s *LibovsdbOVSStore) CreateInterface(ctx context.Context, iface *vswitch.Interface) error {
	if stringsTrim(iface.UUID) == "" {
		iface.UUID = uuid.New().String()
	}
	ops, err := s.client.Create(iface)
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func namedUUID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, strings.ReplaceAll(uuid.New().String(), "-", "_"))
}

func (s *LibovsdbOVSStore) bridgeUUIDByName(ctx context.Context, bridgeName string) (string, error) {
	bridges, err := s.ListBridges(ctx)
	if err != nil {
		return "", err
	}
	for i := range bridges {
		if bridges[i].Name == bridgeName {
			return bridges[i].UUID, nil
		}
	}
	return "", fmt.Errorf("bridge %s not found", bridgeName)
}

func (s *LibovsdbOVSStore) buildCreateInterfacePortAndAttachBridgeOps(iface *vswitch.Interface, port *vswitch.Port, bridgeUUID string) ([]ovsdb.Operation, error) {
	if stringsTrim(iface.UUID) == "" {
		iface.UUID = namedUUID("iface")
	}
	if stringsTrim(port.UUID) == "" {
		port.UUID = namedUUID("port")
	}
	if len(port.Interfaces) == 0 || stringsTrim(port.Interfaces[0]) == "" {
		port.Interfaces = []string{iface.UUID}
	}
	ifaceOps, err := s.client.Create(iface)
	if err != nil {
		return nil, err
	}
	portOps, err := s.client.Create(port)
	if err != nil {
		return nil, err
	}
	bridge := &vswitch.Bridge{UUID: bridgeUUID}
	bridgeOps, err := s.client.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{port.UUID},
	})
	if err != nil {
		return nil, err
	}
	ops := append(ifaceOps, portOps...)
	ops = append(ops, bridgeOps...)
	return ops, nil
}

func (s *LibovsdbOVSStore) CreateInterfacePortAndAttachBridge(ctx context.Context, bridgeName string, iface *vswitch.Interface, port *vswitch.Port) error {
	bridgeUUID, err := s.bridgeUUIDByName(ctx, bridgeName)
	if err != nil {
		return err
	}
	ops, err := s.buildCreateInterfacePortAndAttachBridgeOps(iface, port, bridgeUUID)
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) UpdateInterface(ctx context.Context, iface *vswitch.Interface) error {
	ops, err := s.client.Where(&vswitch.Interface{UUID: iface.UUID}).Update(iface)
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) DeleteInterface(ctx context.Context, iface *vswitch.Interface) error {
	ops, err := s.client.Where(&vswitch.Interface{UUID: iface.UUID}).Delete()
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) DeleteInterfaceIfExternalIDsMatch(ctx context.Context, uuid string, expectedExternalIDs map[string]string) (bool, error) {
	ops, err := s.client.Where(&vswitch.Interface{
		UUID:        uuid,
		ExternalIDs: expectedExternalIDs,
	}).Delete()
	if err != nil {
		return false, err
	}
	results, err := s.client.Transact(ctx, ops...)
	if err != nil {
		return false, err
	}
	for i, r := range results {
		if r.Error != "" {
			return false, fmt.Errorf("ovs conditional delete op %d failed: %s %s", i, r.Error, r.Details)
		}
	}
	for _, r := range results {
		if r.Count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *LibovsdbOVSStore) CreatePort(ctx context.Context, port *vswitch.Port) error {
	if stringsTrim(port.UUID) == "" {
		port.UUID = uuid.New().String()
	}
	ops, err := s.client.Create(port)
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) CreatePortAndAttachBridge(ctx context.Context, bridgeName string, port *vswitch.Port) error {
	bridgeUUID, err := s.bridgeUUIDByName(ctx, bridgeName)
	if err != nil {
		return err
	}
	if stringsTrim(port.UUID) == "" {
		port.UUID = uuid.New().String()
	}
	portOps, err := s.client.Create(port)
	if err != nil {
		return err
	}
	bridge := &vswitch.Bridge{UUID: bridgeUUID}
	bridgeOps, err := s.client.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{port.UUID},
	})
	if err != nil {
		return err
	}
	ops := append(portOps, bridgeOps...)
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) UpdatePort(ctx context.Context, port *vswitch.Port) error {
	ops, err := s.client.Where(&vswitch.Port{UUID: port.UUID}).Update(port)
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) DeletePort(ctx context.Context, port *vswitch.Port) error {
	ops, err := s.client.Where(&vswitch.Port{UUID: port.UUID}).Delete()
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) EnsureBridgePort(ctx context.Context, bridgeName, portUUID string) error {
	bridgeUUID, err := s.bridgeUUIDByName(ctx, bridgeName)
	if err != nil {
		return err
	}
	bridge := &vswitch.Bridge{UUID: bridgeUUID}
	ops, err := s.client.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{portUUID},
	})
	if err != nil {
		return err
	}
	return transactNoErr(ctx, s.client, ops...)
}

func (s *LibovsdbOVSStore) DetachPortFromBridgeIfInterfaceOwned(ctx context.Context, bridgeName, portUUID, ifaceUUID string, expectedExternalIDs map[string]string) (bool, error) {
	bridgeUUID, err := s.bridgeUUIDByName(ctx, bridgeName)
	if err != nil {
		return false, err
	}
	timeout0 := 0
	ifaceGuard := &vswitch.Interface{
		UUID:        ifaceUUID,
		ExternalIDs: expectedExternalIDs,
	}
	waitOps, err := s.client.Where(&vswitch.Interface{UUID: ifaceUUID}).Wait(
		ovsdb.WaitConditionEqual,
		&timeout0,
		ifaceGuard,
		&ifaceGuard.ExternalIDs,
	)
	if err != nil {
		return false, err
	}
	bridge := &vswitch.Bridge{UUID: bridgeUUID}
	bridgeOps, err := s.client.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   []string{portUUID},
	})
	if err != nil {
		return false, err
	}
	ops := append(waitOps, bridgeOps...)
	results, err := s.client.Transact(ctx, ops...)
	if err != nil {
		return false, err
	}
	for i, r := range results {
		if r.Error == "" {
			continue
		}
		if i == 0 && strings.EqualFold(r.Error, "timed out") {
			return false, nil
		}
		opsJSON, _ := json.Marshal(ops)
		return false, fmt.Errorf("ovs op %d failed: %s %s ops=%s", i, r.Error, r.Details, string(opsJSON))
	}
	return true, nil
}

func transactNoErr(ctx context.Context, c client.Client, ops ...ovsdb.Operation) error {
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	for i, r := range results {
		if r.Error != "" {
			opsJSON, _ := json.Marshal(ops)
			return fmt.Errorf("ovs op %d failed: %s %s ops=%s", i, r.Error, r.Details, string(opsJSON))
		}
	}
	return nil
}

func stringsTrim(v string) string {
	if v == "" {
		return ""
	}
	return v
}
