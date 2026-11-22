// trustcut is an isolated-lab fault injector, never part of a runtime image.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func run() error {
	phase := flag.String("phase", "", "prepared, ready, or final")
	request := flag.String("request", "", "exact administrative request UUID")
	previousReady := flag.String("previous-ready", "", "previous export grant hash")
	output := flag.String("output", "", "new root-only diagnostic file")
	release := flag.String("release", "", "release signal pathname")
	confirm := flag.Bool("confirm-isolated-worker", false, "authorize killing the exact journal writer")
	flag.Parse()
	if !*confirm || *request == "" || *output == "" || *release == "" ||
		(*phase != "prepared" && *phase != "ready" && *phase != "final") || os.Geteuid() != 0 {
		return fmt.Errorf("explicit isolated-worker scope, phase, request and diagnostic paths required")
	}
	root := "/var/lib/evpn-worker/"
	watched, observed := root+"export", root+"inventory/journal.json"
	if *phase == "ready" {
		watched, observed = root+"inventory", root+"export/journal.json"
	}
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_CONTENT|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD, unix.FAN_OPEN_PERM|unix.FAN_EVENT_ON_CHILD, unix.AT_FDCWD, watched); err != nil {
		return err
	}
	evidence := map[string]interface{}{"status": "READY", "phase": *phase, "request": *request}
	write := func() error {
		data, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		return os.WriteFile(*output, data, 0600)
	}
	if _, err := os.Lstat(*output); !os.IsNotExist(err) {
		return fmt.Errorf("refusing existing diagnostic file")
	}
	if err := write(); err != nil {
		return err
	}
	held := []int32{}
	defer func() {
		for _, eventFD := range held {
			unix.Close(int(eventFD))
		}
	}()
	answer := func(eventFD int32, decision uint32) error {
		var response [8]byte
		binary.LittleEndian.PutUint32(response[:4], uint32(eventFD))
		binary.LittleEndian.PutUint32(response[4:], decision)
		_, err := unix.Write(fd, response[:])
		return err
	}
	cut := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if cut {
			if _, err := os.Stat(*release); err == nil {
				for _, eventFD := range held {
					if err := answer(eventFD, unix.FAN_ALLOW); err != nil {
						return err
					}
				}
				evidence["status"] = "RELEASED"
				return write()
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		var buffer [65536]byte
		n, err := unix.Read(fd, buffer[:])
		if err == unix.EAGAIN || err == unix.EINTR {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		for offset := 0; offset < n; {
			if n-offset < 24 {
				return fmt.Errorf("truncated fanotify metadata")
			}
			event := buffer[offset:n]
			length := int(binary.LittleEndian.Uint32(event[:4]))
			if length < 24 || length > len(event) || event[4] != unix.FANOTIFY_METADATA_VERSION {
				return fmt.Errorf("invalid fanotify metadata")
			}
			offset += length
			eventFD := int32(binary.LittleEndian.Uint32(event[16:20]))
			pid := int32(binary.LittleEndian.Uint32(event[20:24]))
			if eventFD < 0 {
				return fmt.Errorf("fanotify queue overflow or missing descriptor")
			}
			executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			agent := err == nil && filepath.Base(executable) == "evpn-node-agent"
			if agent && cut {
				held = append(held, eventFD)
				continue
			}
			decision := uint32(unix.FAN_ALLOW)
			if agent {
				data, err := os.ReadFile(observed)
				if err != nil {
					return err
				}
				var journal struct {
					Record struct {
						State struct {
							Trust *struct {
								Phase     string
								RequestID string
							} `json:"trust"`
							InventoryReady string
						}
					}
				}
				if err := json.Unmarshal(data, &journal); err != nil {
					return err
				}
				matches := false
				if *phase == "ready" {
					matches = journal.Record.State.InventoryReady != "" && journal.Record.State.InventoryReady != *previousReady
				} else if trust := journal.Record.State.Trust; trust != nil {
					expected := "RECOVERY_PENDING"
					if *phase == "final" {
						expected = "RECOVERY_AUTHORIZED"
					}
					matches = trust.Phase == expected && trust.RequestID == *request
				}
				if matches {
					command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
					if err != nil || !strings.Contains(string(command), "--worker-inventory-recovery-file") {
						return fmt.Errorf("journal writer is not the scoped inventory agent")
					}
					if err := unix.Kill(int(pid), unix.SIGKILL); err != nil {
						return err
					}
					cut, decision = true, unix.FAN_DENY
					evidence["status"], evidence["pid"], evidence["executable"] = "CUT", pid, executable
					evidence["observedJournal"], evidence["observedAt"] = json.RawMessage(data), time.Now().UTC()
					if err := write(); err != nil {
						return err
					}
				}
			}
			if err := answer(eventFD, decision); err != nil {
				return err
			}
			unix.Close(int(eventFD))
		}
	}
	return fmt.Errorf("recovery boundary or release was not observed within 120s")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
