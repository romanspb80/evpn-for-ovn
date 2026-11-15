// Package workerinventory defines the declared worker identities for a
// multi-chassis deployment. It does not authenticate the publisher or attest
// the declared identities; callers must do both independently.
package workerinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

type Network struct {
	ID            string `json:"id"`
	LogicalSwitch string `json:"logicalSwitch"`
	VNI           uint32 `json:"vni"`
}

type Worker struct {
	NodeName          string    `json:"nodeName"`
	NodeUID           string    `json:"nodeUID"`
	Chassis           string    `json:"chassis"`
	SpeakerRouterID   string    `json:"speakerRouterID"`
	SpeakerASN        uint32    `json:"speakerASN"`
	SpeakerEndpoint   string    `json:"speakerEndpoint"`
	LocalVTEP         string    `json:"localVTEP"`
	AttestationPath   string    `json:"attestationPath"`
	AuthorizationPath string    `json:"authorizationPath"`
	ExportJournal     string    `json:"exportJournal"`
	ImportJournal     string    `json:"importJournal"`
	ContinuityJournal string    `json:"continuityJournal"`
	Networks          []Network `json:"networks"`
}

type Inventory struct {
	Version         int      `json:"version"`
	Revision        uint64   `json:"revision"`
	ClusterUID      string   `json:"clusterUID"`
	InstallationUID string   `json:"installationUID"`
	Workers         []Worker `json:"workers"`
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func validName(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n\t")
}

func cleanPath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && s != "/" && !strings.ContainsRune(s, '\x00')
}

func unixEndpoint(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "unix" && u.Host == "" && u.Opaque == "" &&
		u.RawQuery == "" && u.Fragment == "" && s == "unix://"+u.Path && cleanPath(u.Path)
}

func unicastIPv4(s string) bool {
	ip, err := netip.ParseAddr(s)
	return err == nil && ip.Is4() && ip.String() == s && ip.IsGlobalUnicast() && !ip.IsLoopback()
}

// Canonical validates the full declaration and returns a deterministic
// serialization and digest. Its digest is integrity evidence, not authority.
func (i Inventory) Canonical() ([]byte, string, error) {
	if i.Version != 1 || i.Revision == 0 || !canonicalUUID(i.ClusterUID) || !canonicalUUID(i.InstallationUID) || len(i.Workers) == 0 {
		return nil, "", fmt.Errorf("inventory requires version 1, revision, cluster/installation UUIDs and workers")
	}
	copyInventory := i
	copyInventory.Workers = append([]Worker(nil), i.Workers...)
	sort.Slice(copyInventory.Workers, func(a, b int) bool { return copyInventory.Workers[a].NodeName < copyInventory.Workers[b].NodeName })
	names, nodes, chassis, routers, vteps := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	networkByID, networkBySwitch, networkByVNI := map[string]Network{}, map[string]Network{}, map[uint32]Network{}
	for n := range copyInventory.Workers {
		w := &copyInventory.Workers[n]
		if !validName(w.NodeName) || !canonicalUUID(w.NodeUID) || !validName(w.Chassis) ||
			!unicastIPv4(w.SpeakerRouterID) || w.SpeakerASN == 0 || !unicastIPv4(w.LocalVTEP) ||
			!unixEndpoint(w.SpeakerEndpoint) ||
			!cleanPath(w.AttestationPath) || !cleanPath(w.AuthorizationPath) || !cleanPath(w.ExportJournal) ||
			!cleanPath(w.ImportJournal) || !cleanPath(w.ContinuityJournal) || len(w.Networks) == 0 {
			return nil, "", fmt.Errorf("worker %q requires complete canonical identity, trust, journal and network configuration", w.NodeName)
		}
		if w.ExportJournal == w.ImportJournal || w.ExportJournal == w.ContinuityJournal || w.ImportJournal == w.ContinuityJournal {
			return nil, "", fmt.Errorf("worker %q requires separate journal paths", w.NodeName)
		}
		if names[w.NodeName] || nodes[w.NodeUID] || chassis[w.Chassis] || routers[w.SpeakerRouterID] || vteps[w.LocalVTEP] {
			return nil, "", fmt.Errorf("worker %q duplicates a node, chassis, speaker router ID or VTEP", w.NodeName)
		}
		names[w.NodeName], nodes[w.NodeUID], chassis[w.Chassis], routers[w.SpeakerRouterID], vteps[w.LocalVTEP] = true, true, true, true, true
		w.Networks = append([]Network(nil), w.Networks...)
		sort.Slice(w.Networks, func(a, b int) bool { return w.Networks[a].ID < w.Networks[b].ID })
		local := map[string]bool{}
		for _, net := range w.Networks {
			if !validName(net.ID) || !validName(net.LogicalSwitch) || net.VNI == 0 || net.VNI > 16777215 || local[net.ID] {
				return nil, "", fmt.Errorf("worker %q has an invalid or duplicate network", w.NodeName)
			}
			for _, prior := range []Network{networkByID[net.ID], networkBySwitch[net.LogicalSwitch], networkByVNI[net.VNI]} {
				if prior.ID != "" && prior != net {
					return nil, "", fmt.Errorf("worker %q has conflicting network, switch or VNI mapping", w.NodeName)
				}
			}
			local[net.ID], networkByID[net.ID], networkBySwitch[net.LogicalSwitch], networkByVNI[net.VNI] = true, net, net, net
		}
	}
	data, err := json.Marshal(copyInventory)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// Select matches an independently obtained Kubernetes node name and UID.
func (i Inventory) Select(name, uid string) (Worker, error) {
	if _, _, err := i.Canonical(); err != nil {
		return Worker{}, err
	}
	for _, worker := range i.Workers {
		if worker.NodeName == name && worker.NodeUID == uid {
			return worker, nil
		}
	}
	return Worker{}, fmt.Errorf("inventory has no entry for the verified node name and UID")
}
