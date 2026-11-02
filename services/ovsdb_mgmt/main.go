package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	ovsdbpb "github.com/romanspb80/evpn-for-ovn/gen/ovsdb_mgmt"
	nbdb "github.com/romanspb80/evpn-for-ovn/services/ovsdb_mgmt/ovnnb"

	"github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
	"google.golang.org/grpc"
)

// OvsdbServer implements the OvsdbService gRPC server
type OvsdbServer struct {
	ovsdbpb.UnimplementedOvsdbServiceServer
	nbClient  client.Client
	ovsClient client.Client
	networks  map[string]*nbdb.LogicalSwitch
}

func (s *OvsdbServer) ensureOVNConnected(ctx context.Context) error {
	if s == nil || s.nbClient == nil {
		return fmt.Errorf("OVN northbound client is not initialized")
	}
	if err := s.nbClient.Connect(ctx); err != nil && !strings.Contains(err.Error(), "already connected") {
		return err
	}
	return nil
}

// AddVtep creates a VXLAN tunnel and LSP
func (s *OvsdbServer) AddVtep(ctx context.Context, req *ovsdbpb.AddVtepRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC AddVtep called: lsName=%s, lspName=%s, vxlanName=%s, remoteIP=%s, key=%s",
		req.LsName, req.LspName, req.VxlanName, req.RemoteIp, req.Key)
	if err := s.ensureOVNConnected(ctx); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVN client reconnect failed: %v", err)}, nil
	}

	// Get the LogicalSwitch by name
	// First check cache
	if cached, ok := s.networks[req.LsName]; ok {
		log.Printf("[DEBUG] AddVtep: Network %s found in cache", req.LsName)
		// Use cached value
		lsp, err := addVtep(s.nbClient, s.ovsClient, cached, req.LspName, req.VxlanName, req.RemoteIp, req.Key, req.DstPort)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
		}
		_ = lsp
		return &ovsdbpb.Status{Success: true, Message: "VTEP created successfully"}, nil
	}

	// Not in cache, query database using List() to bypass cache indexing lag
	log.Printf("[DEBUG] AddVtep: Network %s not in cache, querying database", req.LsName)
	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query networks: %v", err)}, nil
	}

	var ls *nbdb.LogicalSwitch
	for i := range allSwitches {
		if allSwitches[i].Name == req.LsName {
			ls = &allSwitches[i]
			log.Printf("[DEBUG] AddVtep: Found network %s in database", req.LsName)
			break
		}
	}

	if ls == nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LogicalSwitch %s not found", req.LsName)}, nil
	}

	lsp, err := addVtep(s.nbClient, s.ovsClient, ls, req.LspName, req.VxlanName, req.RemoteIp, req.Key, req.DstPort)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
	}

	// Store LSP in context for subsequent operations
	// Note: In production, use a proper cache or store
	_ = lsp

	return &ovsdbpb.Status{Success: true, Message: "VTEP created successfully"}, nil
}

// DelVtep deletes a VXLAN tunnel and LSP
func (s *OvsdbServer) DelVtep(ctx context.Context, req *ovsdbpb.DelVtepRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC DelVtep called: lsName=%s, lspName=%s, vxlanName=%s", req.LsName, req.LspName, req.VxlanName)

	// Get the LogicalSwitch by name
	// First check cache
	if cached, ok := s.networks[req.LsName]; ok {
		log.Printf("[DEBUG] DelVtep: Network %s found in cache", req.LsName)
		// Use cached value
		err := delVtep(s.nbClient, s.ovsClient, cached, req.LspName, req.VxlanName)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
		}
		return &ovsdbpb.Status{Success: true, Message: "VTEP deleted successfully"}, nil
	}

	// Not in cache, query database using List() to bypass cache indexing lag
	log.Printf("[DEBUG] DelVtep: Network %s not in cache, querying database", req.LsName)
	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query networks: %v", err)}, nil
	}

	var ls *nbdb.LogicalSwitch
	for i := range allSwitches {
		if allSwitches[i].Name == req.LsName {
			ls = &allSwitches[i]
			log.Printf("[DEBUG] DelVtep: Found network %s in database", req.LsName)
			break
		}
	}

	if ls == nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LogicalSwitch %s not found", req.LsName)}, nil
	}

	err := delVtep(s.nbClient, s.ovsClient, ls, req.LspName, req.VxlanName)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
	}

	return &ovsdbpb.Status{Success: true, Message: "VTEP deleted successfully"}, nil
}

// AddClient adds a MAC/IP pair to an LSP
func (s *OvsdbServer) AddClient(ctx context.Context, req *ovsdbpb.AddClientRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC AddClient called: lspName=%s, mac=%s, ip=%s", req.LspName, req.Mac, req.Ip)
	if err := s.ensureOVNConnected(ctx); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVN client reconnect failed: %v", err)}, nil
	}

	// Retrieve the LSP by name
	lsp, err := getVtep(s.nbClient, req.LspName)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LSP not found: %v", err)}, nil
	}

	err = addClient(s.nbClient, lsp, req.Mac, req.Ip)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
	}

	return &ovsdbpb.Status{Success: true, Message: "Client added successfully"}, nil
}

// AddLogicalSwitchPort creates a LogicalSwitchPort with given addresses and ensures an OVS port exists in br-int
func (s *OvsdbServer) AddLogicalSwitchPort(ctx context.Context, req *ovsdbpb.LogicalSwitchPort) (*ovsdbpb.Status, error) {
	log.Printf("gRPC AddLogicalSwitchPort called: name=%s, ls_name=%s, addresses=%v", req.Name, req.LsName, req.Addresses)
	if err := s.ensureOVNConnected(ctx); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVN client reconnect failed: %v", err)}, nil
	}

	lspName := req.Name
	hostIfaceName := ovnK8sIfaceName(lspName)
	ovsIfaceName := hostIfaceName
	lsName := truncateName(req.LsName)
	if lsName == "" {
		return &ovsdbpb.Status{Success: false, Message: "LogicalSwitch name (LsName) must be specified"}, nil
	}

	// Get the LogicalSwitch by name
	// First check cache
	var ls *nbdb.LogicalSwitch
	if cached, ok := s.networks[lsName]; ok {
		log.Printf("[DEBUG] AddLogicalSwitchPort: Network %s found in cache", lsName)
		ls = cached
	} else {
		// Not in cache, query database using List() to avoid cache indexing lag
		log.Printf("[DEBUG] AddLogicalSwitchPort: Network %s not in cache, querying database", lsName)
		var switchList []nbdb.LogicalSwitch
		if err := s.nbClient.List(ctx, &switchList); err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to find LogicalSwitch %s: %v", lsName, err)}, nil
		}

		for i := range switchList {
			if switchList[i].Name == lsName {
				ls = &switchList[i]
				log.Printf("[DEBUG] AddLogicalSwitchPort: Found network %s in database", lsName)
				break
			}
		}
	}

	if ls == nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LogicalSwitch %s not found", lsName)}, nil
	}

	// 1) Ensure LSP exists in NBDB; bypass the libovsdb cache with a full List()
	// so we don't accidentally find a stale row that was deleted externally (e.g. by ovn-nbctl).
	var allLsps []nbdb.LogicalSwitchPort
	if err := s.nbClient.List(ctx, &allLsps); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query NB for LSP list: %v", err)}, nil
	}
	var existing *nbdb.LogicalSwitchPort
	for i := range allLsps {
		if allLsps[i].Name == lspName {
			existing = &allLsps[i]
			break
		}
	}
	log.Printf("[DEBUG] AddLogicalSwitchPort: full scan found existing=%v for LSP %s", existing != nil, lspName)

	var lsp *nbdb.LogicalSwitchPort
	if existing != nil {
		lsp = existing
		// Merge addresses
		for _, a := range req.Addresses {
			parts := strings.SplitN(a, " ", 2)
			if len(parts) == 2 {
				if err := addClient(s.nbClient, lsp, parts[0], parts[1]); err != nil {
					return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
				}
			}
		}
		log.Printf("[DEBUG] AddLogicalSwitchPort: merged addresses into existing LSP %s (UUID=%s)", lspName, lsp.UUID)
	} else {
		// Create new LSP and attach to LogicalSwitch
		newUUID := uuid.New().String()
		lspRow := &nbdb.LogicalSwitchPort{Name: lspName, UUID: newUUID, Addresses: req.Addresses}
		createOps, err := s.nbClient.Create(lspRow)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build create op for LSP: %v", err)}, nil
		}
		mutateOps, err := s.nbClient.Where(ls).Mutate(ls, model.Mutation{
			Field:   &ls.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{newUUID},
		})
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build mutate op for LogicalSwitch: %v", err)}, nil
		}
		ops := append(createOps, mutateOps...)
		results, err := s.nbClient.Transact(ctx, ops...)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("nb transact failed: %v", err)}, nil
		}
		for i, r := range results {
			if r.Error != "" {
				// Attempt recovery if duplicate-name
				if strings.Contains(r.Error, "constraint violation") && strings.Contains(r.Details, "identical values") {
					// Re-scan with full List to avoid cache returning deleted row
					var reLsps []nbdb.LogicalSwitchPort
					if qerr := s.nbClient.List(ctx, &reLsps); qerr == nil {
						for j := range reLsps {
							if reLsps[j].Name == lspName {
								lsp = &reLsps[j]
								break
							}
						}
					}
					if lsp != nil {
						log.Printf("[DEBUG] AddLogicalSwitchPort: recovered existing LSP %s after constraint violation", lspName)
						break
					}
					return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("NB op %d constraint violation and recovery failed: %s", i, r.Error)}, nil
				}
				return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("NB op %d failed: %s %s", i, r.Error, r.Details)}, nil
			}
		}
		log.Printf("[DEBUG] AddLogicalSwitchPort: created LSP %s (UUID=%s), transact ops=%d", lspName, newUUID, len(results))
		// Fetch fresh row via full scan to bypass cache lag
		var freshLsps []nbdb.LogicalSwitchPort
		if ferr := s.nbClient.List(ctx, &freshLsps); ferr != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("created LSP but failed to retrieve: %v", ferr)}, nil
		}
		for i := range freshLsps {
			if freshLsps[i].Name == lspName {
				lsp = &freshLsps[i]
				break
			}
		}
		if lsp == nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("created LSP %s but not found in NB after transact", lspName)}, nil
		}
	}

	// 2) Ensure OVS interface/port exists and is attached to br-int.
	// For type=system, the OVS Interface name must match an existing Linux netdev name.
	// Keep OVN-Kubernetes binding model via external_ids:iface-id=<LogicalSwitchPort name>.
	if s.ovsClient == nil {
		return &ovsdbpb.Status{Success: false, Message: "OVS client is not initialized"}, nil
	}
	var allIfaces []vswitch.Interface
	if err := s.ovsClient.List(ctx, &allIfaces); err == nil {
		for _, candidate := range allIfaces {
			matchesIfaceID := candidate.ExternalIDs != nil && candidate.ExternalIDs["iface-id"] == lspName
			candidateOptName := ""
			if candidate.Options != nil {
				candidateOptName = candidate.Options["name"]
			}
			matchesStaleK8sName := strings.HasPrefix(candidate.Name, "k8s-") && candidateOptName == hostIfaceName
			if !matchesIfaceID && !matchesStaleK8sName {
				continue
			}
			if _, nerr := net.InterfaceByName(candidate.Name); nerr == nil {
				continue
			}
			log.Printf("Removing stale OVS interface %s for iface-id=%s (no such Linux netdev)", candidate.Name, lspName)
			var stalePorts []vswitch.Port
			if err := s.ovsClient.Where(&vswitch.Port{Name: candidate.Name}).List(ctx, &stalePorts); err == nil && len(stalePorts) > 0 {
				if delPortOps, derr := s.ovsClient.Where(&stalePorts[0]).Delete(); derr == nil {
					_, _ = s.ovsClient.Transact(ctx, delPortOps...)
				}
			}
			if delIfaceOps, derr := s.ovsClient.Where(&candidate).Delete(); derr == nil {
				_, _ = s.ovsClient.Transact(ctx, delIfaceOps...)
			}
		}
	}

	var ifaces []vswitch.Interface
	_ = s.ovsClient.Where(&vswitch.Interface{Name: hostIfaceName}).List(ctx, &ifaces)
	if len(ifaces) == 0 {
		// libovsdb cache/index can lag right after ovs-vsctl add-port; re-scan full table
		// before trying to create a new Interface row with the same name.
		var allIfacesScan []vswitch.Interface
		if err := s.ovsClient.List(ctx, &allIfacesScan); err == nil {
			for i := range allIfacesScan {
				if allIfacesScan[i].Name == hostIfaceName {
					ifaces = []vswitch.Interface{allIfacesScan[i]}
					log.Printf("Recovered OVS interface %s via full scan (UUID=%s)", allIfacesScan[i].Name, allIfacesScan[i].UUID)
					break
				}
			}
		}
	}

	if len(ifaces) > 0 {
		iface := ifaces[0]
		log.Printf("OVS interface %s already exists, UUID=%s", iface.Name, iface.UUID)

		// Ensure ExternalIDs contains iface-id -> LSP name (as in OVN-Kubernetes binding model).
		ifaceExternalIDs := map[string]string{"iface-id": lspName}
		desiredAttachedMac := ""
		if len(req.Addresses) > 0 {
			parts := strings.SplitN(req.Addresses[0], " ", 2)
			if len(parts) == 2 && parts[0] != "" {
				desiredAttachedMac = parts[0]
				ifaceExternalIDs["attached-mac"] = parts[0]
			}
		}
		currentAttachedMac := ""
		if iface.ExternalIDs != nil {
			currentAttachedMac = iface.ExternalIDs["attached-mac"]
		}
		if iface.ExternalIDs == nil || iface.ExternalIDs["iface-id"] != lspName || currentAttachedMac != desiredAttachedMac {
			upd := &vswitch.Interface{ExternalIDs: ifaceExternalIDs}
			updateOps, uerr := s.ovsClient.Where(&vswitch.Interface{UUID: iface.UUID}).Update(upd)
			if uerr == nil {
				reply, terr := s.ovsClient.Transact(ctx, updateOps...)
				if terr != nil {
					log.Printf("Warning: ovs transact failed updating interface external_ids: %v", terr)
				} else {
					for i, r := range reply {
						if r.Error != "" {
							log.Printf("Warning: failed to update interface external_ids op %d: %s %s", i, r.Error, r.Details)
						}
					}
				}
			} else {
				log.Printf("Warning: failed to build update op for interface external_ids: %v", uerr)
			}
		}

		// Ensure interface type (type=system)
		needUpdate := false
		updIface := &vswitch.Interface{}
		if iface.Type != "system" {
			needUpdate = true
			updIface.Type = "system"
		}
		if len(iface.Options) > 0 {
			needUpdate = true
			updIface.Options = map[string]string{}
		}
		if needUpdate {
			updateOps, uerr := s.ovsClient.Where(&vswitch.Interface{UUID: iface.UUID}).Update(updIface)
			if uerr == nil {
				reply, terr := s.ovsClient.Transact(ctx, updateOps...)
				if terr != nil {
					log.Printf("Warning: ovs transact failed updating interface: %v", terr)
				} else {
					for i, r := range reply {
						if r.Error != "" {
							log.Printf("Warning: failed to update interface op %d: %s %s", i, r.Error, r.Details)
						}
					}
				}
			} else {
				log.Printf("Warning: failed to build update op for interface: %v", uerr)
			}
		}

		// Ensure OVS port exists and references this interface.
		var existingPorts []vswitch.Port
		if err := s.ovsClient.Where(&vswitch.Port{Name: iface.Name}).List(ctx, &existingPorts); err == nil && len(existingPorts) > 0 {
			port := existingPorts[0]
			hasIface := false
			for _, ifaceUUID := range port.Interfaces {
				if ifaceUUID == iface.UUID {
					hasIface = true
					break
				}
			}
			if !hasIface {
				updPort := &vswitch.Port{Interfaces: []string{iface.UUID}}
				if portOps, perr := s.ovsClient.Where(&vswitch.Port{UUID: port.UUID}).Update(updPort); perr == nil {
					_, _ = s.ovsClient.Transact(ctx, portOps...)
				}
			}
		}
	} else {
		if _, nerr := net.InterfaceByName(hostIfaceName); nerr != nil {
			log.Printf("Warning: host interface %s not found for LSP %s yet; continuing with OVS port creation", hostIfaceName, lspName)
		}
		iface := &vswitch.Interface{
			Name: ovsIfaceName,
			Type: "system",
			ExternalIDs: map[string]string{
				"iface-id": lspName,
			},
		}
		if len(req.Addresses) > 0 {
			parts := strings.SplitN(req.Addresses[0], " ", 2)
			if len(parts) == 2 && parts[0] != "" {
				iface.ExternalIDs["attached-mac"] = parts[0]
			}
		}
		iface.UUID = uuid.New().String()
		ifaceOps, err := s.ovsClient.Create(iface)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build create op for interface: %v", err)}, nil
		}

		port := &vswitch.Port{Name: ovsIfaceName, Interfaces: []string{iface.UUID}}
		port.UUID = uuid.New().String()
		portOps, err := s.ovsClient.Create(port)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build create op for port: %v", err)}, nil
		}

		bridge := &vswitch.Bridge{Name: "br-int"}
		mutateBridge := model.Mutation{
			Field:   &bridge.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{port.UUID},
		}
		bridgeOps, err := s.ovsClient.Where(bridge).Mutate(bridge, mutateBridge)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build mutate op for bridge: %v", err)}, nil
		}

		ovsOps := append(ifaceOps, portOps...)
		ovsOps = append(ovsOps, bridgeOps...)

		ovsReply, err := s.ovsClient.Transact(ctx, ovsOps...)
		if err != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("ovs transact failed: %v", err)}, nil
		}
		for i, r := range ovsReply {
			if r.Error != "" {
				if strings.Contains(r.Error, "constraint violation") && strings.Contains(r.Details, "identical values") {
					// duplicate interface/port rows are a tolerated stale condition when
					// a previous run left the same OVN binding behind; recover without
					// failing the EVPN route processing flow.
					var postIfaces []vswitch.Interface
					if err := s.ovsClient.Where(&vswitch.Interface{Name: ovsIfaceName}).List(ctx, &postIfaces); err == nil && len(postIfaces) > 0 {
						log.Printf("Recovered: interface %s exists (UUID=%s)", ovsIfaceName, postIfaces[0].UUID)
						if postIfaces[0].ExternalIDs == nil || postIfaces[0].ExternalIDs["iface-id"] != lspName {
							upd := &vswitch.Interface{ExternalIDs: map[string]string{"iface-id": lspName}}
							if updateOps, uerr := s.ovsClient.Where(&vswitch.Interface{UUID: postIfaces[0].UUID}).Update(upd); uerr == nil {
								_, _ = s.ovsClient.Transact(ctx, updateOps...)
							}
						}
					} else {
						log.Printf("Warning: duplicate OVS interface error recovered without a fresh row lookup: %s %s", r.Error, r.Details)
					}
					var postPorts []vswitch.Port
					if err := s.ovsClient.Where(&vswitch.Port{Name: ovsIfaceName}).List(ctx, &postPorts); err == nil && len(postPorts) > 0 {
						log.Printf("Recovered: port %s exists (UUID=%s)", ovsIfaceName, postPorts[0].UUID)
					} else {
						log.Printf("Warning: port %s not found after duplicate-interface error", ovsIfaceName)
					}
					return &ovsdbpb.Status{Success: true, Message: fmt.Sprintf("LogicalSwitchPort existed; duplicate OVS interface recovered for %s", ovsIfaceName)}, nil
				}
				return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVS op %d failed: %s %s", i, r.Error, r.Details)}, nil
			}
		}
	}

	return &ovsdbpb.Status{Success: true, Message: "LogicalSwitchPort created and OVS port ensured"}, nil
}

// DelClient removes a MAC/IP pair from an LSP
func (s *OvsdbServer) DelClient(ctx context.Context, req *ovsdbpb.DelClientRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC DelClient called: lspName=%s, mac=%s, ip=%s", req.LspName, req.Mac, req.Ip)

	// Retrieve the LSP by name
	lsp, err := getVtep(s.nbClient, req.LspName)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LSP not found: %v", err)}, nil
	}

	err = delClient(s.nbClient, lsp, req.Mac, req.Ip)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: err.Error()}, nil
	}

	return &ovsdbpb.Status{Success: true, Message: "Client deleted successfully"}, nil
}

// GetLogicalSwitchPort retrieves a LogicalSwitchPort by name, returning its addresses.
func (s *OvsdbServer) GetLogicalSwitchPort(ctx context.Context, req *ovsdbpb.LogicalSwitchPortRequest) (*ovsdbpb.LogicalSwitchPortResponse, error) {
	log.Printf("gRPC GetLogicalSwitchPort called: name=%s", req.Name)

	var lspList []nbdb.LogicalSwitchPort
	if err := s.nbClient.Where(&nbdb.LogicalSwitchPort{Name: req.Name}).List(ctx, &lspList); err != nil || len(lspList) == 0 {
		// Fallback: scan all LSPs (avoids cache lag after creation)
		var allLsps []nbdb.LogicalSwitchPort
		if err2 := s.nbClient.List(ctx, &allLsps); err2 != nil {
			return &ovsdbpb.LogicalSwitchPortResponse{LogicalSwitchPort: nil}, nil
		}
		for _, l := range allLsps {
			if l.Name == req.Name {
				lspList = []nbdb.LogicalSwitchPort{l}
				break
			}
		}
	}
	if len(lspList) == 0 {
		return &ovsdbpb.LogicalSwitchPortResponse{LogicalSwitchPort: nil}, nil
	}
	lsp := lspList[0]
	return &ovsdbpb.LogicalSwitchPortResponse{
		LogicalSwitchPort: &ovsdbpb.LogicalSwitchPort{
			Name:      lsp.Name,
			Uuid:      lsp.UUID,
			Addresses: lsp.Addresses,
		},
	}, nil
}

// DelLogicalSwitchPort deletes a LogicalSwitchPort by name from OVN NB and removes the OVS port.
func (s *OvsdbServer) DelLogicalSwitchPort(ctx context.Context, req *ovsdbpb.LogicalSwitchPortRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC DelLogicalSwitchPort called: name=%s", req.Name)

	var lspList []nbdb.LogicalSwitchPort
	if err := s.nbClient.Where(&nbdb.LogicalSwitchPort{Name: req.Name}).List(ctx, &lspList); err != nil || len(lspList) == 0 {
		var allLsps []nbdb.LogicalSwitchPort
		if err2 := s.nbClient.List(ctx, &allLsps); err2 != nil {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to list LSPs: %v", err2)}, nil
		}
		for _, l := range allLsps {
			if l.Name == req.Name {
				lspList = []nbdb.LogicalSwitchPort{l}
				break
			}
		}
	}
	if len(lspList) == 0 {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("LSP %s not found", req.Name)}, nil
	}
	lsp := lspList[0]

	deleteOps, err := s.nbClient.Where(&lsp).Delete()
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build delete op: %v", err)}, nil
	}
	results, err := s.nbClient.Transact(ctx, deleteOps...)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("transact failed: %v", err)}, nil
	}
	for _, res := range results {
		if res.Error != "" {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVSDB error: %s", res.Error)}, nil
		}
	}

	// Also remove OVS port from br-int if it exists (by iface-id, OVN-K style iface name, or legacy truncated name).
	var ovsIfaces []vswitch.Interface
	ifaceNameCandidates := []string{ovnK8sIfaceName(req.Name), truncateName(req.Name), req.Name}
	for _, candidate := range ifaceNameCandidates {
		if candidate == "" {
			continue
		}
		if err := s.ovsClient.Where(&vswitch.Interface{Name: candidate}).List(ctx, &ovsIfaces); err == nil && len(ovsIfaces) > 0 {
			break
		}
	}
	if len(ovsIfaces) == 0 {
		var allIfaces []vswitch.Interface
		if err := s.ovsClient.List(ctx, &allIfaces); err == nil {
			for _, iface := range allIfaces {
				if iface.ExternalIDs != nil && iface.ExternalIDs["iface-id"] == req.Name {
					ovsIfaces = append(ovsIfaces, iface)
					break
				}
			}
		}
	}
	if len(ovsIfaces) > 0 {
		iface := ovsIfaces[0]
		var ovsPorts []vswitch.Port
		if err := s.ovsClient.Where(&vswitch.Port{Name: iface.Name}).List(ctx, &ovsPorts); err == nil && len(ovsPorts) > 0 {
			delPortOps, err := s.ovsClient.Where(&ovsPorts[0]).Delete()
			if err == nil {
				_, _ = s.ovsClient.Transact(ctx, delPortOps...)
			}
		}
	}

	log.Printf("Deleted LSP %s", req.Name)
	return &ovsdbpb.Status{Success: true, Message: fmt.Sprintf("LSP %s deleted", req.Name)}, nil
}

// GetVtep retrieves a specific LSP with its clients and associated VXLAN interface.
// Also ensures ExternalIDs["iface-id"] is set on the OVS interface for LSP association.
func (s *OvsdbServer) GetVtep(ctx context.Context, req *ovsdbpb.GetVtepRequest) (*ovsdbpb.GetVtepResponse, error) {
	log.Printf("gRPC GetVtep called: lspName=%s", req.LspName)

	lsp, err := getVtep(s.nbClient, req.LspName)
	if err != nil {
		return &ovsdbpb.GetVtepResponse{LogicalSwitchPort: nil}, nil
	}

	// Query OVS for associated VXLAN interface.
	// Preferred: by interface name == LSP name. Fallback: by ExternalIDs["iface-id"] == LSP name.
	var vxlanIfaces []vswitch.Interface
	ifaceErr := s.ovsClient.Where(&vswitch.Interface{Name: req.LspName}).List(ctx, &vxlanIfaces)
	if (ifaceErr != nil || len(vxlanIfaces) == 0) && ifaceErr == nil {
		var allVxlanIfaces []vswitch.Interface
		if err := s.ovsClient.Where(&vswitch.Interface{Type: "vxlan"}).List(ctx, &allVxlanIfaces); err == nil {
			for _, iface := range allVxlanIfaces {
				if iface.ExternalIDs != nil && iface.ExternalIDs["iface-id"] == req.LspName {
					vxlanIfaces = []vswitch.Interface{iface}
					break
				}
			}
		}
	}

	var vxlanInterface *ovsdbpb.VxlanInterface
	if ifaceErr == nil && len(vxlanIfaces) > 0 {
		iface := vxlanIfaces[0]
		vxlanInterface = &ovsdbpb.VxlanInterface{
			Name:     iface.Name,
			Uuid:     iface.UUID,
			Type:     iface.Type,
			RemoteIp: iface.Options["remote_ip"],
			Key:      iface.Options["key"],
			DstPort:  iface.Options["dst_port"],
		}
		log.Printf("Found associated VXLAN interface: %s (UUID=%s)", iface.Name, iface.UUID)

		// Auto-fix: ensure ExternalIDs["iface-id"] is set for LSP association
		if iface.ExternalIDs == nil || iface.ExternalIDs["iface-id"] != req.LspName {
			log.Printf("Auto-fixing ExternalIDs for interface %s: setting iface-id=%s", iface.Name, req.LspName)
			updIface := &vswitch.Interface{
				ExternalIDs: map[string]string{"iface-id": req.LspName},
			}
			updateOps, uerr := s.ovsClient.Where(&vswitch.Interface{Name: iface.Name}).Update(updIface)
			if uerr == nil {
				reply, terr := s.ovsClient.Transact(ctx, updateOps...)
				if terr != nil {
					log.Printf("Warning: ovs transact failed updating interface external_ids: %v", terr)
				} else {
					for i, r := range reply {
						if r.Error != "" {
							log.Printf("Warning: failed to update interface external_ids op %d: %s %s", i, r.Error, r.Details)
						} else {
							log.Printf("Successfully updated interface %s external_ids", iface.Name)
						}
					}
				}
			} else {
				log.Printf("Warning: failed to build update op for interface external_ids: %v", uerr)
			}
		}
	} else if ifaceErr != nil {
		log.Printf("Warning: failed to query VXLAN interface for LSP %s: %v", req.LspName, ifaceErr)
	}

	return &ovsdbpb.GetVtepResponse{
		LogicalSwitchPort: &ovsdbpb.LogicalSwitchPort{
			Name:      lsp.Name,
			Uuid:      lsp.UUID,
			Addresses: lsp.Addresses,
		},
		VxlanInterface: vxlanInterface,
	}, nil
}

// GetVteps retrieves all VTEPs
func (s *OvsdbServer) GetVteps(ctx context.Context, _ *ovsdbpb.Empty) (*ovsdbpb.GetVtepsResponse, error) {
	log.Printf("gRPC GetVteps called")

	vteps, err := getVteps(s.nbClient, s.ovsClient)
	if err != nil {
		return &ovsdbpb.GetVtepsResponse{LogicalSwitchPorts: nil}, nil
	}

	var pbVteps []*ovsdbpb.LogicalSwitchPort
	for _, v := range vteps {
		pbVteps = append(pbVteps, &ovsdbpb.LogicalSwitchPort{
			Name:      v.Name,
			Uuid:      v.UUID,
			Addresses: v.Addresses,
		})
	}

	return &ovsdbpb.GetVtepsResponse{LogicalSwitchPorts: pbVteps}, nil
}

// AddNetwork creates a new Logical Switch with the given name
func logicalSwitchVni() string {
	vni := strings.TrimSpace(os.Getenv("DEFAULT_DYNAMIC_ROUTING_VNI"))
	if vni == "" {
		vni = strings.TrimSpace(os.Getenv("DYNAMIC_ROUTING_VNI"))
	}
	if vni == "" {
		vni = "1000"
	}
	return vni
}

func dynamicRoutingIfNames(vni string) (string, string, string) {
	return fmt.Sprintf("br-evpn%s", vni), fmt.Sprintf("vxlan%s", vni), fmt.Sprintf("evpnadv%s", vni)
}

func (s *OvsdbServer) AddNetwork(ctx context.Context, req *ovsdbpb.AddNetworkRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC AddNetwork called: name=%s", req.Name)

	networkName := truncateName(req.Name)

	// Check if network already exists (use full List to avoid cache/index lag issues)
	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query NB for LogicalSwitch list: %v", err)}, nil
	}

	var ls *nbdb.LogicalSwitch
	var matching []*nbdb.LogicalSwitch
	for i := range allSwitches {
		if allSwitches[i].Name == networkName {
			matching = append(matching, &allSwitches[i])
		}
	}

	if len(matching) > 0 {
		// Reuse existing switch, never create new one when same name already exists.
		ls = matching[0]
		log.Printf("Logical switch %s already exists with UUID=%s (matches=%d), reusing existing", networkName, ls.UUID, len(matching))
	} else {
		// Create new Logical Switch
		log.Printf("[DEBUG] Creating new LogicalSwitch: %s", networkName)
		vni := logicalSwitchVni()
		bridgeIf, vxlanIf, advertiseIf := dynamicRoutingIfNames(vni)
		ls = &nbdb.LogicalSwitch{
			Name: networkName,
			// OVN Dynamic Routing canonical configuration: per-VNI EVPN settings live in the
			// Logical_Switch.other_config, while the OVS EVPN tunnel setup remains a
			// shared chassis concern owned by ovn-controller.
			OtherConfig: map[string]string{
				"dynamic-routing-vni":              vni,
				"dynamic-routing-bridge-ifname":    bridgeIf,
				"dynamic-routing-vxlan-ifname":     vxlanIf,
				"dynamic-routing-advertise-ifname": advertiseIf,
				"dynamic-routing-redistribute":     "fdb,ip",
			},
		}
		ls.UUID = uuid.New().String() // Required for Create() operation to work
		log.Printf("[DEBUG] Generated UUID: %s", ls.UUID)

		lsOps, err := s.nbClient.Create(ls)
		if err != nil {
			log.Printf("[DEBUG] Create() failed: %v", err)
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build create op: %v", err)}, nil
		}
		log.Printf("[DEBUG] Create() succeeded, operations: %v", len(lsOps))

		results, err := s.nbClient.Transact(ctx, lsOps...)
		if err != nil {
			log.Printf("[DEBUG] Transact() failed: %v", err)
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("transact failed: %v", err)}, nil
		}
		log.Printf("[DEBUG] Transact() succeeded, results count: %d", len(results))

		// Check transaction results
		for i, res := range results {
			log.Printf("[DEBUG] Result %d: Error='%s' Count=%d", i, res.Error, res.Count)
			if res.Error != "" {
				log.Printf("[DEBUG] Transaction error at index %d: %s - %s", i, res.Error, res.Details)
				return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVSDB error: %s - %s", res.Error, res.Details)}, nil
			}
		}

		// Query ALL networks (bypass cache indexing issue with Where()).
		// After Create(), the new row exists in DB but may not be indexed in client cache yet.
		log.Printf("[DEBUG] Querying all networks to find UUID=%s", ls.UUID)
		var allNetworks []nbdb.LogicalSwitch
		if err := s.nbClient.List(ctx, &allNetworks); err != nil {
			log.Printf("[DEBUG] List() failed: %v", err)
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query networks: %v", err)}, nil
		}

		log.Printf("[DEBUG] List() returned %d total networks", len(allNetworks))

		// Find our newly created network by UUID (avoids cache indexing issue)
		found := false
		for _, net := range allNetworks {
			if net.UUID == ls.UUID {
				ls = &net
				found = true
				log.Printf("[DEBUG] Found created network by UUID: name='%s'", ls.Name)
				break
			}
		}

		if !found {
			log.Printf("[DEBUG] ERROR: Created network not found by UUID! Networks in database:")
			for i, net := range allNetworks {
				log.Printf("[DEBUG]   Network %d: name='%s', uuid=%s", i, net.Name, net.UUID)
			}
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("created network found in database but verification failed")}, nil
		}

		log.Printf("Created Logical switch %s with UUID=%s", networkName, ls.UUID)
	}

	// Ensure EVPN configuration exists on the Logical Switch
	needsUpdate := false
	if ls.OtherConfig == nil {
		ls.OtherConfig = make(map[string]string)
		needsUpdate = true
	}

	// Check and ensure all dynamic-routing parameters exist
	currentVNI := logicalSwitchVni()
	bridgeIf, vxlanIf, advertiseIf := dynamicRoutingIfNames(currentVNI)
	requiredConfig := map[string]string{
		"dynamic-routing-vni":              currentVNI,
		"dynamic-routing-bridge-ifname":    bridgeIf,
		"dynamic-routing-vxlan-ifname":     vxlanIf,
		"dynamic-routing-advertise-ifname": advertiseIf,
		"dynamic-routing-redistribute":     "fdb,ip",
	}

	for key, value := range requiredConfig {
		if ls.OtherConfig[key] != value {
			ls.OtherConfig[key] = value
			needsUpdate = true
			log.Printf("[DEBUG] Updated OtherConfig[%s] = %s", key, value)
		}
	}

	// If we need to update, perform transaction
	if needsUpdate {
		log.Printf("[DEBUG] Updating LogicalSwitch %s with EVPN configuration", networkName)
		updOps, err := s.nbClient.Where(ls).Update(ls)
		if err != nil {
			log.Printf("[DEBUG] Failed to build update op: %v", err)
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build update op: %v", err)}, nil
		}

		results, err := s.nbClient.Transact(ctx, updOps...)
		if err != nil {
			log.Printf("[DEBUG] Transact() update failed: %v", err)
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("transact update failed: %v", err)}, nil
		}

		for i, res := range results {
			if res.Error != "" {
				log.Printf("[DEBUG] Update transaction error at index %d: %s - %s", i, res.Error, res.Details)
				return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVSDB update error: %s - %s", res.Error, res.Details)}, nil
			}
		}
		log.Printf("Updated LogicalSwitch %s EVPN configuration", networkName)
	}

	// Store network in server's map (with real database UUID)
	s.networks[networkName] = ls

	return &ovsdbpb.Status{Success: true, Message: "Network created successfully"}, nil
}

// GetNetwork retrieves a Logical Switch by name
func (s *OvsdbServer) GetNetwork(ctx context.Context, req *ovsdbpb.GetNetworkRequest) (*ovsdbpb.GetNetworkResponse, error) {
	log.Printf("gRPC GetNetwork called: name=%s", req.Name)

	networkName := truncateName(req.Name)

	// First check server cache
	if ls, ok := s.networks[networkName]; ok {
		return &ovsdbpb.GetNetworkResponse{
			Network: &ovsdbpb.Network{
				Name: ls.Name,
				Uuid: ls.UUID,
			},
		}, nil
	}

	// If not in cache, query database
	log.Printf("[DEBUG] GetNetwork: %s not in cache, querying database", networkName)
	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.GetNetworkResponse{Network: nil}, nil
	}

	for i := range allSwitches {
		if allSwitches[i].Name == networkName {
			ls := &allSwitches[i]
			s.networks[networkName] = ls
			log.Printf("[DEBUG] GetNetwork: Found %s in database", networkName)
			return &ovsdbpb.GetNetworkResponse{
				Network: &ovsdbpb.Network{
					Name: ls.Name,
					Uuid: ls.UUID,
				},
			}, nil
		}
	}

	return &ovsdbpb.GetNetworkResponse{Network: nil}, nil
}

// GetNetworks retrieves all Logical Switches
func (s *OvsdbServer) GetNetworks(ctx context.Context, req *ovsdbpb.Empty) (*ovsdbpb.GetNetworksResponse, error) {
	log.Printf("gRPC GetNetworks called")

	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.GetNetworksResponse{Networks: nil}, nil
	}

	// Update server cache
	for i := range allSwitches {
		s.networks[allSwitches[i].Name] = &allSwitches[i]
	}

	var pbNetworks []*ovsdbpb.Network
	for _, sw := range allSwitches {
		pbNetworks = append(pbNetworks, &ovsdbpb.Network{
			Name: sw.Name,
			Uuid: sw.UUID,
		})
	}

	return &ovsdbpb.GetNetworksResponse{Networks: pbNetworks}, nil
}

// DelNetwork deletes a Logical Switch by name
func (s *OvsdbServer) DelNetwork(ctx context.Context, req *ovsdbpb.DelNetworkRequest) (*ovsdbpb.Status, error) {
	log.Printf("gRPC DelNetwork called: name=%s", req.Name)

	networkName := truncateName(req.Name)

	// Query database for LogicalSwitch using List() to avoid cache indexing lag
	log.Printf("[DEBUG] DelNetwork: Querying database for %s", networkName)
	var allSwitches []nbdb.LogicalSwitch
	if err := s.nbClient.List(ctx, &allSwitches); err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to query NB: %v", err)}, nil
	}

	var ls *nbdb.LogicalSwitch
	for i := range allSwitches {
		if allSwitches[i].Name == networkName {
			ls = &allSwitches[i]
			log.Printf("[DEBUG] DelNetwork: Found %s in database", networkName)
			break
		}
	}

	if ls == nil {
		return &ovsdbpb.Status{Success: false, Message: "LogicalSwitch not found"}, nil
	}

	// Delete LSP (via cascade)
	deleteOps, err := s.nbClient.Where(ls).Delete()
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("failed to build delete op: %v", err)}, nil
	}

	results, err := s.nbClient.Transact(ctx, deleteOps...)
	if err != nil {
		return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("transact failed: %v", err)}, nil
	}

	for _, res := range results {
		if res.Error != "" {
			return &ovsdbpb.Status{Success: false, Message: fmt.Sprintf("OVSDB error: %s - %s", res.Error, res.Details)}, nil
		}
	}

	// Remove from server cache
	delete(s.networks, networkName)

	log.Printf("Deleted Logical switch %s", networkName)
	return &ovsdbpb.Status{Success: true, Message: "Network deleted successfully"}, nil
}

// truncateName truncates a name to max 13 characters (OVS naming limit)
func truncateName(name string) string {
	if len(name) <= 13 {
		return name
	}
	return name[:13]
}

// ovnK8sIfaceName returns a deterministic short OVS interface name in OVN-Kubernetes style.
// The LogicalSwitchPort name remains full; OVS binds using external_ids:iface-id=<LSP name>.
func ovnK8sIfaceName(lspName string) string {
	sum := sha1.Sum([]byte(lspName))
	return "k8s-" + hex.EncodeToString(sum[:])[:9]
}

// addClient adds MAC and IP addresses to an existing logical switch port
// It appends the new address to existing Addresses; if already present, it's a no-op.
func addClient(nbClient client.Client, lsp *nbdb.LogicalSwitchPort, mac string, ip string) error {
	// Fetch freshest LSP row to avoid stomping concurrent changes
	latest, err := getVtep(nbClient, lsp.Name)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s %s", mac, ip)

	// If already present, no-op
	for _, a := range latest.Addresses {
		if a == addr {
			return nil
		}
	}

	newAddresses := append([]string{}, latest.Addresses...)
	newAddresses = append(newAddresses, addr)

	lspWithAddresses := &nbdb.LogicalSwitchPort{
		Addresses: newAddresses,
	}

	// Update by UUID to be precise
	lspUpdateOps, err := nbClient.Where(&nbdb.LogicalSwitchPort{UUID: latest.UUID}).Update(lspWithAddresses)
	if err != nil {
		return err
	}
	results, err := nbClient.Transact(context.Background(), lspUpdateOps...)
	if err != nil {
		return err
	}
	for _, res := range results {
		if res.Error != "" {
			return fmt.Errorf("OVSDB Error: %s - %s", res.Error, res.Details)
		}
	}
	return nil
}

// addVtep creates (or reuses) a logical switch port, links it to the switch,
// and creates a VXLAN interface mapped to the LSP. If LSP or VXLAN iface/port
// already exist, they are reused. Race conditions are handled by re-querying on constraint errors.
// Names are truncated to 13 characters for OVS compatibility.
func addVtep(nbClient client.Client, ovsClient client.Client, ls *nbdb.LogicalSwitch, lspName string, vxlanName string, remoteIP string, key string, dstPort string) (*nbdb.LogicalSwitchPort, error) {
	ctx := context.Background()

	// Truncate names to 13 chars max (OVS limit)
	lspName = truncateName(lspName)
	vxlanName = truncateName(vxlanName)

	// 1) Check whether an LSP with this name already exists in NB DB
	var existingLSPs []nbdb.LogicalSwitchPort
	if err := nbClient.Where(&nbdb.LogicalSwitchPort{Name: lspName}).List(ctx, &existingLSPs); err != nil {
		// If the list fails, return error
		return nil, fmt.Errorf("failed to query existing LogicalSwitchPort: %w", err)
	}

	var lspUUID string
	var returnLSP *nbdb.LogicalSwitchPort
	var needsCreate bool
	skipOVSCreate := false

	if len(existingLSPs) > 0 {
		// Reuse existing LSP (first matched)
		returnLSP = &existingLSPs[0]
		lspUUID = returnLSP.UUID
		needsCreate = false
		// If LSP already exists we assume the corresponding OVS objects were provisioned previously;
		// skip creating the OVS Interface/Port to avoid duplicate-name errors.
		skipOVSCreate = true
		log.Printf("LSP %s already exists (UUID=%s), reusing and skipping OVS create", lspName, lspUUID)
	} else {
		// Mark for creation; we'll try to create it
		needsCreate = true
	}

	// 2) Attempt to attach LSP to LogicalSwitch (works whether LSP exists or will be created)
	// If LSP doesn't exist yet but will be created, use a new UUID for now
	if needsCreate {
		lspUUID = uuid.New().String()
	}

	mutateOps, err := nbClient.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{lspUUID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build mutate op for LogicalSwitch: %w", err)
	}

	// 3) Build the transaction: create LSP (if needed) + mutate LS
	nbOps := make([]ovsdb.Operation, 0)
	if needsCreate {
		lsp := &nbdb.LogicalSwitchPort{Name: lspName}
		lsp.UUID = lspUUID
		createOps, err := nbClient.Create(lsp)
		if err != nil {
			return nil, fmt.Errorf("failed to build create op for LSP: %w", err)
		}
		nbOps = append(nbOps, createOps...)
		returnLSP = lsp
	}
	nbOps = append(nbOps, mutateOps...)

	// 4) Transact NB ops. If constraint violation occurs, refresh from DB and retry the mutate
	if len(nbOps) > 0 {
		results, err := nbClient.Transact(ctx, nbOps...)
		if err != nil {
			// Transact-level failure (network, etc.)
			return nil, fmt.Errorf("nb transact failed: %w", err)
		}

		// Inspect results for errors
		for i, res := range results {
			if res.Error == "" {
				continue
			}

			// If this is a duplicate-name constraint on Logical_Switch_Port, we have a race
			if strings.Contains(res.Error, "constraint violation") && strings.Contains(res.Details, "identical values") && strings.Contains(res.Details, "Logical_Switch_Port") {
				log.Printf("NB transaction duplicate-name error at op %d (race detected): %s; re-querying NB", i, res.Error)

				// Re-query NB for the LSP by name to get the actual UUID
				var found []nbdb.LogicalSwitchPort
				if qerr := nbClient.Where(&nbdb.LogicalSwitchPort{Name: lspName}).List(ctx, &found); qerr == nil && len(found) > 0 {
					foundLSP := &found[0]
					log.Printf("Recovered: LogicalSwitchPort %s exists with UUID=%s", lspName, foundLSP.UUID)

					// Now ensure this LSP is attached to the LogicalSwitch
					// (even if the mutate in the original transaction failed)
					mutateRetryOps, merr := nbClient.Where(ls).Mutate(ls, model.Mutation{
						Field:   &ls.Ports,
						Mutator: ovsdb.MutateOperationInsert,
						Value:   []string{foundLSP.UUID},
					})
					if merr == nil {
						retryResults, rerr := nbClient.Transact(ctx, mutateRetryOps...)
						if rerr == nil {
							for _, retryRes := range retryResults {
								if retryRes.Error == "" {
									log.Printf("Successfully attached existing LSP %s to LogicalSwitch", lspName)
								} else {
									log.Printf("Warning: failed to attach LSP %s to LogicalSwitch: %s", lspName, retryRes.Error)
								}
							}
						} else {
							log.Printf("Warning: mutate retry transact error: %v", rerr)
						}
					} else {
						log.Printf("Warning: failed to build mutate retry op: %v", merr)
					}

					returnLSP = foundLSP
					// since we recovered, continue
					break
				}
				// If recovery query didn't find the row, that's unexpected
				return nil, fmt.Errorf("OVN NB op %d: constraint violation detected but recovery query did not find LogicalSwitchPort %q: %s", i, lspName, res.Error)
			}

			// Non-constraint-violation error -> fail
			return nil, fmt.Errorf("OVN NB op %d failed: %s %s", i, res.Error, res.Details)
		}
	}

	// 5) Create or ensure VXLAN interface/port on OVS and associate it with LSP
	if skipOVSCreate {
		log.Printf("LSP %s already exists; ensuring OVS interface/port association", lspName)
	}

	// Check for existing Interface by exact name via full scan to avoid cache lag.
	var existingIfaces []vswitch.Interface
	ifaceExists := false
	var resolvedIface *vswitch.Interface
	if err := ovsClient.List(ctx, &existingIfaces); err == nil {
		for _, iface := range existingIfaces {
			if iface.Name == vxlanName {
				ifaceExists = true
				tmp := iface
				resolvedIface = &tmp
				log.Printf("OVS interface %s already exists, reusing UUID=%s", vxlanName, iface.UUID)
				break
			}
		}
	}
	if ifaceExists && resolvedIface != nil {
		iface := *resolvedIface

		// Ensure ExternalIDs contains iface-id -> LSP name and proper type/options
		needUpdate := false
		updIface := &vswitch.Interface{}
		if iface.ExternalIDs == nil || iface.ExternalIDs["iface-id"] != lspName {
			needUpdate = true
			if updIface.ExternalIDs == nil {
				updIface.ExternalIDs = map[string]string{"iface-id": lspName}
			} else {
				updIface.ExternalIDs["iface-id"] = lspName
			}
		}
		if iface.Type != "vxlan" {
			needUpdate = true
			updIface.Type = "vxlan"
		}
		if iface.Options == nil || iface.Options["remote_ip"] != remoteIP || iface.Options["key"] != key || iface.Options["dst_port"] != dstPort {
			needUpdate = true
			updIface.Options = map[string]string{"remote_ip": remoteIP, "key": key, "dst_port": dstPort}
		}
		if needUpdate {
			updateOps, uerr := ovsClient.Where(&vswitch.Interface{Name: iface.Name}).Update(updIface)
			if uerr == nil {
				reply, terr := ovsClient.Transact(ctx, updateOps...)
				if terr != nil {
					log.Printf("Warning: ovs transact failed updating interface: %v", terr)
				} else {
					for i, r := range reply {
						if r.Error != "" {
							log.Printf("Warning: failed to update interface op %d: %s %s", i, r.Error, r.Details)
						}
					}
				}
			} else {
				log.Printf("Warning: failed to build update op for interface: %v", uerr)
			}
		}
	} else if err != nil {
		log.Printf("Warning: failed to query existing interfaces: %v -- will attempt create", err)
	}
	// Legacy-compat recovery: the same VXLAN tuple may already exist under a
	// different node-derived name. Reuse it instead of creating a duplicate device.
	if !ifaceExists {
		var allVxlanIfaces []vswitch.Interface
		if err := ovsClient.List(ctx, &allVxlanIfaces); err == nil {
			for _, iface := range allVxlanIfaces {
				if iface.Name == vxlanName || (iface.Options["remote_ip"] == remoteIP && iface.Options["key"] == key && iface.Options["dst_port"] == dstPort) {
					ifaceExists = true
					tmp := iface
					resolvedIface = &tmp
					log.Printf("Reusing existing VXLAN interface: name=%s uuid=%s (requested=%s)", iface.Name, iface.UUID, vxlanName)
					updIface := &vswitch.Interface{ExternalIDs: map[string]string{"iface-id": lspName}}
					updateOps, uerr := ovsClient.Where(&vswitch.Interface{Name: iface.Name}).Update(updIface)
					if uerr == nil {
						if reply, terr := ovsClient.Transact(ctx, updateOps...); terr == nil {
							for i, r := range reply {
								if r.Error != "" {
									log.Printf("Warning: failed to update iface-id for reused iface op %d: %s %s", i, r.Error, r.Details)
								}
							}
						} else {
							log.Printf("Warning: failed to transact iface-id update for reused iface %s: %v", iface.Name, terr)
						}
					} else {
						log.Printf("Warning: failed to build iface-id update op for reused iface %s: %v", iface.Name, uerr)
					}
					break
				}
			}
		}
	}

	// Check for existing Port by name
	var existingPorts []vswitch.Port
	portExists := false
	if err := ovsClient.List(ctx, &existingPorts); err == nil {
		for _, port := range existingPorts {
			if port.Name == vxlanName {
				portExists = true
				log.Printf("OVS port %s already exists, reusing UUID=%s", vxlanName, port.UUID)
				break
			}
		}
	} else if err != nil {
		log.Printf("Warning: failed to query existing ports: %v -- will attempt create", err)
	}
	// If we reused a differently named interface, detect an existing port bound to it.
	if !portExists && resolvedIface != nil {
		var allPorts []vswitch.Port
		if err := ovsClient.List(ctx, &allPorts); err == nil {
			for _, p := range allPorts {
				for _, ifUUID := range p.Interfaces {
					if ifUUID == resolvedIface.UUID {
						portExists = true
						log.Printf("Reusing existing OVS port %s bound to interface %s", p.Name, resolvedIface.Name)
						break
					}
				}
				if portExists {
					break
				}
			}
		}
	}

	// Create only the missing OVS objects. Never create a second Interface row
	// for an existing VXLAN name: that is what caused the duplicate-name failures.
	var createOps []ovsdb.Operation
	var targetIfaceUUID string

	if ifaceExists {
		targetIfaceUUID = resolvedIface.UUID
	} else {
		iface := &vswitch.Interface{
			Name: vxlanName,
			Type: "vxlan",
			Options: map[string]string{
				"remote_ip": remoteIP,
				"key":       key,
				"dst_port":  dstPort,
			},
			ExternalIDs: map[string]string{
				"iface-id": lspName,
			},
		}
		iface.UUID = uuid.New().String()
		targetIfaceUUID = iface.UUID
		ifaceOps, err := ovsClient.Create(iface)
		if err != nil {
			return nil, fmt.Errorf("failed to build create op for interface: %w", err)
		}
		createOps = append(createOps, ifaceOps...)
	}

	if !portExists {
		port := &vswitch.Port{
			Name:       vxlanName,
			Interfaces: []string{targetIfaceUUID},
		}
		port.UUID = uuid.New().String()
		portOps, err := ovsClient.Create(port)
		if err != nil {
			return nil, fmt.Errorf("failed to build create op for port: %w", err)
		}

		bridge := &vswitch.Bridge{Name: "br-int"}
		mutateBridge := model.Mutation{
			Field:   &bridge.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{port.UUID},
		}
		bridgeOps, err := ovsClient.Where(bridge).Mutate(bridge, mutateBridge)
		if err != nil {
			return nil, fmt.Errorf("failed to build mutate op for bridge: %w", err)
		}

		createOps = append(createOps, portOps...)
		createOps = append(createOps, bridgeOps...)
	} else if !ifaceExists {
		existingPort := existingPorts[0]
		updPort := &vswitch.Port{Interfaces: []string{targetIfaceUUID}}
		portUpdateOps, err := ovsClient.Where(&vswitch.Port{UUID: existingPort.UUID}).Update(updPort)
		if err != nil {
			return nil, fmt.Errorf("failed to build update op for existing port: %w", err)
		}
		createOps = append(createOps, portUpdateOps...)
	} else if ifaceExists {
		// Ensure the existing port references the existing interface.
		var existingPort = existingPorts[0]
		hasIface := false
		for _, ifaceUUID := range existingPort.Interfaces {
			if ifaceUUID == targetIfaceUUID {
				hasIface = true
				break
			}
		}
		if !hasIface {
			updPort := &vswitch.Port{Interfaces: []string{targetIfaceUUID}}
			portUpdateOps, err := ovsClient.Where(&vswitch.Port{UUID: existingPort.UUID}).Update(updPort)
			if err != nil {
				return nil, fmt.Errorf("failed to build update op for existing port: %w", err)
			}
			createOps = append(createOps, portUpdateOps...)
		}
	}

	if len(createOps) > 0 {
		ovsReply, err := ovsClient.Transact(ctx, createOps...)
		if err != nil {
			return nil, fmt.Errorf("ovs transact failed: %w", err)
		}
		for i, r := range ovsReply {
			if r.Error != "" {
				if strings.Contains(r.Error, "constraint violation") && strings.Contains(r.Details, "identical values") {
					// The constraint violation itself proves the interface already exists —
					// the cache was stale and we tried to create a duplicate.
					// Treat this as success: the OVS object is already in place.
					log.Printf("OVS duplicate-name on %s (stale cache): interface/port already present, continuing", vxlanName)
					break
				}
				return nil, fmt.Errorf("OVS op %d failed: %s %s", i, r.Error, r.Details)
			}
		}
	}

	// 6) Return the logical switch port. Ensure we have a fresh copy from the DB.
	return getVtep(nbClient, lspName)
}

// delVtep deletes a VXLAN tunnel (VTEP) and its associated logical switch port
func delVtep(nbClient client.Client, ovsClient client.Client, ls *nbdb.LogicalSwitch, lspName string, vxlanName string) error {
	ctx := context.Background()

	// Find the LSP to get UUID
	lsp, err := getVtep(nbClient, lspName)
	if err != nil {
		return fmt.Errorf("lsp lookup failed: %w", err)
	}

	// Remove from LS (use the real UUID)
	mutateOps, err := nbClient.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return err
	}

	// Delete LSP by UUID
	deleteOps, err := nbClient.Where(&nbdb.LogicalSwitchPort{UUID: lsp.UUID}).Delete()
	if err != nil {
		return err
	}

	ops := append(mutateOps, deleteOps...)

	results, err := nbClient.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	for _, res := range results {
		if res.Error != "" {
			return fmt.Errorf("OVSDB Error: %s - %s", res.Error, res.Details)
		}
	}

	// Delete OVS port by name (if present)
	port := &vswitch.Port{Name: vxlanName}
	deletePortOps, err := ovsClient.Where(port).Delete()
	if err != nil {
		return err
	}
	ovsResults, err := ovsClient.Transact(ctx, deletePortOps...)
	if err != nil {
		return err
	}
	for _, res := range ovsResults {
		if res.Error != "" {
			return fmt.Errorf("OVS Error: %s - %s", res.Error, res.Details)
		}
	}

	return nil
}

// delClient removes a specific MAC/IP address pair from a logical switch port
func delClient(nbClient client.Client, lsp *nbdb.LogicalSwitchPort, mac string, ip string) error {
	// Refresh latest
	latest, err := getVtep(nbClient, lsp.Name)
	if err != nil {
		return err
	}

	addressToRemove := fmt.Sprintf("%s %s", mac, ip)

	// Create new addresses list without the specified pair
	var newAddresses []string
	for _, addr := range latest.Addresses {
		if addr != addressToRemove {
			newAddresses = append(newAddresses, addr)
		}
	}

	// If no change, no-op
	if len(newAddresses) == len(latest.Addresses) {
		return nil
	}

	lspWithAddresses := &nbdb.LogicalSwitchPort{
		Addresses: newAddresses,
	}
	lspUpdateOps, err := nbClient.Where(&nbdb.LogicalSwitchPort{UUID: latest.UUID}).Update(lspWithAddresses)
	if err != nil {
		return err
	}
	results, err := nbClient.Transact(context.Background(), lspUpdateOps...)
	if err != nil {
		return err
	}
	for _, res := range results {
		if res.Error != "" {
			return fmt.Errorf("OVSDB Error: %s - %s", res.Error, res.Details)
		}
	}
	return nil
}

// getVtep retrieves a specific logical switch port with its attached clients (addresses)
func getVtep(nbClient client.Client, lspName string) (*nbdb.LogicalSwitchPort, error) {
	ctx := context.Background()
	var lspList []nbdb.LogicalSwitchPort

	// Retry a few times to allow eventual consistency after a create/mutate transaction.
	for i := 0; i < 10; i++ {
		err := nbClient.
			Where(&nbdb.LogicalSwitchPort{Name: lspName}).
			List(ctx, &lspList)

		if err == nil && len(lspList) > 0 {
			return &lspList[0], nil
		}

		if err != nil {
			// Log transient query error and retry
			log.Printf("getVtep: query attempt %d failed: %v", i+1, err)
		} else {
			// No error but empty result: still retry (row may not be visible yet)
			log.Printf("getVtep: attempt %d: LSP %s not found yet", i+1, lspName)
		}

		time.Sleep(200 * time.Millisecond)
	}

	return nil, fmt.Errorf("LSP %q not found after retries", lspName)
}

// getVteps retrieves all logical switch ports that have corresponding VXLAN interfaces
func getVteps(nbClient client.Client, ovsClient client.Client) ([]*nbdb.LogicalSwitchPort, error) {
	// First, find all VXLAN interfaces
	var vxlanInterfaces []vswitch.Interface
	err := ovsClient.Where(&vswitch.Interface{Type: "vxlan"}).List(context.Background(), &vxlanInterfaces)
	if err != nil {
		return nil, err
	}

	// Extract LSP names from VXLAN interface external IDs
	var lspNames []string
	for _, iface := range vxlanInterfaces {
		if ifaceID, exists := iface.ExternalIDs["iface-id"]; exists {
			lspNames = append(lspNames, ifaceID)
		}
	}

	// Now find LSPs with these names
	var result []*nbdb.LogicalSwitchPort
	for _, name := range lspNames {
		lsp := &nbdb.LogicalSwitchPort{Name: name}
		var lspList []nbdb.LogicalSwitchPort
		err := nbClient.Where(lsp).List(context.Background(), &lspList)
		if err != nil {
			continue // Skip if error
		}
		if len(lspList) > 0 {
			result = append(result, &lspList[0])
		}
	}

	return result, nil
}

func main() {
	ctx := context.Background()
	nbEndpoint := os.Getenv("OVN_NB_ENDPOINT")
	if nbEndpoint == "" {
		nbEndpoint = "tcp:127.0.0.1:6641"
	}
	ovsEndpoint := os.Getenv("OVS_DB_ENDPOINT")
	if ovsEndpoint == "" {
		ovsEndpoint = "unix:/var/run/openvswitch/db.sock"
	}
	log.Printf("OVSDB endpoints: OVN_NB_ENDPOINT=%s OVS_DB_ENDPOINT=%s", nbEndpoint, ovsEndpoint)

	// =========================
	// 1️⃣ NBDB Model (OVN Northbound)
	// =========================
	nbDBModel, err := model.NewClientDBModel("OVN_Northbound",
		map[string]model.Model{
			"Logical_Switch":      &nbdb.LogicalSwitch{},
			"Logical_Switch_Port": &nbdb.LogicalSwitchPort{},
		})
	if err != nil {
		log.Fatalf("Failed to create db model: %v", err)
	}

	// Initialize the Client
	nbClient, err := client.NewOVSDBClient(nbDBModel, client.WithEndpoint(nbEndpoint))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	if err := nbClient.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	// Populate client cache once at startup
	if _, err := nbClient.MonitorAll(ctx); err != nil {
		log.Printf("Warning: nbClient.MonitorAll failed at startup: %v (continuing)", err)
	}
	defer nbClient.Close()

	// =========================
	// 2️⃣ OVS DB Model (Open_vSwitch)
	// =========================
	ovsDBModel, err := model.NewClientDBModel("Open_vSwitch",
		map[string]model.Model{
			"Bridge":    &vswitch.Bridge{},
			"Port":      &vswitch.Port{},
			"Interface": &vswitch.Interface{},
		})
	if err != nil {
		log.Fatal(err)
	}

	ovsClient, err := client.NewOVSDBClient(
		ovsDBModel,
		client.WithEndpoint(ovsEndpoint),
	)
	if err != nil {
		log.Printf("Warning: Failed to create OVS client: %v (continuing for testing)", err)
		// Create a nil client for testing - methods won't work but registration will show
		ovsClient = nil
	} else if err := ovsClient.Connect(ctx); err != nil {
		log.Printf("Warning: Failed to connect to OVS: %v (continuing for testing)", err)
		ovsClient = nil
	}

	if ovsClient != nil {
		defer ovsClient.Close()
	}

	// Initialize networks map (populated by AddNetwork/GetNetwork calls)
	networks := make(map[string]*nbdb.LogicalSwitch)

	// =========================
	// Start gRPC Server
	// =========================
	listener, err := net.Listen("tcp", ":50053")
	if err != nil {
		log.Fatalf("Failed to listen on :50053: %v", err)
	}

	grpcServer := grpc.NewServer()
	server := &OvsdbServer{
		nbClient:  nbClient,
		ovsClient: ovsClient,
		networks:  networks,
	}

	// Log service descriptor BEFORE registering
	log.Printf("About to register OvsdbService with %d methods", len(ovsdbpb.OvsdbService_ServiceDesc.Methods))
	for i, m := range ovsdbpb.OvsdbService_ServiceDesc.Methods {
		log.Printf("  [%d] %s", i, m.MethodName)
	}

	ovsdbpb.RegisterOvsdbServiceServer(grpcServer, server)

	log.Println("gRPC OVSDB server started on :50053")
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("Failed to serve gRPC: %v", err)
	}
}
