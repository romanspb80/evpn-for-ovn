package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

func TestCrossVersionCNI(t *testing.T) {
	mode, dir, scenario := os.Getenv("EVPN_CROSS_MODE"), os.Getenv("EVPN_CROSS_DIR"), os.Getenv("EVPN_CROSS_CASE")
	if mode == "" {
		t.Skip("explicit cross-version coordinator required")
	}
	if os.Geteuid() != 0 || !filepath.IsAbs(dir) {
		t.Fatal("root and an absolute disposable fixture path required")
	}
	fmt.Printf("COMPILER %s MODE %s CASE %s\n", runtime.Version(), mode, scenario)
	d := ownership.Domain{InstallationUID: "7b2caa55-3f84-43ad-99e9-dcaad600a6ac",
		NodeUID: "46b08606-9b26-4098-90d7-02c14098122b", BootID: "8a63e552-b620-445f-9ab6-8d924321ccfe",
		NetworkNamespace: "test-netns", ConfigDigest: strings.Repeat("a", 64)}
	if mode == "read" && scenario == "foreign-domain" {
		d.NodeUID = d.InstallationUID
	}
	j, err := ownership.Open(filepath.Join(dir, "cni"), d, mode == "write")
	if mode == "read" && scenario == "foreign-domain" {
		if j != nil {
			j.Close()
		}
		if !errors.Is(err, ownership.ErrBlocked) {
			t.Fatalf("foreign domain accepted: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s, c, _, w, o, manager, req := continuityFixture(t, false)
	c.journal = j
	if mode == "write" {
		if scenario == "rejected" {
			c.authority = &admissionTestAuthority{err: errors.New("unrelated caller")}
			c.workload = admissionTestWorkload{w}
			if err := s.recoverCNI(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Add(context.Background(), req); err == nil || manager.ensureCount != 0 {
				t.Fatal("rejected creation mutated resources")
			}
			return
		}
		enableTestAdmission(t, s, c, w, o)
		w.err = errCNIRuntimePending
		if _, err := s.Add(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		key := endpointKey(req.Identity)
		ack := c.state.Endpoints[key]
		ack.CreatedAt = time.Now()
		ack.VerifyBy = ack.CreatedAt.Add(2 * time.Minute)
		c.state.Endpoints[key] = ack
		if scenario == "active" || scenario == "uuid-replaced" {
			w.err = nil
			if err := s.verifyPendingCNI(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if scenario == "incomplete" {
			ack = c.state.Endpoints[key]
			ack.Admission = nil
			c.state.Endpoints[key] = ack
		}
		if scenario == "identity-conflict" {
			ack = c.state.Endpoints[key]
			ack.Spec.Workload.UID = "foreign"
			c.state.Endpoints[key] = ack
		}
		seq, err := j.Begin("test-checkpoint", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Commit(seq, c.state); err != nil {
			t.Fatal(err)
		}
		if scenario == "unacknowledged" {
			if _, err := j.Begin("cni-add", req); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	if mode != "read" {
		t.Fatal("unknown helper mode")
	}
	o.present = scenario != "rejected"
	o.receipt.NetNSDevice, o.receipt.NetNSInode = 10, 20
	if scenario == "uuid-replaced" {
		o.receipt.LSPUUID = "foreign-uuid"
	}
	c.workload = admissionTestWorkload{w}
	err = s.recoverCNI(context.Background(), c)
	negative := scenario == "incomplete" || scenario == "identity-conflict" ||
		scenario == "uuid-replaced" || scenario == "unacknowledged"
	if negative {
		if !errors.Is(err, ownership.ErrBlocked) || len(s.store.records) != 0 || s.continuity != nil {
			t.Fatalf("unsafe restart adopted state: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	} else if scenario == "rejected" {
		if len(s.store.records) != 0 {
			t.Fatal("rejected creation adopted after restart")
		}
	} else {
		raw, _, err := j.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		var state cniACKState
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		key := endpointKey(req.Identity)
		want := "pending_creation"
		if scenario == "active" {
			want = "active"
		}
		if state.Endpoints[key].State != want || len(s.store.records) != 1 {
			t.Fatalf("receipt not restored as %s", want)
		}
		if want == "pending_creation" {
			w.err = errCNIRuntimePending
			if err := s.verifyPendingCNI(context.Background()); !errors.Is(err, errCNIRuntimePending) {
				t.Fatalf("pending creation implicitly activated: %v", err)
			}
		}
	}
	if manager.ensureCount != 0 || o.deletes != 0 {
		t.Fatal("restart mutated modeled foreign/dataplane resources")
	}
}
