package ownership

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestCrossVersionJournal(t *testing.T) {
	mode, dir, scenario := os.Getenv("EVPN_CROSS_MODE"), os.Getenv("EVPN_CROSS_DIR"), os.Getenv("EVPN_CROSS_CASE")
	if mode == "" {
		t.Skip("explicit cross-version coordinator required")
	}
	if os.Geteuid() != 0 || !filepath.IsAbs(dir) {
		t.Fatal("root and an absolute disposable fixture path required")
	}
	fmt.Printf("COMPILER %s MODE %s CASE %s\n", runtime.Version(), mode, scenario)
	domain := testDomain()
	path := filepath.Join(dir, "journal")
	if mode == "read" && scenario == "foreign" {
		domain.NodeUID = domain.InstallationUID
	}
	j, err := Open(path, domain, mode == "write")
	negative := scenario == "truncated" || scenario == "checksum" || scenario == "unknown-envelope" ||
		scenario == "unknown-record" || scenario == "extra-value" || scenario == "foreign" || scenario == "overlap"
	if mode == "read" && negative {
		if j != nil {
			j.Close()
		}
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("unsafe cross-version journal accepted: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	payload := map[string]string{"uuid": "b313b954-c7de-43ad-ae2f-cc3a97962ecf", "value": "<durable>&\u2028"}
	if mode == "write" {
		if scenario == "overlap" {
			fmt.Println("READY")
			if _, err := bufio.NewReader(os.Stdin).ReadByte(); err != nil {
				t.Fatal(err)
			}
			return
		}
		if scenario == "crash-before-rename" || scenario == "crash-after-sync" {
			j.fault = func(boundary string) error {
				if scenario == "crash-before-rename" && boundary == "before-rename" ||
					scenario == "crash-after-sync" && boundary == "after-directory-sync" {
					os.Exit(42)
				}
				return nil
			}
		}
		seq, err := j.Begin("AddPath", payload)
		if err != nil {
			t.Fatal(err)
		}
		if scenario != "pending" && scenario != "crash-before-rename" && scenario != "crash-after-sync" {
			if err := j.Commit(seq, payload); err != nil {
				t.Fatal(err)
			}
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(path, journalName)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var e envelope
		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatal(err)
		}
		switch scenario {
		case "truncated":
			data = data[:len(data)/2]
		case "checksum":
			e.SHA256 = "invalid"
			data, err = json.Marshal(e)
		case "unknown-envelope":
			data = append(append([]byte{}, data[:len(data)-1]...), []byte(`,"Unknown":true}`)...)
		case "unknown-record":
			e.Record = append(append([]byte{}, e.Record[:len(e.Record)-1]...), []byte(`,"Unknown":true}`)...)
			h := sha256.Sum256(e.Record)
			e.SHA256 = hex.EncodeToString(h[:])
			data, err = json.Marshal(e)
		case "extra-value":
			data = append(data, []byte("\n{}")...)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode != "read" {
		t.Fatal("unknown helper mode")
	}
	raw, pending, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "pending" || scenario == "crash-after-sync" {
		if pending == nil || pending.Operation != "AddPath" || pending.Sequence != 1 || string(raw) != "null" {
			t.Fatalf("pending intent lost: %s %+v", raw, pending)
		}
		var got map[string]string
		if err := json.Unmarshal(pending.Payload, &got); err != nil || !reflect.DeepEqual(got, payload) {
			t.Fatalf("pending payload changed: %v %v", got, err)
		}
		if _, err := j.Begin("retry", nil); !errors.Is(err, ErrBlocked) {
			t.Fatal("ambiguous intent was retried")
		}
	} else if scenario == "crash-before-rename" {
		if pending != nil || string(raw) != "null" {
			t.Fatal("unrenamed temporary journal promoted")
		}
	} else {
		var got map[string]string
		if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, payload) || pending != nil {
			t.Fatalf("acknowledged receipt changed: %v %v", got, err)
		}
	}
}
