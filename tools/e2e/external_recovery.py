"""Bounded isolation/recovery acceptance on the existing three-domain fixture."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shlex
import time
import uuid

import containerlab as lab
from external_frr import (
    BASE, LOCAL_IP, LOCAL_MAC, REMOTE_IP, REMOTE_MAC, decode_vxlan, validate_raw_rib,
)
from external_gobgp import (
    B_BASE, B_IP, B_MAC, R_BASE, R2_IP, R2_MAC, ThreeDomainBaseline, validate_rib,
)
from geneve_baseline import ENDPOINTS, decode_geneve, require


CASES = (
    "focused_isolation_recovery", "B1_live_identities", "B2_foreign_binding", "E3_no_transit",
    "G1_A_restart_recovery", "G2_B_restart_recovery", "A_speaker_replacement",
    "B_speaker_replacement", "R2_projector_restart_blocker", "R2_speaker_restart_blocker",
    "R2_combined_restart_blocker", "F3_local_replacement",
    "B_live_route_rejections", "A2_disable_restore", "F1_A_retirement",
)
FOCUSED = (
    "TestRemoteType2DecodePolicy|TestReadOnlyType2ImporterSnapshots|TestEndpointExporterSafety|"
    "TestReadOnlyType2ConflictsAndQuarantine|"
    "TestReadOnlyType2MixedConflictsFreezeOwnedState|"
    "TestExportRecoveryCrashBoundaries|TestExportRecoveryIncarnationAndForeignConflicts"
)


def stable_objects(receipts):
    return [(r["Endpoint"], r["FDB"], r.get("Neighbor")) for r in receipts]


def complete_probe(output):
    return re.search(r"3 packets transmitted, 3 (?:packets )?received", output) is not None


def rejected_issue(status, reason):
    snapshot = status["Snapshot"]
    return snapshot["Authoritative"] and any(
        issue["Reason"] == reason for issue in snapshot["Issues"] or [])


def validate_injected_rib(rib, config):
    endpoint = config["Endpoint"]
    paths = [path for group in rib.values() for path in group
             if path["nlri"]["value"].get("mac") == endpoint["MACAddress"]]
    require(len(paths) == 1, "injected route identity is absent or ambiguous")
    path = paths[0]
    route = path["nlri"]["value"]
    admin, assigned = endpoint["RouteDistinguisher"].rsplit(":", 1)
    require(path["nlri"]["type"] == 2 and route["ip"] == endpoint["IPAddresses"][0] and
            route["rd"] == {"type": 1, "admin": admin, "assigned": int(assigned)} and
            route["labels"] == [endpoint["VNI"]] and route["etag"] == 0 and
            route["esi"] == "single-homed", "injected NLRI attributes differ")
    attrs = {attr["type"]: attr for attr in path["attrs"]}
    require(attrs[14]["nexthop"] == endpoint["LocalVTEP"] and attrs[14]["afi"] == 25 and
            attrs[14]["safi"] == 70 and path["peer-address"] == config["RouterID"] and
            [community["value"] for community in attrs[16]["value"]
             if community.get("subtype") == 2] == endpoint["RouteTargets"] and
            any(community.get("tunnel_type") == 8 for community in attrs[16]["value"]),
            "injected MP_REACH, RT, encapsulation or peer provenance differs")
    return path


class RecoveryBaseline(ThreeDomainBaseline):
    def __init__(self, directory, cluster):
        super().__init__(directory, cluster)
        names = list(self.cases)
        position = names.index("projector_uncertainty_conflict")
        names[position:position] = CASES
        self.cases = {n: {"status": "BLOCKED", "detail": "not reached"} for n in names}
        self.revisions = {"a": 0, "b": 0}
        self.phase_capture = None
        self.restarts = {}
        self.a_retirement_verified = False
        self.scenarios = {n: {"status": "PLANNED", "detail": "full definition not exercised"}
                          for n in ("A2", "B1", "B2", "E1", "E2", "E3", "E4",
                                    "F1", "F2", "F3", "F4", "F5", "G1", "G2", "G3", "H1")}

    def inputs(self):
        super().inputs()
        self.command(["go", "test", "-c", "-o", str(self.directory / "build/core.test"),
                      "./pkg/evpn"], "build-focused-core")
        for package in ("pkg/nodeagent/ovnexport", "pkg/endpointauth"):
            self.command(["go", "test", "-c", "-o",
                          str(self.directory / ("build/" + package.rsplit("/", 1)[-1] + ".test")),
                          "./" + package], "build-focused-" + package.rsplit("/", 1)[-1])
        self.command(["go", "build", "-o", str(self.directory / "build/inject"),
                      "./tools/e2e/inject"], "build-inject")
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/external_recovery.py", "tools/e2e/test_external_recovery.py",
                         "pkg/evpn/export_recovery_test.go", "pkg/evpn/remote_type2_test.go",
                         "pkg/evpn/remote_type2_mixed_test.go", "pkg/evpn/exporter_test.go",
                         "tools/e2e/inject/main.go", "tools/e2e/inject/main_test.go",
                         "pkg/nodeagent/ovnexport/authorization_test.go",
                         "pkg/endpointauth/fence_test.go",
                         "tools/lib/cni-evpn-frr-interop-test.sh",
                         "tools/lib/test_cni_capture_lifecycle.py"):
            target = self.directory / "source" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((lab.ROOT / relative).read_bytes())
            pins[relative] = hashlib.sha256(target.read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)

    def external_actions(self):
        actions = list(super().external_actions())
        position = actions.index(self.projector_uncertainty)
        actions[position:position] = [
            self.focused, self.identities_case, self.foreign_binding,
            self.no_transit,
            lambda: self.restart_agent_recovery("a"),
            lambda: self.restart_agent_recovery("b"),
            lambda: self.restart_speaker("a"),
            lambda: self.restart_speaker("b"),
            lambda: self.restart_blocker("r2-projector"),
            lambda: self.restart_blocker("r2-speaker"),
            lambda: self.restart_blocker("r2-combined"),
            self.local_replacements,
            self.route_rejections,
            self.disable_restore,
            self.retire_a,
        ]
        return tuple(actions)

    def policy(self, revision, active, reason=None):
        revision = max(revision, self.revisions["a"] + 1)
        super().policy(revision, active, reason)
        self.revisions["a"] = revision

    def b_policy(self, revision, active, reason=None):
        revision = max(revision, self.revisions["b"] + 1)
        super().b_policy(revision, active, reason)
        self.revisions["b"] = revision

    def bput(self, filename, value, remote=False):
        if filename == "observation" and remote:
            require(value in ("incomplete\n", "conflict\n"), "unsupported recovery observation seam")
            mode = value.strip()
            (self.directory / ("r2-" + mode + "-control.txt")).write_text(value)
            self.b_rib("r2-" + mode + "-actual-source", True)
        super().bput(filename, value, remote)

    def process_record(self, side, name):
        base = {"a": BASE, "b": B_BASE, "r2": R_BASE}[side]
        return json.loads((self.directory / f"{'' if side == 'a' else side + '-'}"
                          f"{name}-process.json").read_text()), base

    def save_state(self, label):
        self.snapshot(label + "-a")
        for side, read in (("b", self.bread), ("r2", self.rread)):
            paths = (("export/journal.json", "export"), ("import/receipts-v1/journal.json", "import"))
            if side == "b":
                paths += (("fence/journal.json", "fence"),)
            for path, name in paths:
                read(path, f"{label}-{side}-{name}")
            self.b_rib(label + "-" + side, remote=side == "r2")
            run = self.r2 if side == "r2" else lambda *a, **kw: self.rt("b", *a, **kw)
            run("bridge", "-j", "fdb", "show", name=label + "-" + side + "-fdb")
            run("ip", "-j", "neigh", "show", name=label + "-" + side + "-neighbors")
        self.rt("b", "ovs-vsctl", "--format=json", "list", "Interface", name=label + "-b-ovs")
        self.rt("b", "ovs-ofctl", "dump-flows", "br-int", name=label + "-b-flows")
        self.b_foreign(label + "-foreign")

    def probes(self, label, domains=("1", "2", "g")):
        results = {}
        if "1" in domains:
            results["a-r1"] = self.rt("a", "ip", "netns", "exec", "ext-a", "ping", "-n", "-c", "3",
                                      "-W", "3", REMOTE_IP, name=label + "-a-r1")
            results["r1-a"] = self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "3",
                                      "-W", "3", LOCAL_IP, name=label + "-r1-a")
        if "2" in domains:
            results["b-r2"] = self.rt("b", "ip", "netns", "exec", "ext-b", "ping", "-n", "-c", "3",
                                      "-W", "3", R2_IP, name=label + "-b-r2")
            results["r2-b"] = self.r2("ip", "netns", "exec", "remote2-work", "ping", "-n", "-c", "3",
                                      "-W", "3", B_IP, name=label + "-r2-b")
        if "g" in domains:
            for source, destination in (("a", "b"), ("b", "a")):
                results["geneve-" + source] = self.rt(
                    source, "ip", "netns", "exec", "local-" + source, "ping",
                    "-n", "-c", "3", "-W", "3", ENDPOINTS[destination][1],
                    name=label + "-geneve-" + source)
        for name, output in results.items():
            require(complete_probe(output), label + "-" + name + " lost required probes")

    def converge_pair(self, side, label):
        local, remote = ("ext-a", "evpnwork") if side == "a" else ("ext-b", "remote2-work")
        remote_ip, local_ip = (REMOTE_IP, LOCAL_IP) if side == "a" else (R2_IP, B_IP)
        local_command = [self.k_binary(), "--kubeconfig", str(self.directory / "kubeconfig"),
                         "--context", "kind-" + self.cluster, "-n", "geneve-test", "exec",
                         self.pods[side], "--"]
        remote_command = ["docker", "exec", self.frr if side == "a" else self.remote2]
        attempts = []
        for attempt in range(1, 5):
            forward = self.command(local_command + ["ip", "netns", "exec", local, "ping", "-n",
                                   "-c", "3", "-W", "3", remote_ip],
                                   name=f"{label}-forward-{attempt}", check=False)
            reverse = self.command(remote_command + ["ip", "netns", "exec", remote, "ping", "-n",
                                   "-c", "3", "-W", "3", local_ip],
                                   name=f"{label}-reverse-{attempt}", check=False)
            attempts.append({"attempt": attempt, "forward": complete_probe(forward),
                             "reverse": complete_probe(reverse)})
            lab.write(self.directory / (label + "-convergence.json"), attempts)
            if attempts[-1]["forward"] and attempts[-1]["reverse"]:
                return
            time.sleep(1)
        raise lab.LabError(label + " did not restore complete bidirectional probes")

    def start_phase(self, label, access=False):
        require(self.phase_capture is None, "overlapping phase captures")
        state = self.directory / (label + "-capture-state.sh")
        self.phase_capture = (label, state)
        names = [label + "-" + n for n in ("a", "r1", "b", "r2", "ga", "gb")]
        if access:
            names += [label + "-r1-access", label + "-r2-access"]
        script = self.capture_preamble() + f"""
INTEROP_CAPTURE_OUTPUT_NAMES=({' '.join(names)})
trap 'declare -p INTEROP_CAPTURE_STAGING_DIR INTEROP_CAPTURE_UNITS INTEROP_CAPTURE_NAMES INTEROP_CAPTURE_MAIN_PIDS INTEROP_CAPTURE_CGROUPS INTEROP_CAPTURE_OUTPUT_NAMES > {shlex.quote(str(state))}' EXIT
interop_capture_create_staging_dir
"""
        for name, container, run, address, expression in (
            ("a", self.cluster + "-worker", lambda *a: self.rt("a", *a), "198.18.10.2", "udp port 4789"),
            ("r1", self.frr, self.fr, "198.18.10.3", "udp port 4789"),
            ("b", self.cluster + "-worker2", lambda *a: self.rt("b", *a), "198.18.20.2", "udp port 4789"),
            ("r2", self.remote2, self.r2, "198.18.20.3", "udp port 4789"),
            ("ga", self.cluster + "-worker", lambda *a: self.rt("a", *a), "198.18.30.2", "udp port 6081"),
            ("gb", self.cluster + "-worker2", lambda *a: self.rt("b", *a), "198.18.30.3", "udp port 6081"),
        ):
            devices = [link["ifname"] for link in json.loads(run("ip", "-j", "-4", "addr"))
                       if any(a.get("local") == address for a in link.get("addr_info", []))]
            require(len(devices) == 1, "capture VTEP interface not unique")
            pid = int(self.command(["docker", "inspect", "--format", "{{.State.Pid}}", container]))
            script += (f"interop_capture_start_one {label}-{name} {pid} {shlex.quote(devices[0])} "
                       f"{shlex.quote(expression)} 180\n")
        if access:
            for side, container, device in (("r1", self.frr, "frr-host0"),
                                             ("r2", self.remote2, "r2-host")):
                pid = int(self.command(["docker", "inspect", "--format", "{{.State.Pid}}", container]))
                script += f"interop_capture_start_one {label}-{side}-access {pid} {device} 'icmp' 180\n"
        script += "interop_capture_assert_active recovery-phase-ready\n"
        self.command(["bash", "-ec", script], label + "-capture-launch")
        state.chmod(0o600)

    def stop_phase(self, domains=("1", "2", "g"), verify=True):
        if self.phase_capture is None:
            return
        label, state = self.phase_capture
        stop_name = label + "-capture-stop"
        if (self.directory / (stop_name + ".json")).exists():
            stop_name += "-cleanup"
        self.command(["bash", "-ec", self.capture_preamble() + f"""
source {shlex.quote(str(state))}
INTEROP_CAPTURE_VERIFY_COMMAND=(true)
rc=0
interop_capture_stop || rc=$?
printf '{{"shutdownFailure":%s,"verificationFailure":%s,"exit":%s}}\\n' "$INTEROP_CAPTURE_SHUTDOWN_FAILURE" "$INTEROP_PACKET_VERIFICATION_FAILURE" "$rc" > "$EVIDENCE_DIR/{stop_name}.json"
exit "$rc"
"""], stop_name, check=False)
        result = json.loads((self.directory / (stop_name + ".json")).read_text())
        require(result["shutdownFailure"] == result["exit"] == 0, "phase capture shutdown unsafe")
        self.phase_capture = None
        if not verify:
            return
        evidence = {}
        for side in ("a", "r1") if "1" in domains else ():
            evidence[side] = decode_vxlan(self.directory / f"{label}-{side}.pcap")
        for side in ("b", "r2") if "2" in domains else ():
            evidence[side] = decode_vxlan(self.directory / f"{label}-{side}.pcap", 2000,
                                         "198.18.20.2", "198.18.20.4", B_MAC, R2_MAC, B_IP, R2_IP)
        for side in ("ga", "gb") if "g" in domains else ():
            evidence[side] = decode_geneve(self.directory / f"{label}-{side}.pcap", self.tunnel_key)
        lab.write(self.directory / (label + "-packets.json"), evidence)

    def focused(self):
        output = self.command(["sudo", "-n", str(self.directory / "build/core.test"),
                               "-test.v", "-test.run=^(" + FOCUSED + ")$"], "focused-core")
        require("--- SKIP:" not in output, "focused safety evidence contains skipped tests")
        for package in ("ovnexport", "endpointauth"):
            output = self.command([str(self.directory / ("build/" + package + ".test")), "-test.v"],
                                  "focused-" + package)
            require("--- SKIP:" not in output, "focused authorization evidence contains skipped tests")
        self.scenarios["G3"] = {"status": "PASS", "detail": "focused old-incarnation and ambiguous AddPath "
                               "boundaries; live replacement reported separately", "evidence": "focused-core.stdout"}
        self.scenarios["E4"] = {"status": "PASS", "detail": "focused conflicting VTEP/IP and duplicate identity "
                               "freeze; existing state retained", "evidence": "focused-core.stdout"}
        self.scenarios["F5"] = {"status": "PASS", "detail": "deterministic incomplete source/RIB observations "
                               "freeze writes; reads restore and subsequent confirmed deletion succeeds",
                               "evidence": "focused-core.stdout"}

    def identities_case(self):
        self.save_state("B1")
        a = self.read_json(BASE + "/export-status.json")
        b = self.bread("export-status.json")
        require(a["SpeakerProcess"]["PID"] != b["SpeakerProcess"]["PID"], "shared speaker process")
        require(self.identities["a"]["nodeUID"] != self.identities["b"]["nodeUID"] and
                self.owned_uuid != self.b_uuid, "shared node or export ownership")
        validate_rib(self.b_rib("B1-b"))
        self.scenarios["B1"] = {"status": "PASS", "detail": "independent actual agents, attested speakers, "
                               "sessions, policies, durable domains and devices", "evidence": "B1-*"}

    def foreign_binding(self):
        # A B-scoped candidate deliberately refers to the actual A-bound LSP.
        policy = self.bread("policy.json", "B2-before-policy")
        candidate = copy.deepcopy(self.endpoint)
        policy["revision"] = self.revisions["b"] + 1
        policy["endpoints"].append(candidate)
        self.bput("B2-candidate.json", policy)
        self.rt("b", "sh", "-ec", f"cp {B_BASE}/B2-candidate.json {B_BASE}/policy.next && "
                f"chmod 600 {B_BASE}/policy.next && mv {B_BASE}/policy.next {B_BASE}/policy.json")
        self.revisions["b"] = policy["revision"]
        before = self.b_uuid
        status = self.bwait("export-status.json", lambda s: any(
            o.get("EndpointID") == candidate["endpointID"] for o in s.get("Observations") or []),
            "B2-rejected")
        require(LOCAL_MAC not in json.dumps(status["Result"]["Applied"]) and
                status["Result"]["Applied"][0]["PathUUID"] == before and
                LOCAL_MAC not in json.dumps(self.b_rib("B2-rib")) and
                self.sb("get", "Port_Binding", "ext-a", "chassis").strip() ==
                self.identities["a"]["chassisUUID"], "non-owner candidate changed export or binding")
        self.save_state("B2-rejected-state")
        self.b_policy(1, True)
        self.bwait("export-status.json", lambda s: not s.get("Error") and
                   s["Result"]["Applied"][0]["PathUUID"] == before, "B2-restored")
        self.scenarios["B2"] = {"status": "PASS", "detail": "B-scoped A-bound candidate rejected; "
                               "B ownership unchanged; candidate removed at higher revision",
                               "evidence": "B2-*"}

    def restart_blocker(self, component):
        remote = component.startswith("r2")
        run = self.r2 if remote else lambda *a, **kw: self.rt(component, *a, **kw)
        device = "vxlan2000" if remote else "evpnvx33" if component == "a" else "evpnvx34"
        entries = json.loads(run("bridge", "-j", "fdb", "show", "dev", device,
                                name=component + "-restart-blocker-fdb"))
        mac = "02:00:00:fa:00:01" if component == "a" else "02:00:00:fb:00:01"
        require(any(e.get("mac") == mac and e.get("dst") and e.get("state") == "permanent"
                    for e in entries), "required foreign restart witness missing")
        detail = (f"BLOCKED: no restart recovery was executed for {component}; its foreign witness "
                  "was retained and no sentinel or journal bypass was attempted.")
        self.restarts[component] = {"status": "BLOCKED", "detail": detail,
                                   "evidence": component + "-restart-blocker-fdb.stdout"}
        lab.write(self.directory / (component + "-restart-blocker.json"), self.restarts[component])

    def restart_agent_recovery(self, side):
        require(side in ("a", "b"), "unsupported Node-Agent restart side")
        label = "G1" if side == "a" else "G2"
        base = BASE if side == "a" else B_BASE
        vxlan = "evpnvx33" if side == "a" else "evpnvx34"
        bridge = "evpnbr33" if side == "a" else "evpnbr34"
        foreign_mac = "02:00:00:fa:00:01" if side == "a" else "02:00:00:fb:00:01"
        foreign_dst = "198.18.10.99" if side == "a" else "198.18.20.99"
        foreign_ip = "10.100.1.199" if side == "a" else "10.100.2.199"
        foreign_neighbor_mac = foreign_mac
        owned_mac, owned_ip, owned_vtep = (
            (REMOTE_MAC, REMOTE_IP, "198.18.10.4") if side == "a" else
            (R2_MAC, R2_IP, "198.18.20.4"))
        read = ((lambda path, name=None: self.read_json(f"{base}/{path}", name)) if side == "a"
                else self.bread)
        run = lambda *args, **kwargs: self.rt(side, *args, **kwargs)
        journal_path = ("import-receipts/receipts-v1/journal.json" if side == "a" else
                        "import/receipts-v1/journal.json")
        export_journal_path = ("export-receipts/journal.json" if side == "a" else
                               "export/journal.json")

        def exact_foreign_state():
            fdb = json.loads(run("bridge", "-j", "fdb", "show", "dev", vxlan,
                                 name=f"{label}-foreign-fdb"))
            neighbors = json.loads(run("ip", "-j", "neigh", "show", "dev", bridge,
                                       name=f"{label}-foreign-neighbors"))
            fdb_entry = next((entry for entry in fdb if entry.get("mac") == foreign_mac and
                              entry.get("dst") == foreign_dst), None)
            neighbor = next((entry for entry in neighbors if entry.get("dst") == foreign_ip and
                             entry.get("lladdr") == foreign_neighbor_mac), None)
            require(fdb_entry is not None and fdb_entry.get("state") == "permanent" and
                    "self" in (fdb_entry.get("flags") or []),
                    f"{side} permanent foreign FDB witness missing")
            neighbor_states = neighbor.get("state", []) if neighbor else []
            if isinstance(neighbor_states, str):
                neighbor_states = [neighbor_states]
            require(neighbor is not None and
                    "PERMANENT" in [state.upper() for state in neighbor_states],
                    f"{side} permanent foreign neighbor witness missing")

            def project(entry, fields):
                projected = {field: entry.get(field) for field in fields}
                if isinstance(projected.get("flags"), list):
                    projected["flags"] = sorted(projected["flags"])
                if isinstance(projected.get("state"), list):
                    projected["state"] = sorted(projected["state"])
                return projected

            return {
                "FDB": project(fdb_entry, ("mac", "dst", "flags", "state", "vlan", "vni")),
                "Neighbor": project(neighbor, ("dst", "lladdr", "state", "flags")),
            }

        def exact_owned_kernel_state():
            fdb = json.loads(run("bridge", "-j", "fdb", "show", "dev", vxlan,
                                 name=f"{label}-owned-fdb"))
            neighbors = json.loads(run("ip", "-j", "neigh", "show", "dev", bridge,
                                       name=f"{label}-owned-neighbors"))
            fdb_entries = [entry for entry in fdb if entry.get("mac") == owned_mac and
                           entry.get("dst") == owned_vtep]
            neighbor_entries = [entry for entry in neighbors if entry.get("dst") == owned_ip and
                                entry.get("lladdr") == owned_mac]
            require(len(fdb_entries) == len(neighbor_entries) == 1,
                    f"{side} exact owned forwarding entries not present after restart")

            def project(entry, fields):
                projected = {field: entry.get(field) for field in fields}
                if isinstance(projected.get("flags"), list):
                    projected["flags"] = sorted(projected["flags"])
                if isinstance(projected.get("state"), list):
                    projected["state"] = sorted(projected["state"])
                return projected

            return {
                "FDB": project(fdb_entries[0], ("mac", "dst", "flags", "state", "vlan", "vni")),
                "Neighbor": project(neighbor_entries[0], ("dst", "lladdr", "state", "flags")),
            }

        def state_from(journal, stage):
            record = journal["Record"]
            require(record.get("Pending") is None, f"{stage} importer journal has a pending intent")
            state = record["State"]
            require(state["Version"] == 2 and not state.get("Quarantine"),
                    f"{stage} importer journal is not v2 or has a global quarantine")
            return state

        def verify_reopened_state(journal, expected, prior_quarantines, stage):
            state = state_from(journal, stage)
            require(state["Controller"] == expected["Controller"] and
                    state["Target"] == expected["Target"] and
                    state["Networks"] == expected["Networks"] and
                    state["Devices"] == expected["Devices"] and
                    state["Receipts"] == expected["Receipts"],
                    f"{stage} did not reopen the same importer journal and exact owned receipts")
            quarantines = state.get("Quarantines")
            require(isinstance(quarantines, dict), f"{stage} lost the slot quarantine map")
            matching = [quarantine for quarantine in quarantines.values()
                        if quarantine.get("NetworkID") == ("ext1" if side == "a" else "ext2") and
                        quarantine.get("Device", {}).get("Name") == vxlan and
                        not quarantine.get("Resolved") and any(
                            observed.get("MAC") == foreign_mac and
                            observed.get("IP") == foreign_dst
                            for observed in quarantine.get("Observed", []))]
            require(matching, f"{stage} lacks durable evidence for its foreign FDB slot")
            if prior_quarantines is not None:
                require(quarantines == prior_quarantines,
                        f"{stage} changed or cleared persisted slot quarantine on restart")
            return state

        self.save_state(label + "-before")
        initial_journal = read(journal_path, label + "-import-journal-before")
        original_state = state_from(initial_journal, label + "-before")
        require(len(original_state["Receipts"]) == 1,
                f"{side} did not begin with exactly one durable imported endpoint")
        initial_foreign = exact_foreign_state()
        initial_owned = exact_owned_kernel_state()
        initial_export_journal = read(export_journal_path, label + "-export-journal-before")
        require(initial_export_journal["Record"].get("Pending") is None,
                f"{side} exporter journal has a pending intent before restart")
        initial_export_receipts = initial_export_journal["Record"]["State"]["Receipts"]
        initial_export_status = read("export-status.json", label + "-export-status-before")
        initial_applied = (initial_export_status.get("Result") or {}).get("Applied") or []
        require(not initial_export_status.get("Error") and len(initial_applied) == 1,
                f"{side} export was not healthy before Node-Agent restart")
        initial_uuid = initial_applied[0]["PathUUID"]

        agent_record, _ = self.process_record(side, "agent")
        speaker_record, _ = self.process_record(side, "speaker")
        agent_pid, speaker_pid = agent_record["pid"], speaker_record["pid"]
        agent_ticks = run("sh", "-ec", f"awk '{{print $22}}' /proc/{agent_pid}/stat").strip()
        speaker_ticks = run("sh", "-ec", f"awk '{{print $22}}' /proc/{speaker_pid}/stat").strip()
        require(agent_ticks and speaker_ticks, f"{side} process identity evidence missing")
        self.start_phase(label + "-agent-restart")
        quarantine_state = None
        iterations = []
        for index in (1, 2):
            iteration = f"{label}-restart-{index}"
            if side == "a":
                self.stop_process("agent")
                new_pid = self.launch("agent", agent_record["executable"], agent_record["arguments"])
            else:
                self.bstop("agent")
                new_pid = self.blaunch("agent", agent_record["executable"], agent_record["arguments"])
            new_ticks = run("sh", "-ec", f"awk '{{print $22}}' /proc/{new_pid}/stat").strip()
            require(new_pid != agent_pid or new_ticks != agent_ticks,
                    f"{iteration} did not create a new Node-Agent process incarnation")

            def recovered_status(status):
                return (not status.get("Error") and status.get("RecoveryState") == "RECOVERY_PARTIAL" and
                        status.get("Snapshot", {}).get("Authoritative") and
                        len(status.get("Receipts") or []) == 1 and
                        stable_objects(status["Receipts"]) == stable_objects(
                            list(original_state["Receipts"].values())))

            def recovered():
                return recovered_status(read("import-status.json", iteration + "-import-status"))

            self.wait(recovered, f"{iteration} did not recover its owned importer receipt")
            first_status = read("import-status.json", iteration + "-import-status-first")
            time.sleep(2)
            second_status = read("import-status.json", iteration + "-import-status-second")
            require(recovered_status(first_status) and recovered_status(second_status) and
                    first_status["Time"] != second_status["Time"] and
                    first_status["Snapshot"] == second_status["Snapshot"],
                    f"{iteration} lacks fresh stable authoritative BGP observations")
            journal = read(journal_path, iteration + "-import-journal")
            recovered_state = verify_reopened_state(journal, original_state, quarantine_state, iteration)
            quarantine_state = recovered_state["Quarantines"]
            require(exact_foreign_state() == initial_foreign,
                    f"{iteration} changed the foreign FDB or neighbor witness")
            require(exact_owned_kernel_state() == initial_owned,
                    f"{iteration} changed the exact owned FDB or neighbor tuple")

            export_journal = read(export_journal_path, iteration + "-export-journal")
            require(export_journal["Record"].get("Pending") is None and
                    export_journal["Record"]["State"]["Receipts"] == initial_export_receipts,
                    f"{iteration} changed durable local export ownership")
            export_status = read("export-status.json", iteration + "-export-status")
            applied = (export_status.get("Result") or {}).get("Applied") or []
            require(not export_status.get("Error") and len(applied) == 1 and
                    applied[0]["PathUUID"] == initial_uuid,
                    f"{iteration} changed the exact acknowledged local AddPath UUID")
            current_speaker = self.process_record(side, "speaker")[0]
            current_speaker_ticks = run(
                "sh", "-ec", f"awk '{{print $22}}' /proc/{current_speaker['pid']}/stat").strip()
            require(current_speaker["pid"] == speaker_pid and current_speaker_ticks == speaker_ticks,
                    f"{iteration} unexpectedly replaced the GoBGP speaker")

            if side == "a":
                validate_raw_rib(self.rib(iteration + "-rib"))
            else:
                validate_rib(self.b_rib(iteration + "-rib"), both=True)
            self.probes(iteration)
            self.save_state(iteration)
            iterations.append({"pid": new_pid, "startTicks": new_ticks,
                               "authoritativeSnapshot": True, "recoveryState": "RECOVERY_PARTIAL",
                               "receiptsPreserved": True, "foreignStatePreserved": True,
                               "exactOwnedStatePreserved": True, "speakerIncarnationPreserved": True})
            agent_pid, agent_ticks = new_pid, new_ticks
            agent_record, _ = self.process_record(side, "agent")
        self.stop_phase()
        self.restarts[side] = {"status": "PASS", "iterations": iterations,
                               "foreign": initial_foreign, "owned": initial_owned}
        self.scenarios[label] = {
            "status": "PASS",
            "detail": f"{side.upper()} Node-Agent restarted twice; same durable journal and exact receipts "
                      "reopened, foreign entries preserved, independent forwarding stayed live; exact "
                      "post-restart withdrawal is verified by the later route-withdrawal action",
            "evidence": f"{label}-restart-1-* / {label}-restart-2-*"}
        lab.write(self.directory / f"{label}-agent-restart-recovery.json", self.restarts[side])

    def remote_withdraw(self):
        super().remote_withdraw()
        if self.scenarios["G1"]["status"] == "PASS":
            self.scenarios["G1"]["detail"] += "; exact receipt-owned withdrawal after recovery passed"
            self.restarts["a"]["exactWithdrawalAfterRestart"] = True
            lab.write(self.directory / "G1-agent-restart-recovery.json", self.restarts["a"])

    def r_withdraw(self):
        super().r_withdraw()
        if self.scenarios["G2"]["status"] == "PASS":
            self.scenarios["G2"]["detail"] += "; exact receipt-owned withdrawal after recovery passed"
            self.restarts["b"]["exactWithdrawalAfterRestart"] = True
            lab.write(self.directory / "G2-agent-restart-recovery.json", self.restarts["b"])

    def restart_speaker(self, side):
        label = "speaker-" + side
        self.save_state(label + "-before")
        record, base = self.process_record(side, "speaker")
        old_uuid = self.owned_uuid if side == "a" else self.b_uuid
        self.start_phase(label)
        if side == "a":
            self.stop_process("speaker")
        else:
            self.bstop("speaker")
        read = (lambda path, name=None: self.read_json(base + "/" + path, name)) if side == "a" else self.bread
        self.wait(lambda: bool(read("export-status.json").get("Error")), "old speaker remained trusted")
        read("export-status.json", label + "-untrusted")
        self.probes(label + "-stopped", ("2", "g") if side == "a" else ("1", "g"))
        if side == "a":
            self.launch("speaker", record["executable"], record["arguments"])
        else:
            self.blaunch("speaker", record["executable"], record["arguments"])
        self.wait(lambda: not read("export-status.json").get("Error") and
                  len(read("export-status.json")["Result"]["Applied"]) == 1 and
                  read("export-status.json")["Result"]["Applied"][0]["PathUUID"] != old_uuid,
                  "new speaker failed authoritative-absence recovery")
        status = read("export-status.json", label + "-new-attestation")
        new, _ = self.process_record(side, "speaker")
        require(status["SpeakerProcess"]["PID"] == new["pid"] and
                status["SpeakerProcess"]["StartTicks"], "new incarnation not attested")
        if side == "a":
            self.owned_uuid = status["Result"]["Applied"][0]["PathUUID"]
        else:
            self.b_uuid = status["Result"]["Applied"][0]["PathUUID"]
        self.wait(lambda: LOCAL_MAC in self.vty("show bgp l2vpn evpn route json") if side == "a"
                  else B_MAC in json.dumps(self.b_rib(label + "-received", True)), "peer recovery failed")
        self.converge_pair(side, label + "-convergence")
        self.probes(label + "-recovered")
        self.stop_phase()
        self.save_state(label + "-after")
        self.restarts[label] = {"before": record, "after": new, "oldUUID": old_uuid,
                                "newUUID": self.owned_uuid if side == "a" else self.b_uuid}

    def no_transit(self):
        label = "E3"
        self.save_state(label + "-before")
        routers = json.loads(self.nb("--format=json", "list", "Logical_Router", name="E3-routers"))
        require(not routers["data"], "unexpected logical router in isolated fixture")
        require(R2_MAC not in json.dumps(self.rib("E3-a-routes")) and
                REMOTE_MAC not in json.dumps(self.b_rib("E3-b-routes")),
                "cross-domain BGP advertisement present")
        for side, foreign, device in (("a", R2_MAC, "evpnvx33"), ("b", REMOTE_MAC, "evpnvx34")):
            require(foreign not in self.rt(side, "bridge", "-j", "fdb", "show", "dev", device,
                                           name=f"E3-{side}-foreign-fdb"),
                    "cross-domain kernel FDB present")
            require(foreign not in self.rt(side, "ovs-ofctl", "dump-flows", "br-int",
                                           name=f"E3-{side}-flows"), "cross-domain OVS flow present")
        self.start_phase(label, access=True)
        self.probes(label + "-positive")
        for remote, namespace, device, own_vtep, destination, mac in (
            (self.frr, "evpnwork", "frr-work0", "198.18.10.2", R2_IP, R2_MAC),
            (self.remote2, "remote2-work", "r2-work", "198.18.20.2", REMOTE_IP, REMOTE_MAC),
        ):
            run = self.fr if remote == self.frr else self.r2
            side = "r1" if remote == self.frr else "r2"
            run("ping", "-n", "-c", "3", "-W", "3", own_vtep, name=f"E3-{side}-underlay-control")
            run("ip", "netns", "exec", namespace, "ip", "route", "add", destination + "/32",
                "dev", device)
            run("ip", "netns", "exec", namespace, "ip", "neigh", "add", destination, "lladdr",
                mac, "nud", "permanent", "dev", device)
            run("ip", "netns", "exec", namespace, "ip", "-j", "route", "get", destination,
                name=f"E3-{side}-cross-route")
            run("ip", "netns", "exec", namespace, "ip", "-j", "neigh", "show", "dev", device,
                name=f"E3-{side}-arp-control")
            name = f"E3-{side}-negative-ping"
            self.command(["docker", "exec", remote, "ip", "netns", "exec", namespace, "ping",
                          "-n", "-c", "3", "-W", "2", destination], name, check=False)
            result = json.loads((self.directory / (name + ".command.json")).read_text())
            require(result["exit"] == 1, "cross-domain ping succeeded or did not execute normally")
            run("ip", "netns", "exec", namespace, "ip", "neigh", "del", destination, "dev", device)
            run("ip", "netns", "exec", namespace, "ip", "route", "del", destination + "/32",
                "dev", device)
        self.probes(label + "-after")
        self.stop_phase()
        self.verify_cross_packets()
        self.save_state(label + "-after")
        self.scenarios["E3"] = {"status": "PASS", "detail": "positive-controlled cross-domain probes; "
                               "R1 cross request reaches A VNI1000 but never R2; R2 has no R1 FDB. "
                               "No router, BGP redistribution, cross FDB or OVS flow. Separate service switches only.",
                               "evidence": "E3-*"}

    def verify_cross_packets(self):
        spec = importlib.util.spec_from_file_location("pcap", lab.ROOT / "tools/lib/cni-vxlan-pcap-verify.py")
        decoder = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(decoder)
        counts = {}
        for surface in ("r1-access", "r2-access", "a", "r1", "b", "r2"):
            _, records = decoder.read_pcap(self.directory / f"E3-{surface}.pcap")
            matches = {"r1-r2": 0, "r2-r1": 0}
            for record in records:
                frame = record["frame"]
                if "access" in surface:
                    inner = decoder.inner_packet(frame, 0)
                else:
                    ether = decoder.ethernet_packet(frame)
                    if not ether or ether[2] != 0x0800:
                        continue
                    outer = decoder.ipv4_packet(frame, ether[3])
                    if not outer or outer["protocol"] != 17:
                        continue
                    inner = decoder.inner_packet(frame[:outer["end"]], outer["payloadOffset"] + 16)
                if not inner or "icmp" not in inner:
                    continue
                for direction, source, destination in (("r1-r2", REMOTE_IP, R2_IP),
                                                        ("r2-r1", R2_IP, REMOTE_IP)):
                    if inner.get("sourceIp") == source and inner.get("destinationIp") == destination:
                        matches[direction] += 1
            counts[surface] = matches
        require(counts["r1-access"]["r1-r2"] >= 3 and counts["r2-access"]["r2-r1"] >= 3,
                "cross-domain requests were not actually emitted")
        require(counts["a"]["r1-r2"] >= 3, "R1 negative probe never exercised A forwarding ingress")
        require(counts["b"]["r1-r2"] == counts["r2-access"]["r1-r2"] == 0 and
                counts["a"]["r2-r1"] == counts["r1-access"]["r2-r1"] == 0,
                "cross-domain packet escaped its domain")
        lab.write(self.directory / "E3-cross-packets.json", counts)

    def publish_policy(self, side, filename, value):
        base = BASE if side == "a" else B_BASE
        put = self.put if side == "a" else self.bput
        put(filename, value)
        self.rt(side, "sh", "-ec", f"cp {base}/{filename} {base}/policy.next && "
                f"chmod 600 {base}/policy.next && mv {base}/policy.next {base}/policy.json")

    def local_replacements(self):
        for side in ("a", "b"):
            label = "F3-" + side
            read = (lambda path, name=None: self.read_json(BASE + "/" + path, name)) if side == "a" else self.bread
            old = copy.deepcopy(self.endpoint if side == "a" else self.b_endpoint)
            old_uuid = self.owned_uuid if side == "a" else self.b_uuid
            self.save_state(label + "-before")
            self.start_phase(label)
            self.nb("lsp-del", old["logicalSwitchPortUUID"])
            self.wait(lambda: bool(read("export-status.json").get("Error")),
                      "missing original LSP did not freeze export")
            journal_path = "export-receipts/journal.json" if side == "a" else "export/journal.json"
            retained = read(journal_path, label + "-uncertain-owned")["Record"]["State"]["Receipts"]
            require(len(retained) == 1 and retained[0]["Owned"]["PathUUID"] == old_uuid,
                    "untrusted absence withdrew old UUID")
            read("export-status.json", label + "-uncertain")
            self.probes(label + "-uncertain", ("2", "g") if side == "a" else ("1", "g"))
            self.rt(side, "ovs-vsctl", "del-port", "br-int", old["logicalSwitchPort"] + "-host")
            switch = "ls-ext1" if side == "a" else "ls-ext2"
            self.nb("lsp-add", switch, old["logicalSwitchPort"])
            self.nb("lsp-set-addresses", old["logicalSwitchPort"], old["mac"] + " " + old["ip"])
            new = {**old, "attachmentID": str(uuid.uuid4()),
                   "attachmentGeneration": old["attachmentGeneration"] + 1,
                   "logicalSwitchPortUUID": self.nb("--bare", "--columns=_uuid", "find",
                                                   "Logical_Switch_Port",
                                                   "name=" + old["logicalSwitchPort"]).strip()}
            require(new["logicalSwitchPortUUID"] != old["logicalSwitchPortUUID"],
                    "replacement did not change LSP UUID")
            self.nb("set", "Logical_Switch_Port", new["logicalSwitchPortUUID"],
                    "external_ids:evpn.framework/network-id=" + new["networkID"],
                    "external_ids:evpn.framework/endpoint-id=" + new["endpointID"],
                    "external_ids:evpn.framework/attachment-id=" + new["attachmentID"],
                    "external_ids:evpn.framework/attachment-generation=" + str(new["attachmentGeneration"]))
            self.rt(side, "ovs-vsctl", "add-port", "br-int", new["logicalSwitchPort"] + "-host",
                    "--", "set", "Interface", new["logicalSwitchPort"] + "-host",
                    "external_ids:iface-id=" + new["logicalSwitchPort"])
            policy = read("policy.json")
            policy.update(revision=self.revisions[side] + 1, endpoints=[new],
                          tombstones=[{"reason": "replaced", "record": old}])
            self.publish_policy(side, label + "-replacement-policy.json", policy)
            self.revisions[side] = policy["revision"]
            self.wait(lambda: not read("export-status.json").get("Error") and
                      len(read("export-status.json")["Result"]["Applied"]) == 1 and
                      read("export-status.json")["Result"]["Applied"][0]["PathUUID"] != old_uuid,
                      "exact replacement failed")
            replacement_uuid = read("export-status.json")["Result"]["Applied"][0]["PathUUID"]
            if side == "a":
                self.endpoint, self.lsp_uuid, self.owned_uuid = new, new["logicalSwitchPortUUID"], replacement_uuid
            else:
                self.b_endpoint, self.b_lsp, self.b_uuid = new, new["logicalSwitchPortUUID"], replacement_uuid
            replay = {**policy, "revision": policy["revision"] + 1, "endpoints": [old], "tombstones": []}
            self.publish_policy(side, label + "-replay-policy.json", replay)
            self.wait(lambda: bool(read("export-status.json").get("Error")), "stale generation replay accepted")
            status = read("export-status.json", label + "-replay-rejected")
            retained = read(journal_path, label + "-replay-owned")["Record"]["State"]["Receipts"]
            require(len(retained) == 1 and retained[0]["Owned"]["PathUUID"] == replacement_uuid,
                    "replay changed replacement ownership")
            policy["revision"] += 2
            self.publish_policy(side, label + "-restored-policy.json", policy)
            self.revisions[side] = policy["revision"]
            self.wait(lambda: not read("export-status.json").get("Error"), "replacement policy not restored")
            self.save_state(label + "-replacement-ready")
            self.converge_pair(side, label + "-convergence")
            self.probes(label + "-replacement")
            self.stop_phase()
            self.save_state(label + "-after")
            lab.write(self.directory / (label + "-ownership.json"),
                      {"original": old, "replacement": new, "oldUUID": old_uuid,
                       "newUUID": replacement_uuid, "replayRejected": status})
        self.scenarios["F3"] = {"status": "PASS", "detail": "A/B LSP UUID replacement, exact replaced "
                               "tombstones, higher attachment generation, stale replay rejection and "
                               "replacement forwarding", "evidence": "F3-*"}

    def route_rejections(self):
        self.command(["docker", "cp", str(self.directory / "build/inject"),
                      f"{self.remote2}:/usr/local/bin/evpn-inject"])
        self.r2("chown", "root:root", "/usr/local/bin/evpn-inject")
        self.r2("chmod", "755", "/usr/local/bin/evpn-inject")
        self.r2("ip", "addr", "add", "198.18.20.98/24", "dev", "eth1")
        self.rt("b", "ping", "-n", "-c", "3", "-W", "3", "198.18.20.98",
                name="E2-reachable-unauthorized-vtep")
        for index, (label, vni, rt, vtep, reason) in enumerate((
            ("E2-b-vtep", 2000, "64513:2000", "198.18.20.98", "remote VTEP not allowlisted"),
            ("E1-b-rt", 2000, "64512:1000", "198.18.20.4", "no import RT intersection"),
            ("B-wrong-vni", 3000, "64513:2000", "198.18.20.4", "no configured VNI mapping"),
        )):
            self.save_state(label + "-before")
            self.start_phase(label)
            mac, ip = f"02:00:00:02:ee:{index + 1:02x}", f"10.100.2.{210 + index}"
            config = {"Socket": f"unix://{R_BASE}/gobgp.sock", "Attestation": f"{R_BASE}/r2-speaker.json",
                      "State": f"{R_BASE}/{label}-journal", "InstallationUID": self.installation,
                      "NodeUID": self.r_node, "ASN": 64513, "RouterID": "198.18.20.3",
                      "Endpoint": {"NetworkID": "negative", "EndpointID": str(uuid.uuid4()),
                                   "Incarnation": str(uuid.uuid4()), "MACAddress": mac, "IPAddresses": [ip],
                                   "VNI": vni, "RouteDistinguisher": f"198.18.20.3:{3100 + index}",
                                   "RouteTargets": [rt], "LocalVTEP": vtep, "Advertise": True}}
            self.bput(label + ".json", config, True)
            added = json.loads(self.r2("/usr/local/bin/evpn-inject", "-config", f"{R_BASE}/{label}.json",
                                      "-action", "advertise", name=label + "-add"))
            self.wait(lambda: mac in json.dumps(self.b_rib(label + "-raw")), "injected route not received")
            path = validate_injected_rib(self.b_rib(label + "-encoded"), config)
            lab.write(self.directory / (label + "-injection-encoding.json"), path)
            status = self.bwait("import-status.json", lambda s: rejected_issue(s, reason),
                               label + "-rejected")
            require(mac not in json.dumps(status["Receipts"]) and ip not in json.dumps(status["Receipts"]),
                    "unauthorized import receipt created")
            require(mac not in self.rt("b", "bridge", "-j", "fdb", "show", "dev", "evpnvx34") and
                    ip not in self.rt("b", "ip", "-j", "neigh", "show", "dev", "evpnbr34") and
                    mac not in self.rt("b", "ovs-ofctl", "dump-flows", "br-int"),
                    "unauthorized forwarding state programmed")
            if vtep != "198.18.20.4":
                require(vtep not in self.rt("b", "ovs-vsctl", "--format=json", "list", "Interface") and
                        vtep not in self.rt("b", "ovs-ofctl", "dump-flows", "br-int"),
                        "unauthorized VTEP programmed in OVS")
            self.rread(f"{label}-journal/journal.json", label + "-injection-receipt")
            self.save_state(label + "-rejected-state")
            self.probes(label + "-rejected")
            deleted = json.loads(self.r2("/usr/local/bin/evpn-inject", "-config", f"{R_BASE}/{label}.json",
                                        "-action", "withdraw", name=label + "-withdraw"))
            require(deleted["PathUUID"] == added["PathUUID"] and
                    deleted["Incarnation"] == added["Incarnation"], "injection cleanup changed owner/UUID")
            self.wait(lambda: mac not in json.dumps(self.b_rib(label + "-absent")),
                      "injected route remained")
            self.bwait("import-status.json", lambda s: s["Snapshot"]["Authoritative"] and
                       not s["Snapshot"]["Issues"], label + "-restored")
            self.probes(label + "-restored")
            self.stop_phase()
            self.save_state(label + "-after")
        self.r2("ip", "addr", "del", "198.18.20.98/24", "dev", "eth1")
        self.scenarios["E1"] = {"status": "BLOCKED", "detail": "B wrong-RT import rejection is supporting "
                               "evidence only. Full E1 still requires pinned peer policies rejecting "
                               "wrong/dual RT sets and injection through FRR in the opposite direction.",
                               "evidence": "E1-b-rt-*"}
        self.scenarios["E2"] = {"status": "BLOCKED", "detail": "B rejects a real matching-RT/VNI route "
                               "with reachable unauthorized VTEP; opposite-direction controlled FRR "
                               "MP_REACH injection is not implemented", "evidence": "E2-b-vtep-*"}

    def local_reader(self, side):
        return (lambda path, name=None: self.read_json(BASE + "/" + path, name)) if side == "a" else self.bread

    def activate_next_generation(self, side, label, old, recreate=False):
        switch, port = ("ls-ext1", "ext-a") if side == "a" else ("ls-ext2", "ext-b")
        if recreate:
            self.nb("lsp-add", switch, port)
            self.nb("lsp-set-addresses", port, old["mac"] + " " + old["ip"])
            self.rt(side, "sh", "-ec",
                    f"ip netns add {port}\nip link add {port}-host type veth peer name {port}-eth\n"
                    f"ip link set {port}-eth netns {port}\nip link set {port}-host up\n"
                    f"ip netns exec {port} ip link set {port}-eth address {old['mac']} mtu 1400\n"
                    f"ip netns exec {port} ip addr add {old['ip']}/24 dev {port}-eth\n"
                    f"ip netns exec {port} ip link set lo up\n"
                    f"ip netns exec {port} ip link set {port}-eth up\n"
                    f"ovs-vsctl add-port br-int {port}-host -- set Interface {port}-host external_ids:iface-id={port}")
            self.rt(side, "ip", "netns", "exec", port, "ip", "neigh", "add",
                    REMOTE_IP if side == "a" else R2_IP, "lladdr",
                    REMOTE_MAC if side == "a" else R2_MAC, "nud", "permanent", "dev", port + "-eth")
        new = {**old, "attachmentID": str(uuid.uuid4()),
               "attachmentGeneration": old["attachmentGeneration"] + 1,
               "logicalSwitchPortUUID": self.nb("--bare", "--columns=_uuid", "find",
                                               "Logical_Switch_Port", "name=" + port).strip()}
        self.nb("set", "Logical_Switch_Port", new["logicalSwitchPortUUID"],
                "external_ids:evpn.framework/network-id=" + new["networkID"],
                "external_ids:evpn.framework/endpoint-id=" + new["endpointID"],
                "external_ids:evpn.framework/attachment-id=" + new["attachmentID"],
                "external_ids:evpn.framework/attachment-generation=" + str(new["attachmentGeneration"]))
        read = self.local_reader(side)
        policy = read("policy.json")
        policy.update(revision=self.revisions[side] + 1, endpoints=[new], tombstones=[])
        self.publish_policy(side, label + "-policy.json", policy)
        self.revisions[side] = policy["revision"]
        self.wait(lambda: not read("export-status.json").get("Error") and
                  len(read("export-status.json")["Result"]["Applied"]) == 1, label + " failed")
        owned = read("export-status.json", label + "-status")["Result"]["Applied"][0]["PathUUID"]
        if side == "a":
            self.endpoint, self.lsp_uuid, self.owned_uuid = new, new["logicalSwitchPortUUID"], owned
        else:
            self.b_endpoint, self.b_lsp, self.b_uuid = new, new["logicalSwitchPortUUID"], owned

    def disable_restore(self):
        old = {"a": copy.deepcopy(self.endpoint), "b": copy.deepcopy(self.b_endpoint)}
        old_uuids = {"a": self.owned_uuid, "b": self.b_uuid, "r2": self.r_uuid}
        files = ("gobgpd.yaml", "b-gobgpd.yaml", "r2-gobgpd.yaml",
                 "export.json", "b-export.json", "import.json", "b-import.json", "remote2.json")
        before = {name: hashlib.sha256((self.directory / name).read_bytes()).hexdigest() for name in files}
        self.save_state("A2-before")
        self.start_phase("a2-drain")
        for side in ("a", "b"):
            port = old[side]["logicalSwitchPort"]
            self.nb("lsp-del", old[side]["logicalSwitchPortUUID"])
            self.rt(side, "ovs-vsctl", "del-port", "br-int", port + "-host")
            self.rt(side, "ip", "link", "del", port + "-host")
            self.rt(side, "ip", "netns", "del", port)
            self.wait(lambda p=port: not self.sb("--bare", "--columns=_uuid", "find",
                      "Port_Binding", "logical_port=" + p).strip(),
                      "A2 retired binding remains")
            policy = self.local_reader(side)("policy.json")
            policy.update(revision=self.revisions[side] + 1, endpoints=[],
                          tombstones=[{"reason": "deleted", "record": old[side]}])
            self.publish_policy(side, "A2-" + side + "-disabled-policy.json", policy)
            self.revisions[side] = policy["revision"]
            self.wait(lambda s=side: not self.local_reader(s)("export-status.json").get("Error") and
                      not self.local_reader(s)("export-status.json")["Result"]["Applied"],
                      "A2 local exact drain failed")
        ep = self.rconfig["Endpoint"]
        self.bput("retire", ep["EndpointID"] + "/" + self.r_incarnation + "\n", True)
        self.bwait("status.json", lambda s: not s["Error"] and not s["Export"]["Applied"],
                   "A2-r2-drained", True)
        self.fr("ip", "netns", "exec", "evpnwork", "ip", "link", "set", "frr-work0", "down")
        self.wait(lambda: not self.rib("A2-a-empty") and not self.b_rib("A2-b-empty") and
                  not self.b_rib("A2-r2-empty", True), "A2 Type-2 routes did not drain")
        for side in ("a", "b"):
            read = self.local_reader(side)
            self.wait(lambda r=read: not r("import-status.json").get("Error") and
                      r("import-status.json")["Snapshot"]["Authoritative"] and
                      not r("import-status.json")["Receipts"], "A2 local import drain failed")
        self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"] and
                   not s["Receipts"], "A2-projector-drained", True)
        for side, base, peer in (("a", BASE, "198.18.10.3"), ("b", B_BASE, "198.18.20.3")):
            self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock", "neighbor", peer, "disable",
                    name="A2-" + side + "-disable-peer")
            self.wait(lambda s=side, b=base, p=peer: "ESTABLISHED" not in self.rt(
                s, "gobgp", "--target", f"unix://{b}/gobgp.sock", "neighbor", p).upper(),
                "A2 peer remained established")
        self.probes("A2-drained-geneve", ("g",))
        self.stop_phase(("g",))
        self.start_phase("a2-disabled")
        self.probes("A2-disabled-geneve", ("g",))
        self.stop_phase(("g",))
        self.save_state("A2-disabled")
        self.start_phase("a2-restored")
        for side, base, peer in (("a", BASE, "198.18.10.3"), ("b", B_BASE, "198.18.20.3")):
            self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock", "neighbor", peer, "enable",
                    name="A2-" + side + "-restore-peer")
            self.wait(lambda s=side, b=base, p=peer: "ESTABLISHED" in self.rt(
                s, "gobgp", "--target", f"unix://{b}/gobgp.sock", "neighbor", p).upper(),
                "A2 peer not restored")
        self.fr("ip", "netns", "exec", "evpnwork", "ip", "link", "set", "frr-work0", "up")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "2", "-W", "3", "10.100.1.1",
                name="A2-r1-ready-warmup")
        self.r2("rm", f"{R_BASE}/retire")
        self.recreate_r2_generation()
        status = self.bwait("status.json", lambda s: not s["Error"] and
                           len(s["Export"]["Applied"]) == 1, "A2-r2-restored", True)
        self.r_uuid = status["Export"]["Applied"][0]["PathUUID"]
        for side in ("a", "b"):
            self.activate_next_generation(side, "A2-" + side + "-restored", old[side], recreate=True)
        for side in ("a", "b"):
            read = self.local_reader(side)
            self.wait(lambda r=read: not r("import-status.json").get("Error") and
                      r("import-status.json")["Snapshot"]["Authoritative"] and
                      len(r("import-status.json").get("Receipts") or []) == 1,
                      "A2 imports not restored")
        self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"] and
                   len(s.get("Receipts") or []) == 1, "A2-projector-restored", True)
        for side in ("a", "b"):
            self.converge_pair(side, "A2-" + side + "-convergence")
        self.probes("A2-restored")
        self.stop_phase()
        after = {name: hashlib.sha256((self.directory / name).read_bytes()).hexdigest() for name in files}
        require(before == after, "A2 changed pinned pair configurations")
        require(self.owned_uuid != old_uuids["a"] and self.b_uuid != old_uuids["b"] and
                self.r_uuid != old_uuids["r2"], "A2 reused a withdrawn UUID")
        self.save_state("A2-restored")
        lab.write(self.directory / "A2-exact-config-restoration.json",
                  {"before": before, "after": after, "withdrawnUUIDs": old_uuids,
                   "note": "authorization high-water preserved; valid next generations, never policy rollback"})
        self.scenarios["A2"] = {"status": "PASS", "detail": "both external pairs and all framework Type-2 "
                               "routes drained, sessions disabled; Geneve forwards; exact pinned pair "
                               "config restored with retained fences and fresh acknowledged UUIDs",
                               "evidence": "A2-* / a2-*"}

    def retire_a(self):
        old = copy.deepcopy(self.endpoint)
        self.save_state("F1-a-before")
        self.start_phase("f1-a")
        super().uncertainty()
        self.probes("F1-a-uncertain", ("2", "g"))
        super().local_withdraw()
        self.probes("F1-a-retired", ("2", "g"))
        self.stop_phase(("2", "g"))
        self.save_state("F1-a-retired")
        self.a_retirement_verified = True
        self.start_phase("f1-a-recreated")
        self.activate_next_generation("a", "F1-a-recreated", old, recreate=True)
        self.wait(lambda: LOCAL_MAC in self.vty("show bgp l2vpn evpn route json"),
                  "F1 A recreation not observed by FRR")
        self.converge_pair("a", "F1-a-recreated-convergence")
        self.probes("F1-a-recreated")
        self.stop_phase()
        self.save_state("F1-a-recreated")

    def b_withdraw(self):
        self.start_phase("f1-b")
        super().b_withdraw()
        self.probes("F1-b-retired", ("1", "g"))
        self.stop_phase(("1", "g"))
        self.save_state("F1-b-retired")
        require(self.a_retirement_verified, "F1 A retirement not exercised")
        self.scenarios["F1"] = {"status": "PASS", "detail": "A/B actual local retirements, incomplete "
                               "observation retention, exact higher-revision tombstones and UUID withdrawal; "
                               "other pair and Geneve forward during each retirement", "evidence": "F1-* / f1-*"}

    def finish(self, status):
        if status == "PASS":
            self.scenarios["F2"] = {"status": "BLOCKED", "detail": "supporting external withdrawals "
                                   "do not exercise both surviving local exports and safe external "
                                   "identity recreation; Remote2 composition lifetime remains unresolved"}
            self.scenarios["F4"] = {"status": "BLOCKED", "detail": "foreign preservation assertions "
                                   "do not certify complete F2 recreation coverage"}
            self.scenarios["H1"] = {"status": "BLOCKED", "detail": "normal exact fixture teardown "
                                   "does not establish pre-destruction route drainage after quarantined "
                                   "agent recovery; production recovery approval is required"}
        if status != "PASS":
            for scenario in self.scenarios.values():
                if scenario["status"] == "PASS":
                    scenario["status"] = "BLOCKED"
                    scenario["detail"] += "; supporting evidence only: attempt did not complete C0"
        lab.write(self.directory / "recovery-scenarios.json",
                  {"implementedScope": status, "scenarios": self.scenarios, "restarts": self.restarts,
                   "fullMilestoneAccepted": False})
