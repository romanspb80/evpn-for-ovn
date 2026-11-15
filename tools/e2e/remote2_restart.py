#!/usr/bin/env python3
"""Bounded R3 live restarts; not E1/E2 or full F2/F4/H1 acceptance."""

import argparse
import hashlib
import json
from pathlib import Path
import signal
import subprocess
import time

import containerlab as lab
from external_gobgp import B_BASE, R_BASE, R2_MAC, ThreeDomainBaseline
from external_recovery import RecoveryBaseline, stable_objects
from geneve_baseline import require


SCENARIOS = ("projector-only", "speaker-only", "combined")


class Remote2Restarts(RecoveryBaseline):
    def inputs(self):
        super().inputs()
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/remote2_restart.py", "tools/e2e/test_remote2_restart.py"):
            data = (lab.ROOT / relative).read_bytes()
            target = self.directory / "source" / relative
            target.write_bytes(data)
            pins[relative] = hashlib.sha256(data).hexdigest()
        # Include every compiled Go source and module input, not only fixture files.
        sources = self.command(["go", "list", "-deps", "-f",
                               '{{if not .Standard}}{{.Dir}}{{range .GoFiles}} {{.}}{{end}}{{end}}',
                               "./cmd/evpn-node-agent", "./tools/e2e/remote2", "./pkg/evpn"],
                              "compiled-source-list")
        for line in sources.splitlines():
            parts = line.split()
            if not parts:
                continue
            for name in parts[1:]:
                source = Path(parts[0]) / name
                key = str(source.relative_to(lab.ROOT)) if source.is_relative_to(lab.ROOT) else str(source)
                pins[key] = hashlib.sha256(source.read_bytes()).hexdigest()
        for name in ("go.mod", "go.sum"):
            pins[name] = hashlib.sha256((lab.ROOT / name).read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)
        lab.write(self.directory / "build-inputs.json", {
            "sources": pins,
            "binaries": {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                         for p in (self.directory / "build").iterdir() if p.is_file()},
        })

    def status(self, label):
        return self.rread("status.json", label)

    def ready(self, label, after=None):
        def healthy(s):
            return (not s["Error"] and (after is None or s["Time"] > after) and
                    s["Snapshot"]["Authoritative"] and
                    not s["Export"]["OwnershipUnverified"] and
                    len(s["Export"].get("Applied") or []) == 1 and
                    len(s.get("Receipts") or []) == 1 and
                    s["RecoveryState"] in ("RUNNING", "RECOVERY_PARTIAL") and
                    all(s["Receipts"][0][n]["ACK"] and s["Receipts"][0][n]["Status"] == "confirmed"
                        for n in ("FDB", "Neighbor")))
        first = self.bwait("status.json", healthy, label + "-ready-first", True)
        self.bwait("import-status.json", lambda s: not s.get("Error") and
                   s["Snapshot"]["Authoritative"] and len(s.get("Receipts") or []) == 1,
                   label + "-b-import")
        return self.bwait("status.json", lambda s: healthy(s) and s["Time"] > first["Time"] and
                          s["Snapshot"]["Endpoints"] == first["Snapshot"]["Endpoints"],
                          label + "-ready-second", True)

    def peer(self, label):
        return self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
                       "neighbor", "198.18.20.3", "-j", name=label)

    def peer_established(self, label):
        peer = json.loads(self.peer(label))
        require(peer["conf"]["neighbor_address"] == "198.18.20.3", "unexpected BGP neighbor")
        # GoBGP's JSON format uses the protobuf FSM enum (ESTABLISHED = 6).
        return peer["state"]["session_state"] == 6

    def witness(self, label):
        status = self.status(label + "-status")
        for field, process in (("Projector", "projector"), ("Speaker", "speaker")):
            pid, ticks, _ = self.r_processes[process]
            require(status[field]["PID"] == pid and status[field]["StartTicks"] == ticks,
                    "status does not identify the actual registered process")
        result = {"observedAt": time.time(), "status": status, "journals": {},
                  "peer": json.loads(self.peer(label + "-peer"))}
        for path in ("lifecycle/journal.json", "export/journal.json", "import/receipts-v1/journal.json"):
            journal = self.rread(path, label + "-" + path.replace("/", "-"))
            require(journal["Record"]["Pending"] is None, label + ": global pending journal intent")
            result["journals"][path] = journal
        result["metadata"] = self.r2("sh", "-ec",
            f"sha256sum {R_BASE}/remote2.json {R_BASE}/r2-speaker.json {R_BASE}/r2-gobgpd.yaml; "
            f"stat -c '%n %u %a %d %i' {R_BASE}/lifecycle/owner.lock {R_BASE}/export/owner.lock "
            f"{R_BASE}/import/receipts-v1/owner.lock {R_BASE}/import/remote-netlink-owner; "
            f"cat {R_BASE}/import/remote-netlink-owner; readlink /proc/self/ns/net",
            name=label + "-metadata")
        digest = result["metadata"].split()[0]
        for journal in result["journals"].values():
            require(journal["Record"]["Domain"]["ConfigDigest"] == digest,
                    "journal domain is not bound to complete immutable config bytes")
        result["devices"] = json.loads(self.r2("ip", "-d", "-j", "link", "show",
                                                name=label + "-devices"))
        result["fdb"] = json.loads(self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000",
                                           name=label + "-fdb"))
        result["neighbors"] = json.loads(self.r2("ip", "-j", "neigh", "show", "dev", "br2000",
                                                 name=label + "-neighbors"))
        self.b_rib(label + "-r2", True)
        self.b_rib(label + "-b")
        self.b_foreign(label + "-foreign")
        result["foreignBGP"] = json.loads(self.rt("b", "gobgp", "--target",
            f"unix://{B_BASE}/gobgp.sock", "global", "rib", "-a", "ipv4", "-j",
            name=label + "-foreign-bgp"))
        require("203.0.113.222/32" in json.dumps(result["foreignBGP"]), "foreign BGP sentinel missing")
        require("203.0.113.222" not in json.dumps(status["Export"]), "foreign BGP sentinel adopted")
        lab.write(self.directory / (label + "-witness.json"), result)
        return result

    def baseline(self):
        self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
                "global", "rib", "add", "203.0.113.222/32", "nexthop", "198.18.20.2",
                name="foreign-bgp-create")
        self.ready("baseline")
        self.command(["docker", "exec", self.remote2, "/usr/local/bin/remote2",
                      "-config", f"{R_BASE}/remote2.json"], "baseline-overlapping-writer", check=False)
        overlap = json.loads((self.directory / "baseline-overlapping-writer.command.json").read_text())
        require(overlap["exit"] != 0 and "owner still active" in
                (self.directory / "baseline-overlapping-writer.stderr").read_text(),
                "second live projector was not fenced by original lifecycle journal")
        self.start_phase("baseline")
        self.probes("baseline")
        self.stop_phase()
        self.witness("baseline")

    def launch_projector(self):
        self.blaunch("projector", "/usr/local/bin/remote2",
                     ["-config", f"{R_BASE}/remote2.json"], True)

    def launch_speaker(self):
        # Reuse the original immutable attestation document: same pins, actual new peer.
        self.blaunch("speaker", "/usr/local/bin/gobgpd",
                     ["-f", f"{R_BASE}/r2-gobgpd.yaml", "--api-hosts",
                      f"unix://{R_BASE}/gobgp.sock"], True)

    def drain_recreate(self, label):
        before = self.status(label + "-pre-drain")
        old_uuid = before["Export"]["Applied"][0]["PathUUID"]
        self.bput("endpoint.json", {"Generation": self.r_generation,
                  "Incarnation": self.r_incarnation, "Retired": True}, True)
        drained = self.bwait("status.json", lambda s: not s["Error"] and
                            s["Lifecycle"]["Phase"] == "retired" and
                            not s["Export"]["Applied"] and s["Snapshot"]["Authoritative"] and
                            len(s.get("Receipts") or []) == 1, label + "-drained", True)
        journal = self.rread("export/journal.json", label + "-drained-export")
        require(not journal["Record"]["State"]["Receipts"] and
                journal["Record"]["Pending"] is None, "withdrawal has no durable exact drain")
        self.wait(lambda: R2_MAC not in json.dumps(self.b_rib(label + "-drain-r2", True)) and
                  R2_MAC not in json.dumps(self.b_rib(label + "-drain-b")),
                  "retired route remains in authoritative live RIB")
        require(stable_objects(drained["Receipts"]) == stable_objects(before["Receipts"]),
                "local retirement changed independently owned importer objects")
        self.b_foreign(label + "-drained-foreign")
        self.recreate_r2_generation()
        restored = self.ready(label + "-recreated", drained["Time"])
        require(restored["Export"]["Applied"][0]["PathUUID"] != old_uuid and
                restored["Lifecycle"]["Generation"] == before["Lifecycle"]["Generation"] + 1,
                "recreation reused withdrawn UUID/generation")
        self.converge_pair("b", label + "-recreated")
        self.probes(label + "-recreated")
        self.witness(label + "-recreated")

    def restart(self, scenario):
        before = self.witness(scenario + "-before")
        old = before["status"]
        processes = {n: list(v) for n, v in self.r_processes.items()}
        events = {"restartRequested": time.time(), "processesBefore": processes}
        lab.write(self.directory / (scenario + "-events.json"), events)
        self.start_phase(scenario)
        # Preserve evidence that the unaffected paths forward during the disruption.
        if scenario != "speaker-only":
            self.r2("cp", f"{R_BASE}/projector.log", f"{R_BASE}/{scenario}-projector-before.log")
            self.bstop("projector", True)
        if scenario != "projector-only":
            self.r2("cp", f"{R_BASE}/speaker.log", f"{R_BASE}/{scenario}-speaker-before.log")
            self.bstop("speaker", True)
            self.wait(lambda: not self.peer_established(scenario + "-peer-down"),
                      "BGP session did not leave ESTABLISHED")
            events["bgpDown"] = time.time()
            if scenario == "speaker-only":
                self.wait(lambda: bool(self.status(scenario + "-stale-peer-rejected")["Error"]),
                          "projector did not reject stale actual Unix peer")
                # Bound the fresh-speaker RIB observation before automatic re-origination.
                self.r2("kill", "-STOP", str(processes["projector"][0]))
                events["projectorObservationPause"] = time.time()
        self.probes(scenario + "-disrupted", ("1", "g"))
        if scenario != "projector-only":
            self.launch_speaker()
            events["speakerLaunched"] = time.time()
            self.wait(lambda: "READY" in self.r2("sh", "-ec",
                      f"test ! -S {R_BASE}/gobgp.sock || echo READY"),
                      "replacement speaker Unix socket unavailable")
            fresh = self.b_rib(scenario + "-fresh-speaker", True)
            require(R2_MAC not in json.dumps(fresh),
                    "replacement speaker unexpectedly retained old local endpoint path")
            events["freshRIBObserved"] = time.time()
            events["speakerRIB"] = "fresh: old local endpoint path absent before projector resumed"
            if scenario == "speaker-only":
                self.r2("kill", "-CONT", str(processes["projector"][0]))
                events["projectorObservationResumed"] = time.time()
            self.wait(lambda: self.peer_established(scenario + "-peer-up"),
                      "BGP session not restored")
            events["bgpEstablished"] = time.time()
        if scenario != "speaker-only":
            self.launch_projector()
        recovered = self.ready(scenario, old["Time"])
        events["attestedAuthoritativeReady"] = time.time()
        after = self.witness(scenario + "-after")
        new = after["status"]
        p_old, p_new = old["Projector"], new["Projector"]
        s_old, s_new = old["Speaker"], new["Speaker"]
        projector_same = p_old == p_new
        speaker_same = (s_old["PID"], s_old["StartTicks"]) == (s_new["PID"], s_new["StartTicks"])
        require(projector_same == (scenario == "speaker-only"), "projector process identity mismatch")
        require(speaker_same == (scenario == "projector-only"), "speaker process identity mismatch")
        require(before["metadata"] == after["metadata"], "immutable pins, locks or marker changed")
        for name in before["journals"]:
            require(before["journals"][name]["Record"]["Domain"] == after["journals"][name]["Record"]["Domain"],
                    "recovery domain changed")
        for name in ("lifecycle/journal.json", "import/receipts-v1/journal.json"):
            if name.startswith("lifecycle"):
                require(before["journals"][name]["Record"]["State"] ==
                        after["journals"][name]["Record"]["State"], "endpoint ledger changed on restart")
            else:
                require(before["journals"][name]["Record"]["State"]["Controller"] ==
                        after["journals"][name]["Record"]["State"]["Controller"],
                        "independent importer controller changed")
                require(before["journals"][name]["Record"]["State"]["Devices"] ==
                        after["journals"][name]["Record"]["State"]["Devices"],
                        "kernel recovery device identity changed")
        old_owned, new_owned = old["Export"]["Applied"][0], recovered["Export"]["Applied"][0]
        require((old_owned["PathUUID"] == new_owned["PathUUID"]) == speaker_same,
                "old UUID ownership transferred or valid UUID not recovered")
        require(foreign_paths(before["foreignBGP"]) == foreign_paths(after["foreignBGP"]),
                "foreign BGP route changed")
        self.converge_pair("b", scenario + "-forwarding")
        self.probes(scenario + "-recovered")
        events["dataplaneReady"] = time.time()
        self.drain_recreate(scenario)
        self.stop_phase()
        events["completed"] = time.time()
        events["processesAfter"] = {n: list(v) for n, v in self.r_processes.items()}
        events["status"] = "PASS"
        lab.write(self.directory / (scenario + "-events.json"), events)


def run(directory, cluster):
    require(not directory.exists(), "evidence directory already exists")
    require(directory != lab.ROOT and lab.ROOT not in directory.parents, "evidence must be outside repository")
    directory.mkdir(parents=True, mode=0o700)
    fixture = Remote2Restarts(directory, cluster)
    cases = {n: {"status": "NOT_RUN"} for n in ("offline-prerequisites", "topology-prerequisites",
             "baseline", *SCENARIOS, "owned-withdrawal", "cleanup")}
    current = "offline-prerequisites"
    handlers = {}

    def interrupted(signum, _frame):
        raise lab.LabError(f"R3 interrupted by signal {signum}")

    for sig in (signal.SIGINT, signal.SIGTERM):
        handlers[sig] = signal.signal(sig, interrupted)
    with lab.exclusive():
        try:
            fixture.inputs()
            cases[current] = {"status": "PASS"}
            current = "topology-prerequisites"
            for action in (fixture.provision, fixture.ovs, fixture.chassis, fixture.endpoints,
                           fixture.bindings, fixture.connectivity, fixture.packets, fixture.no_evpn,
                           fixture.fixture, fixture.runtime, fixture.export, fixture.import_route,
                           fixture.forwarding, fixture.coexistence, fixture.b_fixture,
                           fixture.b_runtime, fixture.b_export, fixture.b_import, fixture.three_packets):
                print("R3 prerequisite=" + action.__name__, flush=True)
                action()
            cases[current] = {"status": "PASS"}
            current = "baseline"
            fixture.baseline()
            cases[current] = {"status": "PASS"}
            for current in SCENARIOS:
                print("R3 scenario=" + current, flush=True)
                fixture.restart(current)
                cases[current] = {"status": "PASS"}
            current = "owned-withdrawal"
            # Call the base fixture drain, not RecoveryBaseline's full F1 scenario.
            fixture.nb("lsp-del", fixture.b_lsp)
            ThreeDomainBaseline.b_withdraw(fixture)
            ThreeDomainBaseline.r_withdraw(fixture)
            fixture.b_foreign()
            fixture.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
                       "global", "rib", "del", "203.0.113.222/32", "nexthop", "198.18.20.2",
                       name="foreign-bgp-owner-cleanup")
            fixture.b_cleanup()
            fixture.uncertainty()
            fixture.local_withdraw()
            fixture.remote_withdraw()
            fixture.foreign_kernel()
            fixture.external_cleanup()
            fixture.remove_endpoints()
            fixture.foreign()
            cases[current] = {"status": "PASS"}
        except (lab.LabError, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
            cases[current] = {"status": "FAIL", "detail": str(error)}
            (directory / "first-failure.txt").write_text(str(error) + "\n")
            print(f"R3 FAIL {current}: {error}", flush=True)
        finally:
            errors = []
            if (directory / "lab.json").exists() and lab.load(directory)["phase"] not in ("reserved", "down"):
                for action in (lambda: fixture.stop_phase(verify=False),
                               lambda: fixture.stop_external_capture(require_packets=False)
                               if fixture.ext_captures else None,
                               lambda: fixture.capture_stop() if fixture.captures else None,
                               lambda: fixture.diagnostics() if cases[current]["status"] == "FAIL" else None,
                               lambda: lab.collect(directory)):
                    try:
                        action()
                    except (lab.LabError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
                        errors.append(str(error))
                try:
                    lab.down(directory)
                    inventory = lab.inventory(lab.load(directory))
                    require(not any(inventory.values()), "owned resources remain after scoped teardown")
                    require(not (lab.STATE_ROOT / "active.json").exists(), "coordinator lease not released")
                    require(not errors, "diagnostic/capture cleanup failed: " + "; ".join(errors))
                    cases["cleanup"] = {"status": "PASS", "inventory": inventory, "diagnosticErrors": errors}
                except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                    cases["cleanup"] = {"status": "BLOCKED", "detail": str(error), "diagnosticErrors": errors}
            else:
                cases["cleanup"] = {"status": "PASS", "detail": "no provisioned resources"}
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
            status = "PASS" if all(c["status"] == "PASS" for c in cases.values()) else "BLOCKED"
            lab.write(directory / "r3-result.json", {"status": status, "cases": cases,
                      "fullMilestoneAccepted": False, "evidenceDirectory": str(directory)})
            print(json.dumps({"status": status, "cases": cases, "evidenceDirectory": str(directory)}), flush=True)
    return 0 if status == "PASS" else 1


def foreign_paths(rib):
    return [{"nlri": p["nlri"], "attrs": p["attrs"], "peer": p.get("peer-address")}
            for paths in rib.values() for p in paths]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    raise SystemExit(run(args.directory.resolve(), args.cluster))
