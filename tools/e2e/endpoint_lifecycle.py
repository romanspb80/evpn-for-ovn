#!/usr/bin/env python3
"""Bounded historical F1/F2/F3/F4 acceptance; never H1 acceptance."""

import argparse
import copy
import hashlib
import json
from pathlib import Path
import signal
import subprocess
import uuid

import containerlab as lab
from cross_domain import CrossDomain, paths_for
from external_frr import BASE, LOCAL_IP, LOCAL_MAC, REMOTE_IP, REMOTE_MAC
from external_gobgp import B_BASE, B_IP, B_MAC, R_BASE, R2_IP, R2_MAC, ThreeDomainBaseline
from external_recovery import RecoveryBaseline, stable_objects
from geneve_baseline import OVN_IMAGE, require
from remote2_restart import foreign_paths


TRANSITIONS = ("F1-a", "F1-b", "F2-r1", "F2-r2", "F3-a", "F3-b")


def confirmed(status, mac):
    require(not status.get("Error") and status["Snapshot"]["Authoritative"],
            "receipt evidence is not complete and authoritative")
    receipts = [r for r in status.get("Receipts") or [] if r["Endpoint"]["MAC"] == mac]
    require(len(receipts) == 1 and receipts[0]["Status"] == "applied" and
            all(receipts[0][n]["ACK"] and receipts[0][n]["Status"] == "confirmed"
                for n in ("FDB", "Neighbor")), "endpoint lacks exact confirmed kernel receipts")
    return receipts[0]


def retired_ledger(envelope, generation, incarnation):
    record = envelope["Record"]
    state = record["State"]
    require(record["Pending"] is None and state["Phase"] == "retired" and
            state["Generation"] == generation and state["Incarnation"] == incarnation and
            len(state["Used"]) == generation and state["Used"][-1] == incarnation,
            "durable current-generation retirement not acknowledged")
    return state


def same_foreign(before, after):
    require(before == after, "foreign object identity, value or device changed")


def device_identity(link):
    result = {k: link.get(k) for k in ("ifindex", "ifname", "address", "master", "linkinfo")}
    result = copy.deepcopy(result)
    if result["linkinfo"]["info_kind"] == "bridge":
        # Live timers and learned-entry counts are observations, not identity
        # or configuration. Retain the complete raw dump separately.
        for field in ("hello_timer", "tcn_timer", "topology_change_timer", "gc_timer", "fdb_n_learned"):
            result["linkinfo"]["info_data"].pop(field, None)
    return result


class EndpointLifecycle(CrossDomain):
    # R5 uses the accepted R3 speaker configurations, not R4's RT-only
    # negative-injection policy, which also rejects an unrelated IPv4 witness.
    put = RecoveryBaseline.put
    bput = RecoveryBaseline.bput

    def __init__(self, directory, cluster):
        super().__init__(directory, cluster)
        self.foreign_expected = None
        self.foreign_checks = []
        self.docker_witness = None
        self.local_processes = None
        self.transition = "baseline"

    def inputs(self):
        super().inputs()
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/endpoint_lifecycle.py", "tools/e2e/test_endpoint_lifecycle.py"):
            target = self.directory / "source" / relative
            target.write_bytes((lab.ROOT / relative).read_bytes())
            pins[relative] = hashlib.sha256(target.read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)
        build = json.loads((self.directory / "build-inputs.json").read_text())
        build["sources"] = pins
        lab.write(self.directory / "build-inputs.json", build)

    def docker_state(self, label):
        owned = self.docker_witness
        container = json.loads(self.command(["docker", "inspect", owned["container"]],
                                            label + "-docker-container"))[0]
        network = json.loads(self.command(["docker", "network", "inspect", owned["network"]],
                                          label + "-docker-network"))[0]
        require(container["Id"] == owned["containerID"] and network["Id"] == owned["networkID"],
                "foreign Docker resource replaced")
        return {"container": {k: container[k] for k in ("Id", "Name", "Image", "Config", "State")},
                "network": {k: network[k] for k in ("Id", "Name", "Driver", "Internal", "Labels", "IPAM")}}

    def witnesses(self, label):
        evidence = {"kernel": {}, "ovs": {}, "docker": self.docker_state(label)}
        sessions = {}
        for side, run, bridge, vxlan, mac, ip in (
            ("a", lambda *a, **k: self.rt("a", *a, **k), "evpnbr33", "evpnvx33",
             "02:00:00:fa:00:01", "10.100.1.199"),
            ("r1", self.fr, "br1000", "vxlan1000", "02:00:00:fa:00:02", "10.100.1.198"),
            ("b", lambda *a, **k: self.rt("b", *a, **k), "evpnbr34", "evpnvx34",
             "02:00:00:fb:00:01", "10.100.2.199"),
            ("r2", self.r2, "br2000", "vxlan2000", "02:00:00:fb:00:01", "10.100.2.199"),
        ):
            base = {"a": BASE, "b": B_BASE, "r2": R_BASE}.get(side)
            metadata = ""
            peer_command = "printf 'null';"
            if base:
                address = {"a": "198.18.10.3", "b": "198.18.20.3", "r2": "198.18.20.2"}[side]
                peer_command = f"gobgp --target unix://{base}/gobgp.sock neighbor {address} -j;"
                paths = {
                    "a": ("export-receipts/owner.lock", "authorization-fence/owner.lock",
                          "import-receipts/receipts-v1/owner.lock", "import-receipts/remote-netlink-owner"),
                    "b": ("export/owner.lock", "fence/owner.lock", "import/receipts-v1/owner.lock",
                          "import/remote-netlink-owner"),
                    "r2": ("lifecycle/owner.lock", "export/owner.lock", "import/receipts-v1/owner.lock",
                           "import/remote-netlink-owner"),
                }[side]
                metadata = (f"m=$(stat -c '%n %u %a %d %i' {' '.join(base + '/' + p for p in paths)});"
                            " printf '%s' \"$m\" | tr '\\n' ',';")
            raw = json.loads(run("sh", "-ec",
                f"printf '{{\"links\":'; ip -d -j link show; printf ',\"fdb\":'; "
                f"bridge -j fdb show dev {vxlan}; printf ',\"neighbors\":'; "
                f"ip -j neigh show dev {bridge}; printf ',\"metadata\":\"'; {metadata} "
                f"printf '\",\"peer\":'; {peer_command} printf '}}'",
                name=label + "-" + side + "-foreign-kernel"))
            if base:
                require(raw["peer"]["conf"]["neighbor_address"] == address and
                        raw["peer"]["state"]["session_state"] == 6,
                        "endpoint lifecycle disrupted an independent BGP session")
                sessions[side] = raw["peer"]
            fdb = [e for e in raw["fdb"] if e.get("mac") == mac]
            neighbors = [e for e in raw["neighbors"] if e.get("dst") == ip]
            require(len(fdb) == len(neighbors) == 1 and fdb[0].get("dst") and
                    fdb[0].get("state") == "permanent" and neighbors[0].get("lladdr") == mac and
                    "PERMANENT" in neighbors[0].get("state", []), "foreign kernel witness missing")
            devices = [device_identity(link)
                       for link in raw["links"] if link["ifname"] in (bridge, vxlan)]
            require(len(devices) == 2, "foreign kernel device identity absent")
            evidence["kernel"][side] = {"devices": devices, "fdb": fdb, "neighbors": neighbors,
                                       "metadata": raw["metadata"]}
        for side in ("a", "b"):
            evidence["ovs"][side] = json.loads(self.rt(side, "ovs-vsctl", "--format=json",
                "--columns=_uuid,name,external_ids,ports", "list", "Bridge", "br-r5foreign",
                name=label + "-" + side + "-foreign-ovs"))
        evidence["nb"] = json.loads(self.nb("--format=json", "list", "Logical_Switch",
                                            self.foreign_uuid, name=label + "-foreign-nb"))
        frr = json.loads(self.vty("show bgp ipv4 unicast 203.0.113.0/24 json",
                                  name=label + "-foreign-frr"))
        evidence["frr"] = {"prefix": frr["prefix"], "paths": [
            {k: p.get(k) for k in ("valid", "origin", "nexthops", "peer", "metric", "localPref")}
            for p in frr["paths"]]}
        rib = json.loads(self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
            "global", "rib", "-a", "ipv4", "-j", name=label + "-foreign-bgp"))
        evidence["bgp"] = foreign_paths(rib)
        require("203.0.113.222/32" in json.dumps(evidence["bgp"]), "foreign GoBGP route missing")
        for side, reader in (("a", self.local_reader("a")), ("b", self.bread), ("r2", self.rread)):
            status = reader("status.json" if side == "r2" else "import-status.json",
                            label + "-" + side + "-no-adoption")
            require(all(mac not in json.dumps(status.get("Receipts")) for mac in
                        ("02:00:00:fa:00:01", "02:00:00:fa:00:02", "02:00:00:fb:00:01")),
                    "foreign kernel witness adopted")
        lab.write(self.directory / (label + "-foreign.json"), evidence)
        lab.write(self.directory / (label + "-sessions.json"), sessions)
        if self.foreign_expected is not None:
            same_foreign(self.foreign_expected, evidence)
        self.foreign_checks.append({"label": label, "transition": self.transition, "status": "PASS"})
        lab.write(self.directory / "foreign-checkpoints.json", self.foreign_checks)
        return evidence

    def save_state(self, label):
        super().save_state(label)
        if self.foreign_expected is not None:
            self.witnesses(label)

    def publish_policy(self, side, filename, value):
        super().publish_policy(side, filename, value)
        if self.foreign_expected is not None:
            self.witnesses(filename.removesuffix(".json") + "-published")

    def probes(self, label, domains=("1", "2", "g")):
        if self.foreign_expected is not None:
            self.witnesses(label + "-before-probes")
        super().probes(label, domains)
        if self.foreign_expected is not None:
            self.witnesses(label + "-after-probes")

    def setup_witnesses(self):
        name = self.cluster + "-foreign"
        self.command(["docker", "network", "create", "--internal", "--label",
                      "evpn.r5.witness=" + self.directory.name, name], "foreign-network-create")
        network = json.loads(self.command(["docker", "network", "inspect", name]))[0]
        self.docker_witness = {"network": name, "networkID": network["Id"],
                               "container": name, "containerID": None}
        lab.write(self.directory / "foreign-docker-owner.json", self.docker_witness)
        self.command(["docker", "create", "--name", name, "--network", name,
                      "--label", "evpn.r5.witness=" + self.directory.name, OVN_IMAGE,
                      "sleep", "infinity"], "foreign-container-create")
        container = json.loads(self.command(["docker", "inspect", name]))[0]
        self.docker_witness["containerID"] = container["Id"]
        lab.write(self.directory / "foreign-docker-owner.json", self.docker_witness)
        for side in ("a", "b"):
            self.rt(side, "ovs-vsctl", "add-br", "br-r5foreign", "--", "set", "Bridge",
                    "br-r5foreign", "external_ids:r5.witness=" + self.directory.name)
        self.fr("ip", "neigh", "add", "10.100.1.198", "lladdr", "02:00:00:fa:00:02",
                "nud", "permanent", "dev", "br1000")
        self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock", "global", "rib",
                "add", "203.0.113.222/32", "nexthop", "198.18.20.2", name="foreign-bgp-create")
        self.foreign_expected = self.witnesses("foreign-original")
        self.local_processes = self.export_continuity("original")
        self.start_phase("r5-baseline")
        self.probes("r5-baseline")
        self.stop_phase()
        self.save_state("r5-baseline")

    def export_continuity(self, label):
        result = {}
        for side, base, mac, expected in (("a", BASE, LOCAL_MAC, self.owned_uuid),
                                          ("b", B_BASE, B_MAC, self.b_uuid)):
            first = self.local_reader(side)("export-status.json")
            self.wait(lambda: (s := self.local_reader(side)("export-status.json"))["Time"] > first["Time"]
                      and not s.get("Error") and not s["Result"]["OwnershipUnverified"] and
                      len(s["Result"]["Applied"] or []) == 1 and
                      s["Result"]["Applied"][0]["PathUUID"] == expected,
                      "local exporter is not continuously authorized")
            status = self.local_reader(side)("export-status.json", label + "-" + side + "-export")
            path = "export-receipts/journal.json" if side == "a" else "export/journal.json"
            journal = self.local_reader(side)(path, label + "-" + side + "-acknowledged-export")["Record"]
            require(journal["Pending"] is None and len(journal["State"]["Receipts"] or []) == 1 and
                    journal["State"]["Receipts"][0]["Owned"] == status["Result"]["Applied"][0],
                    "local live export lacks its original acknowledged UUID receipt")
            speaker = status["SpeakerProcess"]
            pid = self.processes["agent"][0] if side == "a" else self.b_processes["agent"][0]
            ticks = self.rt(side, "sh", "-ec", f"awk '{{print $22}}' /proc/{pid}/stat",
                            name=label + "-" + side + "-agent-start").strip()
            result[side] = {"agentPID": pid, "agentStartTicks": ticks, "speaker": speaker}
            local = self.local_rib(side, label + "-" + side + "-local")
            require(len(paths_for(local, mac)) == 1, "local acknowledged route missing")
            peer = self.vty("show bgp l2vpn evpn route json", name=label + "-frr-local-export") \
                if side == "a" else json.dumps(self.b_rib(label + "-r2-local-export", True))
            require(mac in peer, "local export absent from actual remote peer")
            other = B_MAC if side == "a" else LOCAL_MAC
            require(not paths_for(local, other), "cross-domain route contamination")
        if self.local_processes is not None:
            same_foreign(self.local_processes, result)
        lab.write(self.directory / (label + "-local-continuity.json"), result)
        return result

    def source_absent(self, side, label, mac):
        self.wait(lambda: not paths_for(self.local_rib(side, label + "-rib"), mac),
                  "retired route remains in receiver RIB")
        status = self.fresh_import(side, label)
        require(not status["Receipts"] and not status["Snapshot"]["Endpoints"] and
                not status["Snapshot"]["Issues"], "retired endpoint still desired or owned")
        kernel = self.kernel(side, label)
        require(mac not in json.dumps(kernel), "receipt-owned retired kernel state remains")
        self.wait(lambda: mac not in self.rt(side, "ovs-ofctl", "dump-flows", "br-int",
                                            name=label + "-ovs-absent"), "retired OVS state remains")
        self.save_state(label + "-authoritative-absence")
        self.export_continuity(label)
        port, ip = ("ext-a", REMOTE_IP) if side == "a" else ("ext-b", R2_IP)
        name = label + "-negative-ping"
        output = self.command([self.k_binary(), "--kubeconfig", str(self.directory / "kubeconfig"),
            "--context", "kind-" + self.cluster, "-n", "geneve-test", "exec", self.pods[side], "--",
            "ip", "netns", "exec", port, "ping", "-n", "-c", "3", "-W", "2", ip], name, check=False)
        require(json.loads((self.directory / (name + ".command.json")).read_text())["exit"] == 1 and
                "3 packets transmitted, 0 received" in output, "retired endpoint still forwards")

    def remote1_identity(self, label):
        value = {"namespace": self.fr("ip", "netns", "exec", "evpnwork", "readlink", "/proc/self/ns/net",
                                      name=label + "-netns").strip(),
                 "device": json.loads(self.fr("ip", "-d", "-j", "link", "show", "dev", "frr-host0",
                                               name=label + "-device"))[0]}
        lab.write(self.directory / (label + "-endpoint.json"), value)
        return value

    def remote1(self):
        self.transition = "F2-r1"
        old = self.remote1_identity("F2-r1-before")
        receipt = confirmed(self.fresh_import("a", "F2-r1-before"), REMOTE_MAC)
        self.save_state("F2-r1-before")
        self.export_continuity("F2-r1-before")
        self.start_phase("f2-r1-retirement")
        self.witnesses("F2-r1-before-retirement")
        # Allocate the replacement while the original is live: Linux may reuse
        # a released namespace inode, which is not an endpoint generation.
        self.fr("ip", "netns", "add", "evpnwork-next", name="F2-r1-allocate-next-identity")
        # Native FRR lifecycle authority: delete this exact created namespace.
        self.fr("ip", "netns", "del", "evpnwork", name="F2-r1-authoritative-retire")
        self.witnesses("F2-r1-retired")
        self.wait(lambda: REMOTE_MAC not in self.vty("show bgp l2vpn evpn route json",
                                                   name="F2-r1-source-withdraw"),
                  "native FRR source did not withdraw retired endpoint")
        self.source_absent("a", "F2-r1-withdrawn", REMOTE_MAC)
        self.probes("F2-r1-retired-unaffected", ("2", "g"))
        self.stop_phase(("2", "g"))
        self.start_phase("f2-r1-recreation")
        self.fr("sh", "-ec",
            "test ! -e /run/netns/evpnwork; : > /run/netns/evpnwork;"
            "mount --bind /run/netns/evpnwork-next /run/netns/evpnwork;"
            "ip netns del evpnwork-next; ip link add frr-host0 type veth peer name frr-work0;"
            "ip link set frr-host0 addrgenmode none; ip link set frr-host0 master br1000;"
            "ip link set frr-host0 up; ip link set frr-work0 netns evpnwork;"
            "ip netns exec evpnwork ip link set frr-work0 addrgenmode none;"
            f"ip netns exec evpnwork ip link set frr-work0 address {REMOTE_MAC} mtu 1400;"
            f"ip netns exec evpnwork ip addr add {REMOTE_IP}/24 dev frr-work0;"
            "ip netns exec evpnwork ip link set lo up; ip netns exec evpnwork ip link set frr-work0 up;"
            f"ip netns exec evpnwork ip neigh add 10.100.1.1 lladdr {self.frr_bridge_mac} "
            "dev frr-work0 nud permanent; ip netns exec evpnwork ping -n -c 2 -W 2 10.100.1.1",
            name="F2-r1-recreate")
        new = self.remote1_identity("F2-r1-recreated")
        require(new["namespace"] != old["namespace"] and
                new["device"]["ifindex"] != old["device"]["ifindex"], "Remote1 reused retired identity")
        self.wait(lambda: bool(paths_for(self.local_rib("a", "F2-r1-recreated-rib"), REMOTE_MAC)),
                  "new FRR endpoint not advertised")
        self.wait(lambda: any(r["Endpoint"]["MAC"] == REMOTE_MAC for r in
                  self.import_status("a", "F2-r1-recreated-poll")["Receipts"] or []),
                  "new FRR endpoint not imported")
        fresh = confirmed(self.fresh_import("a", "F2-r1-recreated"), REMOTE_MAC)
        require(fresh["Incarnation"] > receipt["Incarnation"], "retired importer ownership reused")
        self.fr("ip", "netns", "exec", "evpnwork", "ip", "neigh", "add", LOCAL_IP,
                "lladdr", LOCAL_MAC, "dev", "frr-work0", "nud", "permanent")
        self.export_continuity("F2-r1-recreated")
        self.converge_pair("a", "F2-r1-recreated")
        self.probes("F2-r1-recreated")
        self.stop_phase()
        self.save_state("F2-r1-recreated")
        lab.write(self.directory / "F2-r1-result.json",
                  {"status": "PASS", "old": old, "new": new, "oldReceipt": receipt, "newReceipt": fresh})

    def remote2_retire(self, label):
        before = self.status(label + "-before")
        owned = before["Export"]["Applied"][0]
        self.export_continuity(label + "-before")
        self.bput("endpoint.json", {"Generation": self.r_generation,
            "Incarnation": self.r_incarnation, "Retired": True}, True)
        self.witnesses(label + "-retirement-published")
        self.bwait("status.json", lambda s: not s["Error"] and s["Lifecycle"]["Phase"] == "retired"
                   and not s["Export"]["Applied"], label + "-retired", True)
        retired_ledger(self.rread("lifecycle/journal.json", label + "-ledger"),
                       self.r_generation, self.r_incarnation)
        journal = self.rread("export/journal.json", label + "-export-drained")["Record"]
        require(journal["Pending"] is None and not journal["State"]["Receipts"],
                "Remote2 exact UUID not durably withdrawn")
        require(not paths_for(self.b_rib(label + "-source", True), R2_MAC),
                "retired acknowledged UUID route remains at source")
        self.source_absent("b", label + "-withdrawn", R2_MAC)
        after = self.status(label + "-independent-import")
        require(stable_objects(before["Receipts"]) == stable_objects(after["Receipts"]),
                "endpoint retirement altered independent importer ownership")
        self.export_continuity(label + "-retired")
        return before, owned

    def lifecycle_remote2(self):
        self.transition = "F2-r2"
        before_receipt = confirmed(self.fresh_import("b", "F2-r2-before"), R2_MAC)
        self.save_state("F2-r2-before")
        self.start_phase("f2-r2-retirement")
        before, owned = self.remote2_retire("F2-r2")
        self.probes("F2-r2-retired-unaffected", ("1", "g"))
        self.stop_phase(("1", "g"))
        self.start_phase("f2-r2-recreation")
        self.recreate_r2_generation()
        restored = self.ready("F2-r2-recreated", before["Time"])
        ledger = self.rread("lifecycle/journal.json", "F2-r2-recreated-ledger")["Record"]
        require(ledger["Pending"] is None and ledger["State"]["Generation"] ==
                before["Lifecycle"]["Generation"] + 1 and ledger["State"]["Phase"] == "active" and
                len(set(ledger["State"]["Used"])) == self.r_generation and
                restored["Export"]["Applied"][0]["PathUUID"] != owned["PathUUID"],
                "Remote2 recreation reused generation, incarnation or UUID")
        fresh = confirmed(self.fresh_import("b", "F2-r2-recreated"), R2_MAC)
        require(fresh["Incarnation"] > before_receipt["Incarnation"], "old B importer ownership adopted")
        self.r_uuid = restored["Export"]["Applied"][0]["PathUUID"]
        self.bput("endpoint.json", {"Generation": before["Lifecycle"]["Generation"],
            "Incarnation": before["Lifecycle"]["Incarnation"], "Retired": False}, True)
        rejected = self.bwait("status.json", lambda s: "stale endpoint generation" in s["Error"],
                              "F2-r2-stale-rejected", True)
        require(rejected["Export"]["Applied"][0]["PathUUID"] == self.r_uuid and
                self.rread("export/journal.json")["Record"]["State"]["Receipts"][0]["Owned"]["PathUUID"]
                == self.r_uuid, "stale generation regained UUID ownership")
        self.witnesses("F2-r2-stale-rejected")
        self.bput("endpoint.json", {"Generation": self.r_generation,
            "Incarnation": self.r_incarnation, "Retired": False}, True)
        self.ready("F2-r2-restored")
        self.export_continuity("F2-r2-recreated")
        self.converge_pair("b", "F2-r2-recreated")
        self.probes("F2-r2-recreated")
        self.stop_phase()
        self.save_state("F2-r2-recreated")
        lab.write(self.directory / "F2-r2-result.json", {"status": "PASS",
            "oldUUID": owned["PathUUID"], "newUUID": self.r_uuid, "ledger": ledger,
            "oldReceipt": before_receipt, "newReceipt": fresh, "staleRejected": rejected})

    def retire_b_restore(self):
        self.transition = "F1-b"
        old = copy.deepcopy(self.b_endpoint)
        self.save_state("F1-b-before")
        self.start_phase("f1-b")
        ThreeDomainBaseline.b_uncertainty(self)
        self.probes("F1-b-uncertain", ("1", "g"))
        ThreeDomainBaseline.b_withdraw(self)
        self.probes("F1-b-retired", ("1", "g"))
        self.stop_phase(("1", "g"))
        self.save_state("F1-b-retired")
        self.start_phase("f1-b-recreated")
        self.activate_next_generation("b", "F1-b-recreated", old, recreate=True)
        self.ready("F1-b-recreated")
        self.converge_pair("b", "F1-b-recreated")
        self.probes("F1-b-recreated")
        self.stop_phase()
        self.save_state("F1-b-recreated")

    def final_withdrawals(self):
        self.transition = "final-withdrawal"
        self.start_phase("f2-final-r2")
        self.remote2_retire("F2-r2-final")
        self.probes("F2-r2-final-unaffected", ("1", "g"))
        self.stop_phase(("1", "g"))
        self.start_phase("f2-final-r1")
        self.fr("ip", "netns", "del", "evpnwork", name="F2-r1-final-authoritative-retire")
        self.wait(lambda: REMOTE_MAC not in self.vty("show bgp l2vpn evpn route json"),
                  "final Remote1 source withdrawal incomplete")
        self.source_absent("a", "F2-r1-final", REMOTE_MAC)
        self.probes("F2-final-geneve", ("g",))
        self.stop_phase(("g",))
        self.witnesses("F4-before-scoped-cleanup")
        # Local drain is housekeeping, not the quarantined H1 acceptance.
        self.foreign_expected = None
        for side, base, policy in (("a", BASE, self.policy), ("b", B_BASE, self.b_policy)):
            endpoint = self.endpoint if side == "a" else self.b_endpoint
            port = endpoint["logicalSwitchPort"]
            self.nb("lsp-del", endpoint["logicalSwitchPortUUID"])
            self.rt(side, "ovs-vsctl", "del-port", "br-int", port + "-host")
            self.rt(side, "ip", "link", "del", port + "-host")
            self.rt(side, "ip", "netns", "del", port)
            self.wait(lambda: not self.sb("--bare", "--columns=_uuid", "find",
                      "Port_Binding", "logical_port=" + port).strip(), "final local binding remains")
            policy(1, False, "deleted")
            self.wait(lambda: not (s := self.local_reader(side)("export-status.json")).get("Error")
                      and not s["Result"]["Applied"], "final local UUID not withdrawn")
            path = "export-receipts/journal.json" if side == "a" else "export/journal.json"
            journal = self.local_reader(side)(path, "final-" + side + "-export")["Record"]
            require(journal["Pending"] is None and not journal["State"]["Receipts"],
                    "final local UUID receipt not drained")
        self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"]
                   and not s["Receipts"], "final-r2-import-drained", True)
        self.wait(lambda: LOCAL_MAC not in self.vty("show bgp l2vpn evpn route json") and
                  not paths_for(self.b_rib("final-r2-source", True), B_MAC),
                  "final local exports remain at remote peers")
        self.cleanup_after_withdrawals()

    def cleanup_after_withdrawals(self):
        self.r2("ip", "netns", "del", "remote2-work")
        self.foreign_kernel()
        self.b_foreign("F4-before-witness-owner-cleanup")
        self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock", "global", "rib",
                "del", "203.0.113.222/32", "nexthop", "198.18.20.2", name="foreign-bgp-owner-cleanup")
        self.fr("ip", "neigh", "del", "10.100.1.198", "lladdr", "02:00:00:fa:00:02", "dev", "br1000")
        self.b_cleanup()
        self.external_cleanup()
        # The unrelated OVS and NB objects must survive lifecycle-owner cleanup.
        for side in ("a", "b"):
            after = json.loads(self.rt(side, "ovs-vsctl", "--format=json",
                "--columns=_uuid,name,external_ids,ports", "list", "Bridge", "br-r5foreign",
                name="F4-post-cleanup-" + side + "-ovs"))
            original = json.loads((self.directory / "foreign-original-foreign.json").read_text())
            same_foreign(original["ovs"][side], after)
            self.rt(side, "ovs-vsctl", "del-br", "br-r5foreign", name="F4-witness-owner-" + side)
        self.remove_endpoints()
        self.foreign()
        same_foreign(original["docker"], self.docker_state("F4-pre-teardown"))

    def cleanup_docker_witness(self):
        if self.docker_witness is None:
            return
        owner = self.docker_witness
        if owner["containerID"] is not None:
            current = json.loads(self.command(["docker", "inspect", owner["container"]]))[0]
            require(current["Id"] == owner["containerID"] and
                    current["Config"]["Labels"]["evpn.r5.witness"] == self.directory.name,
                    "refuse cleanup of changed Docker witness")
            self.command(["docker", "rm", owner["containerID"]], "foreign-container-owner-cleanup")
        network = json.loads(self.command(["docker", "network", "inspect", owner["network"]]))[0]
        require(network["Id"] == owner["networkID"] and
                network["Labels"]["evpn.r5.witness"] == self.directory.name, "foreign network owner changed")
        self.command(["docker", "network", "rm", owner["networkID"]], "foreign-network-owner-cleanup")

    def teardown_allowed(self):
        return True


def run(directory, cluster, fixture_type=EndpointLifecycle):
    require(not directory.exists() and directory != lab.ROOT and lab.ROOT not in directory.parents,
            "new evidence directory outside repository required")
    directory.mkdir(mode=0o700, parents=True)
    fixture = fixture_type(directory, cluster)
    cases = {n: {"status": "NOT_RUN"} for n in
             ("inputs", "baseline", "F1", "F2-r1", "F2-r2", "F3", "F4", "owned-cleanup", "cleanup")}
    current = "inputs"

    def interrupted(signum, _frame):
        raise lab.LabError(f"R5 interrupted by signal {signum}")

    handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    with lab.exclusive():
        try:
            fixture.inputs()
            cases[current] = {"status": "PASS"}
            current = "baseline"
            for action in (fixture.provision, fixture.ovs, fixture.chassis, fixture.endpoints,
                           fixture.bindings, fixture.connectivity, fixture.packets, fixture.no_evpn,
                           fixture.fixture, fixture.runtime, fixture.export, fixture.import_route,
                           fixture.forwarding, fixture.coexistence, fixture.b_fixture,
                           fixture.b_runtime, fixture.b_export, fixture.b_import, fixture.three_packets,
                           fixture.setup_witnesses):
                print("R5 prerequisite=" + action.__name__, flush=True)
                action()
            cases[current] = {"status": "PASS"}
            current = "F1"
            fixture.transition = "F1-a"
            fixture.retire_a()
            fixture.retire_b_restore()
            cases[current] = {"status": "PASS"}
            for current, action in (("F2-r1", fixture.remote1), ("F2-r2", fixture.lifecycle_remote2)):
                print("R5 scenario=" + current, flush=True)
                action()
                cases[current] = {"status": "PASS"}
            current = "F3"
            # Existing helper exercises both real LSP replacements and stale replay.
            fixture.transition = "F3-a/b"
            fixture.local_replacements()
            cases[current] = {"status": "PASS"}
            current = "F4"
            require(all(any(c["transition"] == t or t.startswith("F3-") and c["transition"] == "F3-a/b"
                            for c in fixture.foreign_checks) for t in TRANSITIONS),
                    "F4 historical transition coverage incomplete")
            fixture.witnesses("F4-all-transitions-complete")
            cases[current] = {"status": "PASS"}
            current = "owned-cleanup"
            fixture.final_withdrawals()
            cases[current] = {"status": "PASS"}
        except (lab.LabError, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
            cases[current] = {"status": "FAIL", "detail": str(error)}
            (directory / "first-failure.txt").write_text(str(error) + "\n")
            print(f"R5 FAIL {current}: {error}", flush=True)
        finally:
            errors = []
            if (directory / "lab.json").exists() and lab.load(directory)["phase"] not in ("reserved", "down"):
                for action in (lambda: fixture.stop_phase(verify=False),
                               lambda: fixture.stop_external_capture(require_packets=False)
                               if fixture.ext_captures else None,
                               lambda: fixture.capture_stop() if fixture.captures else None,
                               lambda: lab.collect(directory)):
                    try:
                        action()
                    except (lab.LabError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
                        errors.append(str(error))
                try:
                    original = None
                    if fixture.docker_witness and fixture.docker_witness["containerID"] is not None:
                        try:
                            original = fixture.docker_state("F4-before-lab-down")
                        except (lab.LabError, OSError, ValueError, KeyError) as error:
                            errors.append("Docker witness before teardown: " + str(error))
                    require(fixture.teardown_allowed(),
                            "pre-destruction barrier not accepted; retain owners, topology and lease")
                    lab.down(directory)
                    inventory = lab.inventory(lab.load(directory))
                    require(not any(inventory.values()), "owned topology resources remain")
                    require(not (lab.STATE_ROOT / "active.json").exists(), "coordinator lease not released")
                    if original:
                        same_foreign(original, fixture.docker_state("F4-after-lab-down"))
                    fixture.cleanup_docker_witness()
                    require(not errors, "capture cleanup failed: " + "; ".join(errors))
                    cases["cleanup"] = {"status": "PASS", "inventory": inventory,
                                        "foreignDockerSurvivedTopologyCleanup": original is not None}
                except (lab.LabError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
                    cases["cleanup"] = {"status": "BLOCKED", "detail": str(error), "errors": errors}
            else:
                cases["cleanup"] = {"status": "PASS", "detail": "no provisioned topology"}
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
            status = "PASS" if all(c["status"] == "PASS" for c in cases.values()) else "BLOCKED"
            lab.write(directory / "r5-result.json", {"status": status, "cases": cases,
                      "H1": "NOT_RUN", "fullMilestoneAccepted": False})
            print(json.dumps({"status": status, "cases": cases}), flush=True)
    return 0 if status == "PASS" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    raise SystemExit(run(args.directory.resolve(), args.cluster))
