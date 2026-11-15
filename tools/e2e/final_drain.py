#!/usr/bin/env python3
"""Historical H1: independently checked drain before owner destruction."""

import argparse
import copy
import hashlib
import json
from pathlib import Path
import time

import containerlab as lab
import endpoint_lifecycle
from endpoint_lifecycle import EndpointLifecycle, device_identity, same_foreign
from external_frr import BASE, LOCAL_MAC, REMOTE_MAC, REMOTE_IP
from external_gobgp import B_BASE, R_BASE, B_MAC, R2_MAC
from geneve_baseline import require


OWNERS = ("a", "b", "r2")
MACS = (LOCAL_MAC, REMOTE_MAC, B_MAC, R2_MAC)


def journal(envelope):
    record = envelope["Record"]
    encoded = json.dumps(record, separators=(",", ":"), ensure_ascii=False).encode()
    require(hashlib.sha256(encoded).hexdigest() == envelope["SHA256"], "journal checksum mismatch")
    require(record["Pending"] is None, "unresolved owned mutation intent")
    return record


def check_barrier(before, after):
    require(after["observedAt"] > before["observedAt"] and after["leaseHeld"],
            "drain observed without live coordinator lease")
    expected_processes = before["processes"]
    if "retiredRecovery" in after:
        recovery = after["retiredRecovery"]
        require("retiredRecovery" not in recovery["drained"], "recursive recovery evidence")
        check_barrier(before, recovery["drained"])
        require(recovery["barrier"]["status"] == "PASS" and
                recovery["barrier"]["acceptedAt"] >= recovery["drained"]["observedAt"] and
                [e["side"] for e in recovery["events"]] == ["a", "b"],
                "retired recovery began before independently verified drain")
        expected_processes = copy.deepcopy(before["processes"])
        previous = recovery["barrier"]["acceptedAt"]
        for event in recovery["events"]:
            side = event["side"]
            require(event["beforeAgent"] == expected_processes[side]["agent"] and
                    event["beforeAgent"] != event["afterAgent"] and
                    event["beforeAgent"].split(maxsplit=2)[2] ==
                    event["afterAgent"].split(maxsplit=2)[2] and
                    previous < event["stoppedAt"] < event["recoveredAt"] < after["observedAt"],
                    "uncontrolled retired-state owner replacement")
            expected_processes[side]["agent"] = event["afterAgent"]
            previous = event["recoveredAt"]
    require(after["processes"] == expected_processes and after["processes"],
            "owner or speaker destroyed before drain barrier")
    same_foreign(before["foreign"], after["foreign"])
    require(not after["ovs"]["tunnels"] and not after["ovs"]["endpointPorts"] and
            not after["ovs"]["endpointFlows"] and not after["bindings"] and
            not after["southboundOwned"],
            "owned OVS/OVN endpoint state remains")
    require(not any(e.get("mac", e.get("lladdr")) in MACS for e in
                    after["remote1Kernel"]["fdb"] + after["remote1Kernel"]["neighbors"]),
            "FRR-owned endpoint kernel state remains")
    require(before["remote1Kernel"]["devices"] == after["remote1Kernel"]["devices"],
            "FRR kernel device identity changed")
    for owner in OWNERS:
        old, new = before["owners"][owner], after["owners"][owner]
        for kind in ("export", "import"):
            initial, final = journal(old[kind]), journal(new[kind])
            require(initial["Domain"] == final["Domain"], "journal recovery domain replaced")
            require(final["Sequence"] > initial["Sequence"], "withdrawal did not advance journal")
            require(not final["State"]["Receipts"], "owned receipt not durably withdrawn")
            if kind == "import":
                state = final["State"]
                require(not state["Quarantine"] and
                        state["Quarantines"] == initial["State"]["Quarantines"],
                        "global recovery blocker or changed foreign slot quarantine")
                require(state["Snapshot"]["Authoritative"] and
                        not state["Snapshot"]["Endpoints"], "durable desired state not drained")
                for field in ("Controller", "Target", "Devices"):
                    require(state[field] == initial["State"][field], "importer ownership replaced")
            else:
                require(len(initial["State"]["Receipts"]) == 1 and
                        initial["State"]["Receipts"][0]["Owned"]["PathUUID"],
                        "missing exact acknowledged UUID before retirement")
        export = new["exportStatus"]
        require(not export["OwnershipUnverified"] and not export["Applied"] and not export["Desired"] and
                export["RecoveryState"] == "RUNNING", "unacknowledged export withdrawal or blocker")
        status = new["status"]
        require(not status.get("Error") and status["Snapshot"]["Authoritative"] and
                not status["Snapshot"]["Endpoints"] and not status["Snapshot"].get("Issues") and
                not status["Receipts"] and
                status["RecoveryState"] in ("RUNNING", "RECOVERY_PARTIAL"),
                "incomplete authoritative drain or global recovery blocker")
        require(status["Time"] > old["status"]["Time"] and
                new["speaker"] == old["speaker"], "stale observation or failed speaker attestation")
        require(new["freshTimes"][0] < new["freshTimes"][1] and
                new["freshTimes"][0] > old["status"]["Time"],
                "missing independent fresh complete observations")
        for receipt in journal(old["import"])["State"]["Receipts"].values():
            endpoint = receipt["Endpoint"]
            require(receipt["Status"] == "applied" and all(
                receipt[k]["ACK"] and receipt[k]["Status"] == "confirmed" for k in ("FDB", "Neighbor")),
                "before-drain ownership lacks acknowledged exact kernel receipts")
            require(not any(e.get("mac") == endpoint["MAC"] for e in new["kernel"]["fdb"]) and
                    not any(e.get("dst") == endpoint["IP"] for e in new["kernel"]["neighbors"]),
                    "receipt-owned kernel tuple remains, including matching foreign tuple")
        require(not any(mac in json.dumps(new["rib"]) for mac in MACS),
                "owned or resurrected Type-2 route remains")
    require(not any(mac in after["frrRIB"] for mac in MACS), "FRR test-owned route remains")
    ledger = journal(after["ledger"])["State"]
    require(ledger["Phase"] == "retired" and ledger["Generation"] == before["generation"] and
            ledger["Incarnation"] == before["incarnation"], "stale generation or retirement not acknowledged")
    for side in ("a", "b"):
        policy = after["policies"][side]
        require(not policy["endpoints"] and len(policy["tombstones"]) == 1 and
                policy["tombstones"][0]["reason"] == "deleted" and
                policy["tombstones"][0]["record"] == before["endpoints"][side],
                "missing exact authoritative tombstone")
        fence = journal(after["fences"][side])
        state = fence["State"]
        endpoint = before["endpoints"][side]
        retired = [e for e in state["endpoints"] if
                   (e["networkID"], e["endpointID"]) == (endpoint["networkID"], endpoint["endpointID"])]
        require(state["revision"] == policy["revision"] and len(retired) == 1 and
                not any(e["active"] for e in state["endpoints"]) and
                retired[0]["retiredGeneration"] == endpoint["attachmentGeneration"] and
                not after["owners"][side]["error"],
                "authorization fence or attestation failed")
    return {"status": "PASS", "acceptedAt": time.time(),
            "retained": "foreign slot quarantines, empty infrastructure, native Geneve, journals and locks"}


class FinalDrain(EndpointLifecycle):
    def __init__(self, directory, cluster):
        super().__init__(directory, cluster)
        self.draining = False
        self.barrier = None

    def setup_witnesses(self):
        for action in (self.focused, self.identities_case, self.foreign_binding,
                       self.no_transit, self.disable_restore):
            print("R6 integrated=" + action.__name__, flush=True)
            action()
        super().setup_witnesses()

    def inputs(self):
        super().inputs()
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for path in sorted((lab.ROOT / "tools/e2e").glob("*.py")):
            relative = str(path.relative_to(lab.ROOT))
            pins[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)
        build = json.loads((self.directory / "build-inputs.json").read_text())
        build["sources"] = pins
        lab.write(self.directory / "build-inputs.json", build)

    def process_inventory(self, label):
        result = {}
        for side, processes, run in (
            ("a", self.processes, lambda *a, **k: self.rt("a", *a, **k)),
            ("b", self.b_processes, lambda *a, **k: self.rt("b", *a, **k)),
            ("r2", self.r_processes, self.r2),
        ):
            result[side] = {}
            for name, process in processes.items():
                pid = process[0]
                result[side][name] = run("sh", "-ec",
                    f"test \"$(awk '{{print $3}}' /proc/{pid}/stat)\" != Z; "
                    f"printf '{pid} '; awk '{{print $22}}' /proc/{pid}/stat; "
                    f"tr '\\0' ' ' < /proc/{pid}/cmdline", name=label + "-" + side + "-" + name)
        require(set(result["a"]) == set(result["b"]) == {"agent", "speaker"} and
                set(result["r2"]) == {"projector", "speaker"}, "missing live drain owner")
        # FRR is a separate fixture speaker, never stopped to manufacture route absence.
        result["frr"] = self.fr("sh", "-ec", "p=$(pidof bgpd); test -n \"$p\"; "
            "test \"$(awk '{print $3}' /proc/$p/stat)\" != Z; "
            "printf '%s ' \"$p\"; awk '{print $22}' /proc/$p/stat", name=label + "-frr-process")
        return result

    def inventory(self, label):
        owners = {}
        for side in OWNERS:
            read = self.local_reader(side) if side != "r2" else self.rread
            def settled(path, name):
                result = None
                def complete():
                    nonlocal result
                    result = read(path)
                    return result["Record"]["Pending"] is None
                self.wait(complete, "journal mutation intent did not settle before drain observation")
                lab.write(self.directory / (name + ".json"), result)
                return result
            if side == "a":
                paths = ("export-receipts/journal.json", "import-receipts/receipts-v1/journal.json")
            else:
                paths = ("export/journal.json", "import/receipts-v1/journal.json")
            first = read("import-status.json" if side != "r2" else "status.json",
                         label + "-" + side + "-first")
            def stable():
                nonlocal status
                status = read("import-status.json" if side != "r2" else "status.json")
                return (not status.get("Error") and status["Snapshot"]["Authoritative"] and
                        status["Time"] > first["Time"] and status["Snapshot"] == first["Snapshot"])
            status = first
            self.wait(stable, "drain requires fresh stable authoritative RIB observations")
            lab.write(self.directory / (label + "-" + side + "-status.json"), status)
            exp = read("export-status.json", label + "-" + side + "-export-status") if side != "r2" else status
            kernel = self.kernel(side, label + "-" + side) if side != "r2" else {
                "fdb": json.loads(self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000",
                                        name=label + "-r2-fdb")),
                "neighbors": json.loads(self.r2("ip", "-j", "neigh", "show", "dev", "br2000",
                                              name=label + "-r2-neighbors"))}
            owners[side] = {
                "export": settled(paths[0], label + "-" + side + "-export-journal"),
                "import": settled(paths[1], label + "-" + side + "-import-journal"),
                "status": status, "freshTimes": [first["Time"], status["Time"]],
                "exportStatus": exp["Result"] if side != "r2" else exp["Export"],
                "speaker": exp["SpeakerProcess"] if side != "r2" else status["Speaker"],
                "error": exp["Error"], "kernel": kernel,
                "rib": self.local_rib(side, label + "-" + side + "-rib") if side != "r2"
                else self.b_rib(label + "-r2-rib", True)}
        ovs = {"tunnels": [], "endpointPorts": [], "endpointFlows": []}
        bindings = []
        for side in ("a", "b"):
            endpoint = self.endpoint if side == "a" else self.b_endpoint
            port = endpoint["logicalSwitchPort"]
            for column, condition, key in (
                ("_uuid,name,options", "type=vxlan", "tunnels"),
                ("_uuid,name,external_ids", "name=" + port + "-host", "endpointPorts"),
            ):
                value = json.loads(self.rt(side, "ovs-vsctl", "--format=json", "--columns=" + column,
                    "find", "Interface", condition, name=label + "-" + side + "-" + key))
                ovs[key].extend(value["data"])
            flows = self.rt(side, "ovs-ofctl", "dump-flows", "br-int", name=label + "-" + side + "-flows")
            ovs["endpointFlows"].extend(line for line in flows.splitlines() if any(mac in line for mac in MACS))
            for db, query, table, condition in (
                ("nb", self.nb, "Logical_Switch_Port", "name=" + port),
                ("sb", self.sb, "Port_Binding", "logical_port=" + port),
            ):
                value = json.loads(query("--format=json", "find", table, condition,
                                         name=label + "-" + side + "-" + db))
                bindings.extend(value["data"])
        state = lab.load(self.directory)
        lease = json.loads((lab.STATE_ROOT / "active.json").read_text())
        require(lease["directory"] == str(self.directory), "coordinator lease belongs to another lab")
        southbound = []
        for table in ("FDB", "MAC_Binding"):
            rows = json.loads(self.sb("--format=json", "list", table, name=label + "-sb-" + table))
            southbound.extend(row for row in rows["data"] if any(mac in json.dumps(row) for mac in MACS))
        remote1_kernel = {
            "devices": [device_identity(json.loads(self.fr("ip", "-j", "-d", "link", "show",
                         "dev", dev, name=label + "-r1-" + dev))[0])
                        for dev in ("br1000", "vxlan1000")],
            "fdb": json.loads(self.fr("bridge", "-j", "fdb", "show", "dev", "vxlan1000",
                                    name=label + "-r1-fdb")),
            "neighbors": json.loads(self.fr("ip", "-j", "neigh", "show", "dev", "br1000",
                                          name=label + "-r1-neighbors"))}
        return {"observedAt": time.time(), "processes": self.process_inventory(label),
                "leaseHeld": state["phase"] != "down", "owners": owners, "ovs": ovs, "bindings": bindings,
                "foreign": self.witnesses(label + "-foreign"), "frrRIB": self.vty(
                    "show bgp l2vpn evpn route json", name=label + "-frr-rib"),
                "remote1Kernel": remote1_kernel, "southboundOwned": southbound,
                "ledger": self.rread("lifecycle/journal.json", label + "-ledger"),
                "policies": {s: self.local_reader(s)("policy.json", label + "-" + s + "-policy")
                             for s in ("a", "b")},
                "fences": {s: self.local_reader(s)(
                    ("authorization-fence" if s == "a" else "fence") + "/journal.json",
                    label + "-" + s + "-fence") for s in ("a", "b")}}

    def teardown_allowed(self):
        return self.barrier is not None

    def retire_remote1_neighbor(self, before):
        current = json.loads(self.fr("ip", "-j", "neigh", "show", "dev", "br1000",
                                     name="H1-r1-retired-neighbors"))
        entries = [e for e in current if e["dst"] == REMOTE_IP]
        if not entries:
            return
        initial = [e for e in before["remote1Kernel"]["neighbors"] if e["dst"] == REMOTE_IP]
        require(len(entries) == len(initial) == 1 and
                entries[0].get("lladdr") == initial[0].get("lladdr") == REMOTE_MAC,
                "refuse changed or unproven FRR fixture neighbor cleanup")
        devices = [device_identity(json.loads(self.fr("ip", "-j", "-d", "link", "show",
                   "dev", dev))[0]) for dev in ("br1000", "vxlan1000")]
        same_foreign(before["remote1Kernel"]["devices"], devices)
        self.fr("ip", "neigh", "del", REMOTE_IP, "lladdr", REMOTE_MAC, "dev", "br1000",
                name="H1-r1-fixture-neighbor-owner-cleanup")

    def stop_process(self, name):
        require(not self.draining or self.barrier is not None, "premature A shutdown before H1")
        self.shutdown_event("a", name)
        super().stop_process(name)
        self.shutdown_event("a", name, stopped=True)

    def bstop(self, name, remote=False):
        require(not self.draining or self.barrier is not None, "premature speaker/owner shutdown before H1")
        self.shutdown_event("r2" if remote else "b", name)
        super().bstop(name, remote)
        self.shutdown_event("r2" if remote else "b", name, stopped=True)

    def shutdown_event(self, side, name, stopped=False):
        if self.draining:
            events_path = self.directory / "H1-shutdown-events.json"
            events = json.loads(events_path.read_text()) if events_path.exists() else []
            if stopped:
                require(events[-1]["side"] == side and events[-1]["process"] == name,
                        "shutdown evidence order changed")
                events[-1]["stoppedAt"] = time.time()
            else:
                events.append({"side": side, "process": name, "requestedAt": time.time(),
                               "barrierAcceptedAt": self.barrier["acceptedAt"]})
            lab.write(events_path, events)

    def final_withdrawals(self):
        # These actual recoveries leave unknown foreign slots quarantined. H1
        # must drain owned slots without clearing/adopting those quarantines.
        self.local_processes = None
        for side in ("a", "b"):
            self.restart_agent_recovery(side)
        lab.write(self.directory / "integrated-scenarios.json", self.scenarios)
        self.local_processes = self.export_continuity("H1-after-recovery")
        self.draining = True
        self.transition = "H1"
        before = self.inventory("H1-before")
        before.update(generation=self.r_generation, incarnation=self.r_incarnation,
                      endpoints={"a": self.endpoint, "b": self.b_endpoint})
        for side in OWNERS:
            require(len(journal(before["owners"][side]["export"])["State"]["Receipts"]) == 1 and
                    len(journal(before["owners"][side]["import"])["State"]["Receipts"]) == 1,
                    "drain lacks complete acknowledged baseline ownership")
        lab.write(self.directory / "H1-before.json", before)
        self.start_phase("h1-remote-retirement")
        self.remote2_retire("H1-r2")
        self.probes("H1-r2-unaffected", ("1", "g"))
        self.fr("ip", "netns", "del", "evpnwork", name="H1-r1-authoritative-retire")
        self.wait(lambda: REMOTE_MAC not in self.vty("show bgp l2vpn evpn route json"),
                  "FRR source route not withdrawn")
        self.source_absent("a", "H1-r1", REMOTE_MAC)
        self.retire_remote1_neighbor(before)
        self.probes("H1-remotes-retired", ("g",))
        self.stop_phase(("g",))
        for side, policy in (("a", self.policy), ("b", self.b_policy)):
            endpoint = before["endpoints"][side]
            port = endpoint["logicalSwitchPort"]
            self.nb("lsp-del", endpoint["logicalSwitchPortUUID"])
            self.rt(side, "ovs-vsctl", "del-port", "br-int", port + "-host")
            self.rt(side, "ip", "link", "del", port + "-host")
            self.rt(side, "ip", "netns", "del", port)
            self.wait(lambda p=port: not self.sb("--bare", "--columns=_uuid", "find",
                      "Port_Binding", "logical_port=" + p).strip(), "retired SB binding remains")
            policy(1, False, "deleted")
            read = self.local_reader(side)
            self.wait(lambda r=read: not (s := r("export-status.json"))["Error"] and
                      not s["Result"]["Applied"] and not s["Result"]["OwnershipUnverified"],
                      "local exact UUID withdrawal not acknowledged")
        self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"] and
                   not s["Receipts"] and not s["Export"]["Applied"], "H1-r2-drained", True)
        # OVS external tunnels are fixture-owned, not importer receipts. Remove
        # their exact fixture configuration while every owner is still alive.
        for side in ("a", "b"):
            self.rt(side, "ovs-vsctl", "remove", "Open_vSwitch", ".", "external_ids",
                    "ovn-evpn-local-ip", "ovn-evpn-vxlan-ports")
            self.wait(lambda s=side: not self.rt(s, "ovs-vsctl", "--bare", "--columns=name",
                      "find", "Interface", "type=vxlan").strip(), "owned OVS tunnel remains before barrier")
        self.finish_drain(before)

    def finish_drain(self, before):
        after = self.inventory("H1-after")
        initial = check_barrier(before, after)
        lab.write(self.directory / "H1-drained-before-retired-recovery.json", after)
        lab.write(self.directory / "H1-initial-barrier.json", initial)
        events = []
        for side in ("a", "b"):
            record, _ = self.process_record(side, "agent")
            event = {"side": side, "beforeAgent": after["processes"][side]["agent"]}
            read = self.local_reader(side)
            prior_time = read("import-status.json")["Time"]
            prior_export_time = read("export-status.json")["Time"]
            # A separately checked empty-state barrier authorizes this bounded
            # recovery test; final owner shutdown remains interlocked below.
            if side == "a":
                EndpointLifecycle.stop_process(self, "agent")
                event["stoppedAt"] = time.time()
                self.launch("agent", record["executable"], record["arguments"])
            else:
                EndpointLifecycle.bstop(self, "agent")
                event["stoppedAt"] = time.time()
                self.blaunch("agent", record["executable"], record["arguments"])
            self.wait(lambda r=read: not (s := r("import-status.json")).get("Error") and
                      s["Time"] > prior_time and s["Snapshot"]["Authoritative"] and not s["Snapshot"]["Endpoints"] and
                      not s["Receipts"], "retired importer recovery resurrected owned state")
            self.wait(lambda r=read: not (s := r("export-status.json")).get("Error") and
                      s["Time"] > prior_export_time and not s["Result"]["Applied"] and
                      not s["Result"]["OwnershipUnverified"],
                      "retired exporter recovery resurrected owned state")
            processes = self.process_inventory("H1-retired-" + side)
            event.update(afterAgent=processes[side]["agent"], recoveredAt=time.time())
            events.append(event)
        recovery = {"drained": after, "barrier": initial, "events": events}
        after = self.inventory("H1-after-retired-recovery")
        after["retiredRecovery"] = recovery
        lab.write(self.directory / "H1-after.json", after)
        # Read the durable evidence back through an independent pure validator.
        self.barrier = check_barrier(json.loads((self.directory / "H1-before.json").read_text()),
                                     json.loads((self.directory / "H1-after.json").read_text()))
        lab.write(self.directory / "H1-barrier.json", self.barrier)
        self.foreign_expected = None
        self.cleanup_after_withdrawals()


def run(directory, cluster):
    result = endpoint_lifecycle.run(directory, cluster, FinalDrain)
    underlying = json.loads((directory / "r5-result.json").read_text())
    barrier = directory / "H1-barrier.json"
    h1 = json.loads(barrier.read_text()) if barrier.exists() else {"status": "BLOCKED"}
    lab.write(directory / "r6-result.json", {"status": "PASS" if result == 0 and h1["status"] == "PASS"
              else "BLOCKED", "H1": h1, "cases": underlying["cases"], "fullMilestoneAccepted": False})
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    raise SystemExit(run(args.directory.resolve(), args.cluster))
