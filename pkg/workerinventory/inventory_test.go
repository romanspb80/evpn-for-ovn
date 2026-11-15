package workerinventory

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleInventory() Inventory {
	network := Network{ID: "tenant-a", LogicalSwitch: "tenant-a", VNI: 100}
	worker := Worker{
		NodeName: "worker-a", NodeUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Chassis: "chassis-a", SpeakerRouterID: "192.0.2.11", SpeakerASN: 64512,
		SpeakerEndpoint: "unix:///var/run/gobgp/api.sock", LocalVTEP: "192.0.2.21",
		AttestationPath: "/etc/evpn/provider-attestation/provider.json", AuthorizationPath: "/run/evpn-node-agent/cni.json",
		ExportJournal: "/var/lib/evpn-node-agent/export", ImportJournal: "/var/lib/evpn-node-agent/import",
		ContinuityJournal: "/var/lib/evpn-node-agent/cni", Networks: []Network{network},
	}
	other := worker
	other.NodeName, other.NodeUID, other.Chassis = "worker-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "chassis-b"
	other.SpeakerRouterID, other.LocalVTEP = "192.0.2.12", "192.0.2.22"
	return Inventory{Version: 1, Revision: 1, ClusterUID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		InstallationUID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Workers: []Worker{worker, other}}
}

func TestCanonicalAndSelection(t *testing.T) {
	in := sampleInventory()
	data, digest, err := in.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	reversed := sampleInventory()
	reversed.Workers[0], reversed.Workers[1] = reversed.Workers[1], reversed.Workers[0]
	data2, digest2, err := reversed.Canonical()
	if err != nil || string(data) != string(data2) || digest != digest2 {
		t.Fatalf("canonical serialization changed with worker order: %v", err)
	}
	if _, err := in.Select("worker-a", in.Workers[1].NodeUID); err == nil {
		t.Fatal("wrong node UID accepted")
	}
	for _, change := range []func(*Inventory){
		func(i *Inventory) { i.Workers[1].NodeName = i.Workers[0].NodeName },
		func(i *Inventory) { i.Workers[1].NodeUID = i.Workers[0].NodeUID },
		func(i *Inventory) { i.Workers[1].Chassis = i.Workers[0].Chassis },
		func(i *Inventory) { i.Workers[1].SpeakerRouterID = i.Workers[0].SpeakerRouterID },
		func(i *Inventory) { i.Workers[1].LocalVTEP = i.Workers[0].LocalVTEP },
		func(i *Inventory) {
			i.Workers[1].Networks = []Network{{ID: "tenant-b", LogicalSwitch: "tenant-b", VNI: 100}}
		},
		func(i *Inventory) { i.Workers[1].ExportJournal = i.Workers[1].ImportJournal },
	} {
		bad := sampleInventory()
		change(&bad)
		if _, _, err := bad.Canonical(); err == nil {
			t.Fatal("conflicting inventory accepted")
		}
	}
}

func TestFenceRestartRevisionAndAssignment(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership journal requires root")
	}
	in := sampleInventory()
	directory := filepath.Join(t.TempDir(), "fence")
	open := func() *Fence {
		t.Helper()
		f, err := OpenFence(directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	f := open()
	if _, err := f.Accept(in); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	defer f.Close()
	if _, err := f.Accept(in); err != nil {
		t.Fatal(err)
	}
	changed := sampleInventory()
	changed.Revision = 2
	changed.Workers[0].Chassis = "replacement"
	if _, err := f.Accept(changed); err == nil {
		t.Fatal("worker reassignment accepted")
	}
	next := sampleInventory()
	next.Revision = 2
	next.Workers = next.Workers[:1]
	if _, err := f.Accept(next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Accept(in); err == nil {
		t.Fatal("stale revision accepted")
	}
	next.Workers = append(next.Workers, in.Workers[1])
	if _, err := f.Accept(next); err == nil {
		t.Fatal("same revision with different digest accepted")
	}
	next.Revision = 3
	if _, err := f.Accept(next); err != nil {
		t.Fatalf("adding another worker must preserve the unchanged local assignment: %v", err)
	}
	next.Revision = 4
	next.Workers[0].NodeUID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if _, err := f.Accept(next); err == nil {
		t.Fatal("wrong node accepted")
	}
}

func TestFencePendingAcceptanceFailsClosed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership journal requires root")
	}
	in := sampleInventory()
	directory := filepath.Join(t.TempDir(), "fence")
	f, err := OpenFence(directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Accept(in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.journal.Begin(fenceOperation, f.state); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = OpenFence(directory, in.ClusterUID, in.InstallationUID, in.Workers[0].NodeName, in.Workers[0].NodeUID)
	if err == nil {
		f.Close()
		t.Fatal("pending acceptance should block restart")
	}
}
