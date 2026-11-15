"""Three-domain extension using the existing fixture and owned core primitives."""

import hashlib
import json
from pathlib import Path
import shlex
import time
import uuid

import containerlab as lab
from external_frr import FRRBaseline, BASE, LOCAL_IP, LOCAL_MAC, REMOTE_IP, decode_vxlan
from geneve_baseline import Baseline, require


B_MAC = "02:00:00:02:00:0b"
R2_MAC = "02:00:00:02:00:66"
R2_BRIDGE_MAC = "02:00:00:02:fe:01"
B_IP = "10.100.2.10"
R2_IP = "10.100.2.101"
B_BASE = "/run/evpn-external-b"
R_BASE = "/run/evpn-remote2"
CASES = ("domain2_fixture", "domain2_attested_runtime", "D1_export", "D2_import",
         "D3_three_domain_packets", "independent_owners", "projector_uncertainty_conflict", "B_uncertain_retention",
         "B_exact_withdrawal", "R2_exact_withdrawal", "domain2_foreign_preservation",
         "domain2_cleanup")


def validate_rib(rib, remote=False, both=True):
    found = set()
    for paths in rib.values():
        for path in paths:
            nlri = path["nlri"]
            require(nlri["type"] == 2, "Domain2 received non-Type2")
            route = nlri["value"]
            is_b = route["mac"] == B_MAC
            mac, ip, admin, vtep = ((B_MAC, B_IP, "198.18.20.2", "198.18.20.2") if is_b else
                                    (R2_MAC, R2_IP, "198.18.20.3", "198.18.20.4"))
            require(route["mac"] == mac and route["ip"] == ip and
                    route["rd"] == {"type": 1, "admin": admin, "assigned": 2000} and
                    route["labels"] == [2000] and route["etag"] == 0 and
                    route["esi"] == "single-homed", "Domain2 endpoint NLRI differs")
            attrs = {a["type"]: a for a in path["attrs"]}
            require(attrs[14]["nexthop"] == vtep and attrs[14]["afi"] == 25 and
                    attrs[14]["safi"] == 70 and
                    any(c.get("value") == "64513:2000" for c in attrs[16]["value"]) and
                    any(c.get("tunnel_type") == 8 for c in attrs[16]["value"]),
                    "Domain2 MP_REACH/RT/encapsulation differs")
            if (remote and is_b) or (not remote and not is_b):
                require(path.get("peer-address") == admin, "received path provenance differs")
            found.add(mac)
    require(({B_MAC, R2_MAC} if both else {B_MAC if remote else R2_MAC}) <= found,
            "required actual Domain2 route absent")


def packets(directory):
    return {name: decode_vxlan(directory / f"{name}.pcap", 2000, "198.18.20.2",
                              "198.18.20.4", B_MAC, R2_MAC, B_IP, R2_IP)
            for name in ("b-underlay", "remote2-underlay")}


class ThreeDomainBaseline(FRRBaseline):
    def __init__(self, directory, cluster):
        super().__init__(directory, cluster)
        names = list(self.cases)
        position = names.index("non_owner_b")
        names[position:position] = CASES
        self.cases = {n: {"status": "BLOCKED", "detail": "not reached"} for n in names}
        self.remote2 = cluster + "-remote2"
        self.b_processes = {}
        self.r_processes = {}

    def provision(self):
        Baseline.provision(self, "geneve-full", self.frr)

    def inputs(self):
        super().inputs()
        self.command(["go", "build", "-o", str(self.directory / "build/remote2"),
                      "./tools/e2e/remote2"], "build-remote2")
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/external_gobgp.py", "tools/e2e/remote2/main.go",
                         "tools/e2e/remote2/main_test.go",
                         "tools/e2e/remote2/lifecycle.go", "tools/e2e/remote2/lifecycle_test.go",
                         "tools/e2e/remote2/transport.go", "tools/e2e/remote2/transport_test.go",
                         "tools/e2e/remote2/peer_test.go", "tools/e2e/remote2/README.md",
                         "tools/e2e/test_external_gobgp.py"):
            target = self.directory / "source" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((lab.ROOT / relative).read_bytes())
            pins[relative] = hashlib.sha256(target.read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)

    def external_actions(self):
        actions = list(super().external_actions())
        actions[6:6] = [self.b_fixture, self.b_runtime, self.b_export, self.b_import,
                       self.three_packets, self.owners, self.projector_uncertainty, self.b_uncertainty,
                       self.b_withdraw, self.r_withdraw, self.b_foreign, self.b_cleanup]
        return tuple(actions)

    def r2(self, *args, name=None):
        return self.command(["docker", "exec", self.remote2, *args], name)

    def bput(self, filename, value, remote=False):
        evidence_name = f"b-{filename}" if filename in ("export.json", "import.json", "runtime.json") else filename
        source = self.directory / evidence_name
        if isinstance(value, dict):
            lab.write(source, value)
        else:
            source.write_text(value)
        source.chmod(0o600)
        if remote:
            self.command(["docker", "cp", str(source), f"{self.remote2}:{R_BASE}/{filename}"])
            self.r2("chown", "root:root", f"{R_BASE}/{filename}")
            self.r2("chmod", "600", f"{R_BASE}/{filename}")
        else:
            self.k("-n", "geneve-test", "cp", str(source), f"{self.pods['b']}:{B_BASE}/{filename}")
            self.rt("b", "chown", "root:root", f"{B_BASE}/{filename}")
            self.rt("b", "chmod", "600", f"{B_BASE}/{filename}")

    def bread(self, path, name=None):
        return json.loads(self.rt("b", "cat", f"{B_BASE}/{path}", name=name))

    def rread(self, path, name=None):
        return json.loads(self.r2("cat", f"{R_BASE}/{path}", name=name))

    def recreate_r2_generation(self):
        state = self.rread("status.json")["Lifecycle"]
        require(state["Phase"] == "retired" and state["Generation"] == self.r_generation and
                state["Incarnation"] == self.r_incarnation,
                "Remote2 recreation requires acknowledged current-generation retirement")
        generation, incarnation = self.r_generation + 1, str(uuid.uuid4())
        self.bput("endpoint.json", {"Generation": generation, "Incarnation": incarnation,
                                   "Retired": False}, True)
        self.r_generation, self.r_incarnation = generation, incarnation

    def bwait(self, path, predicate, label, remote=False):
        read = self.rread if remote else self.bread
        run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
        base = R_BASE if remote else B_BASE
        self.wait(lambda: "READY" in run("sh", "-ec",
                  f"if test -s {base}/{path}; then echo READY; else echo STARTING; fi"),
                  label+" status unavailable")
        self.wait(lambda: predicate(read(path, label+"-poll")), label+" did not converge")
        return read(path, label)

    def b_fixture(self):
        self.rt("b", "sh", "-ec",
                "ip link add evpnbr34 type bridge; ip link set evpnbr34 up\n"
                "ip link add evpnvx34 type vxlan id 2000 local 198.18.20.2 dstport 60100 nolearning\n"
                "ip link set evpnvx34 master evpnbr34; ip link set evpnvx34 up\n"
                "ip link add evpnad34 type dummy; ip link set evpnad34 master evpnbr34\n"
                "ip link set evpnad34 up\n"
                "ovs-vsctl set Open_vSwitch . external_ids:ovn-evpn-local-ip=198.18.20.2 "
                "external_ids:ovn-evpn-vxlan-ports=4789")
        self.nb("ls-add", "ls-ext2")
        self.b_switch = self.nb("--bare", "--columns=_uuid", "find",
                                "Logical_Switch", "name=ls-ext2").strip()
        self.nb("set", "Logical_Switch", self.b_switch,
                f"external_ids:evpn.e2e.owner={self.directory.name}",
                "other_config:dynamic-routing-vni=2000",
                "other_config:dynamic-routing-bridge-ifname=evpnbr34",
                "other_config:dynamic-routing-vxlan-ifname=evpnvx34",
                "other_config:dynamic-routing-advertise-ifname=evpnad34",
                "other_config:dynamic-routing-redistribute=fdb,ip")
        self.nb("lsp-add", "ls-ext2", "ext-b")
        self.nb("lsp-set-addresses", "ext-b", f"{B_MAC} {B_IP}")
        self.b_lsp = self.nb("--bare", "--columns=_uuid", "find",
                             "Logical_Switch_Port", "name=ext-b").strip()
        self.b_endpoint = {"networkID": "ext2", "endpointID": str(uuid.uuid4()),
                           "attachmentID": str(uuid.uuid4()), "attachmentGeneration": 1,
                           "logicalSwitchPort": "ext-b", "logicalSwitchPortUUID": self.b_lsp,
                           "mac": B_MAC, "ip": B_IP}
        self.nb("set", "Logical_Switch_Port", self.b_lsp,
                "external_ids:evpn.framework/network-id=ext2",
                f"external_ids:evpn.framework/endpoint-id={self.b_endpoint['endpointID']}",
                f"external_ids:evpn.framework/attachment-id={self.b_endpoint['attachmentID']}",
                "external_ids:evpn.framework/attachment-generation=1")
        self.rt("b", "sh", "-ec",
                "ip netns add ext-b; ip link add ext-b-host type veth peer name ext-b-eth\n"
                "ip link set ext-b-eth netns ext-b; ip link set ext-b-host up\n"
                f"ip netns exec ext-b ip link set ext-b-eth address {B_MAC} mtu 1400\n"
                f"ip netns exec ext-b ip addr add {B_IP}/24 dev ext-b-eth\n"
                "ip netns exec ext-b ip link set lo up; ip netns exec ext-b ip link set ext-b-eth up\n"
                "ovs-vsctl add-port br-int ext-b-host -- set Interface ext-b-host external_ids:iface-id=ext-b")
        self.wait(lambda: self.sb("--bare", "--columns=_uuid", "find", "Port_Binding",
                                  "logical_port=ext-b", f"chassis={self.identities['b']['chassisUUID']}",
                                  "up=true").strip(), "B endpoint not ready on B chassis")
        require(self.b_lsp in self.nb("get", "Logical_Switch", self.b_switch, "ports"),
                "B LSP not in exact switch")
        self.nb("--format=json", "list", "Logical_Switch_Port", self.b_lsp, name="b-lsp")
        self.sb("--format=json", "list", "Port_Binding", name="b-bindings")
        self.r2("sh", "-ec",
                "ip addr add 198.18.20.4/24 dev eth1\n"
                "ip route add prohibit 198.18.30.0/24\n"
                f"ip link add br2000 address {R2_BRIDGE_MAC} type bridge; ip link set br2000 up\n"
                "ip link add vxlan2000 type vxlan id 2000 local 198.18.20.4 dstport 4789 nolearning\n"
                "ip link set vxlan2000 master br2000; ip link set vxlan2000 up\n"
                "bridge link set dev vxlan2000 learning off\n"
                "ip netns add remote2-work; ip link add r2-host type veth peer name r2-work\n"
                "ip link set r2-host master br2000; ip link set r2-host up\n"
                "ip link set r2-work netns remote2-work\n"
                f"ip netns exec remote2-work ip link set r2-work address {R2_MAC} mtu 1400\n"
                f"ip netns exec remote2-work ip addr add {R2_IP}/24 dev r2-work\n"
                "ip netns exec remote2-work ip link set lo up\n"
                "ip netns exec remote2-work ip link set r2-work up")
        self.r2("ip", "-d", "-j", "link", "show", name="r2-fixture-links")
        self.r2("bridge", "-d", "-j", "link", "show", "dev", "vxlan2000",
                name="r2-exclusive-projector-port")
        require(B_MAC not in self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000"),
                "Remote2 tested FDB preseeded")
        lab.write(self.directory / "domain2-static-state.json",
                  {"workloadNeighbors": {"B": {R2_IP: R2_MAC}, "Remote2": {B_IP: B_MAC}},
                   "floodEntries": [], "testedFDBPreseeded": False})

    def blaunch(self, name, executable, args, remote=False):
        run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
        base = R_BASE if remote else B_BASE
        pid = int(run("sh", "-ec", f'{executable} "$@" >{base}/{name}.log 2>&1 </dev/null & echo $!',
                      name, *args).strip())
        run("kill", "-0", str(pid))
        ticks = run("sh", "-ec", f"awk '{{print $22}}' /proc/{pid}/stat").strip()
        registry = self.r_processes if remote else self.b_processes
        registry[name] = (pid, ticks, executable)
        lab.write(self.directory / f"{'r2' if remote else 'b'}-{name}-process.json",
                  {"pid": pid, "startTicks": ticks, "executable": executable, "arguments": args})
        return pid

    def attestation(self, pid, config_file, args, remote=False):
        run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
        base = R_BASE if remote else B_BASE
        cgroup = run("cat", f"/proc/{pid}/cgroup")
        cgroup = next(line[3:] for line in cgroup.splitlines() if line.startswith("0::"))
        require(cgroup != "/", "speaker cgroup unpinned")
        node = self.r_node if remote else self.identities["b"]["nodeUID"]
        service = self.r_service if remote else self.identities["b"]["podUID"]
        router = "198.18.20.3" if remote else "198.18.20.2"
        value = {"mode": "standalone", "standalone": {
            "installationUID": self.installation, "nodeUID": node, "serviceUID": service,
            "expectedUID": 0, "cgroupPath": cgroup, "executablePath": "/usr/local/bin/gobgpd",
            "executableSHA256": hashlib.sha256((self.directory / "build/gobgpd").read_bytes()).hexdigest(),
            "arguments": ["/usr/local/bin/gobgpd", *args],
            "goBGPConfigPath": f"{base}/{config_file}",
            "goBGPConfigSHA256": hashlib.sha256((self.directory / config_file).read_bytes()).hexdigest(),
            "asn": 64513, "routerID": router}}
        self.bput("r2-speaker.json" if remote else "b-speaker.json", value, remote)

    def b_policy(self, revision, active, reason=None):
        value = {"version": 2, "revision": revision, "installationUID": self.installation,
                 "nodeUID": self.identities["b"]["nodeUID"],
                 "localChassis": self.identities["b"]["systemId"],
                 "endpoints": [self.b_endpoint] if active else [],
                 "tombstones": [] if not reason else [{"reason": reason, "record": self.b_endpoint}]}
        self.bput(f"b-policy-{revision}.json", value)
        self.rt("b", "sh", "-ec", f"cp {B_BASE}/b-policy-{revision}.json {B_BASE}/policy.next && "
                f"mv {B_BASE}/policy.next {B_BASE}/policy.json")

    def b_runtime(self):
        self.rt("b", "install", "-d", "-m", "700", B_BASE)
        self.r2("install", "-d", "-m", "700", R_BASE)
        self.r_node, self.r_service = str(uuid.uuid4()), str(uuid.uuid4())
        for name in ("gobgp", "gobgpd", "evpn-node-agent"):
            self.k("-n", "geneve-test", "cp", str(self.directory / "build" / name),
                   f"{self.pods['b']}:/usr/local/bin/{name}")
            self.rt("b", "chmod", "755", f"/usr/local/bin/{name}")
        for name in ("gobgp", "gobgpd", "remote2"):
            self.command(["docker", "cp", str(self.directory / "build" / name),
                          f"{self.remote2}:/usr/local/bin/{name}"])
            self.r2("chown", "root:root", f"/usr/local/bin/{name}")
            self.r2("chmod", "755", f"/usr/local/bin/{name}")
        for remote in (False, True):
            address, peer = ("198.18.20.3", "198.18.20.2") if remote else ("198.18.20.2", "198.18.20.3")
            filename = "r2-gobgpd.yaml" if remote else "b-gobgpd.yaml"
            base = R_BASE if remote else B_BASE
            self.bput(filename, f"""global:
  config:
    as: 64513
    router-id: "{address}"
    port: 179
    local-address-list: ["{address}"]
neighbors:
- config:
    neighbor-address: "{peer}"
    peer-as: 64513
  afi-safis:
  - config:
      afi-safi-name: l2vpn-evpn
""", remote)
            args = ["-f", f"{base}/{filename}", "--api-hosts", f"unix://{base}/gobgp.sock"]
            pid = self.blaunch("speaker", "/usr/local/bin/gobgpd", args, remote)
            self.attestation(pid, filename, args, remote)
        self.wait(lambda: "ESTABLISHED" in self.rt("b", "gobgp", "--target",
                  f"unix://{B_BASE}/gobgp.sock", "neighbor", "198.18.20.3",
                  name="b-peer-poll").upper(), "Domain2 BGP not established")
        recovery = {"InstallationUID": self.installation, "NodeUID": self.identities["b"]["nodeUID"]}
        common = {"Enabled": True, "GRPCEndpoint": f"unix://{B_BASE}/gobgp.sock",
                  "AttestationConfigPath": f"{B_BASE}/b-speaker.json", "ASN": 64513,
                  "RouterID": "198.18.20.2"}
        export = {**common, "DryRun": False, "ProcessContinuity": True,
                  "NodeName": self.identities["b"]["nodeName"], "LocalChassis": self.identities["b"]["systemId"],
                  "Recovery": {"StateDirectory": f"{B_BASE}/export", "Domain": recovery},
                  "Networks": [{"NetworkID": "ext2", "LogicalSwitch": "ls-ext2", "Enabled": True,
                                "ExporterSource": "framework", "VNI": 2000,
                                "RouteDistinguisher": "198.18.20.2:2000",
                                "RouteTargets": ["64513:2000"], "LocalVTEP": "198.18.20.2"}]}
        remote = {**common, "Import": {"LocalVTEP": "198.18.20.2", "Networks": [{
            "NetworkID": "ext2", "VNI": 2000, "ImportRouteTargets": ["64513:2000"],
            "AllowedVTEPs": ["198.18.20.4"]}]},
            "Netlink": {"Enabled": True, "ExclusiveWriter": True, "StateDirectory": f"{B_BASE}/import",
                        "Recovery": {"Domain": recovery}, "Networks": [{
                            "NetworkID": "ext2", "LogicalSwitch": "ls-ext2", "VNI": 2000,
                            "BridgeDevice": "evpnbr34", "VXLANDevice": "evpnvx34"}]}}
        runtime = {"enabled": True, "enableRemoteImport": True, "installationUID": self.installation,
                   "nodeUID": self.identities["b"]["nodeUID"], "localChassis": self.identities["b"]["systemId"],
                   "exporterConfigPath": f"{B_BASE}/export.json", "authorizationPolicyPath": f"{B_BASE}/policy.json",
                   "authorizationFenceDirectory": f"{B_BASE}/fence"}
        self.b_policy(1, True)
        for name, value in (("export.json", export), ("import.json", remote), ("runtime.json", runtime)):
            self.bput(name, value)
        self.rt("b", "install", "-d", "-m", "700", f"{B_BASE}/import")
        rconfig = {"Socket": f"unix://{R_BASE}/gobgp.sock", "Attestation": f"{R_BASE}/r2-speaker.json",
                   "State": R_BASE, "EndpointNamespace": "remote2-work", "EndpointDevice": "r2-work",
                   "InstallationUID": self.installation, "NodeUID": self.r_node,
                   "Endpoint": {"NetworkID": "ext2", "EndpointID": str(uuid.uuid4()),
                                "Incarnation": str(uuid.uuid4()), "MACAddress": R2_MAC, "IPAddresses": [R2_IP],
                                "VNI": 2000, "RouteDistinguisher": "198.18.20.3:2000",
                                "RouteTargets": ["64513:2000"], "LocalVTEP": "198.18.20.4", "Advertise": True},
                   "Import": {"LocalVTEP": "198.18.20.4", "Networks": [{
                       "NetworkID": "ext2", "VNI": 2000, "ImportRouteTargets": ["64513:2000"],
                       "AllowedVTEPs": ["198.18.20.2"]}]},
                   "Netlink": {"NetworkID": "ext2", "LogicalSwitch": "linux-bridge:br2000", "VNI": 2000,
                               "BridgeDevice": "br2000", "VXLANDevice": "vxlan2000"}}
        self.rconfig = rconfig
        self.r_generation = 1
        self.r_incarnation = rconfig["Endpoint"]["Incarnation"]
        self.bput("remote2.json", rconfig, True)
        self.blaunch("projector", "/usr/local/bin/remote2", ["-config", f"{R_BASE}/remote2.json"], True)
        self.wait(lambda: R2_MAC in self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
                  "global", "rib", "-a", "evpn", "-j"), "Remote2 route origination absent")
        first = self.b_rib("b-before-1")
        time.sleep(2)
        second = self.b_rib("b-before-2")
        validate_rib(first, both=False)
        validate_rib(second, both=False)
        require([(p["nlri"], p["attrs"]) for paths in first.values() for p in paths] ==
                [(p["nlri"], p["attrs"]) for paths in second.values() for p in paths],
                "two complete B RIB reads differ")
        require(R2_MAC not in self.rt("b", "bridge", "-j", "fdb", "show", "dev", "evpnvx34"),
                "B tested path preseeded")
        self.blaunch("agent", "/usr/local/bin/evpn-node-agent", [
            "--mode=real-ovn-ovs", "--network-backend=linux-veth",
            "--ovn-nb-endpoint=tcp:198.18.30.2:6641", "--ovn-sb-endpoint=tcp:198.18.30.2:6642",
            "--ovsdb-endpoint=unix:/var/run/openvswitch/db.sock",
            f"--standalone-evpn-export-config={B_BASE}/runtime.json",
            f"--evpn-export-status={B_BASE}/export-status.json", "--evpn-export-interval=1s",
            f"--evpn-remote-config={B_BASE}/import.json",
            f"--evpn-remote-status={B_BASE}/import-status.json", "--evpn-remote-interval=1s"])

    def b_rib(self, label, remote=False):
        if remote:
            return json.loads(self.r2("gobgp", "--target", f"unix://{R_BASE}/gobgp.sock",
                                      "global", "rib", "-a", "evpn", "-j", name=label+"-rib"))
        return json.loads(self.rt("b", "gobgp", "--target", f"unix://{B_BASE}/gobgp.sock",
                                  "global", "rib", "-a", "evpn", "-j", name=label+"-rib"))

    def b_export(self):
        status = self.bwait("export-status.json", lambda s: not s.get("Error") and
                           len((s.get("Result") or {}).get("Applied") or []) == 1, "b-export")
        self.b_uuid = status["Result"]["Applied"][0]["PathUUID"]
        require(status["SpeakerProcess"]["PID"] == self.b_processes["speaker"][0] and
                status["SpeakerProcess"]["StartTicks"], "B speaker not attested")
        journal = self.bread("export/journal.json", "b-export-receipt")
        require(journal["Record"]["Pending"] is None and
                journal["Record"]["State"]["Receipts"][0]["Owned"]["PathUUID"] == self.b_uuid,
                "B exact durable UUID absent")
        fence = self.bread("fence/journal.json", "b-accepted-fence")
        require(fence["Record"]["State"]["revision"] == 1 and fence["Record"]["State"]["digest"],
                "B fence missing")
        self.bwait("status.json", lambda s: not s["Error"] and len(s.get("Receipts") or []) == 1,
                   "r2-projected", True)
        validate_rib(self.b_rib("r2-received", True), remote=True)
        validate_rib(self.b_rib("b-exported"))
        self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000", name="r2-route-driven-fdb")
        self.r2("ip", "-j", "neigh", "show", "dev", "br2000", name="r2-route-driven-neighbor")
        self.rread("import/receipts-v1/journal.json", "r2-projector-journal")
        self.rread("export/journal.json", "r2-origin-journal")
        rstatus = self.rread("status.json", "r2-status")
        self.r_uuid = rstatus["Export"]["Applied"][0]["PathUUID"]
        require(rstatus["Speaker"]["PID"] == self.r_processes["speaker"][0] and
                rstatus["Receipts"][0]["Endpoint"]["MAC"] == B_MAC and
                all(rstatus["Receipts"][0][n]["ACK"] and
                    rstatus["Receipts"][0][n]["Status"] == "confirmed" for n in ("FDB", "Neighbor")),
                "Remote2 projector receipts/provenance differ")

    def b_import(self):
        first = self.bwait("import-status.json", lambda s: not s.get("Error") and
                          s["Snapshot"]["Authoritative"] and len(s.get("Receipts") or []) == 1, "b-import")
        time.sleep(3)
        second = self.bread("import-status.json", "b-second-observation")
        require(second["Time"] > first["Time"] and second["Snapshot"]["Authoritative"] and
                first["Snapshot"]["Endpoints"] == second["Snapshot"]["Endpoints"],
                "B complete observations differ")
        receipt = second["Receipts"][0]
        require(receipt["Endpoint"]["MAC"] == R2_MAC and receipt["Endpoint"]["IP"] == R2_IP and
                receipt["Endpoint"]["RemoteVTEP"] == "198.18.20.4" and
                all(receipt[n]["ACK"] and receipt[n]["Status"] == "confirmed" for n in ("FDB", "Neighbor")),
                "B importer identity/receipts differ")
        self.bread("import/receipts-v1/journal.json", "b-import-journal")
        self.wait(lambda: R2_MAC in self.rt("b", "ovs-ofctl", "dump-flows", "br-int"),
                  "B OVS remote flow absent")
        self.rt("b", "ip", "netns", "exec", "ext-b", "ip", "neigh", "add", R2_IP,
                "lladdr", R2_MAC, "nud", "permanent", "dev", "ext-b-eth")
        self.r2("ip", "netns", "exec", "remote2-work", "ip", "neigh", "add", B_IP,
                "lladdr", B_MAC, "nud", "permanent", "dev", "r2-work")
        for remote in (False, True):
            run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
            run("bridge", "fdb", "add", "02:00:00:fb:00:01", "dev",
                "vxlan2000" if remote else "evpnvx34", "dst", "198.18.20.99", "self", "permanent")
            run("ip", "neigh", "add", "10.100.2.199", "lladdr", "02:00:00:fb:00:01",
                "dev", "br2000" if remote else "evpnbr34", "nud", "permanent")

    def three_packets(self):
        self.ext_captures["state"] = self.directory / "three-capture-state.sh"
        names = ("three-worker-underlay", "three-frr-underlay", "b-underlay", "remote2-underlay")
        script = self.capture_preamble() + f"""
INTEROP_CAPTURE_OUTPUT_NAMES=({' '.join(names)})
trap 'declare -p INTEROP_CAPTURE_STAGING_DIR INTEROP_CAPTURE_UNITS INTEROP_CAPTURE_NAMES INTEROP_CAPTURE_MAIN_PIDS INTEROP_CAPTURE_CGROUPS INTEROP_CAPTURE_OUTPUT_NAMES > {shlex.quote(str(self.ext_captures["state"]))}' EXIT
interop_capture_create_staging_dir
"""
        for name, container, address, vtep, side in (
            ("three-worker-underlay", self.cluster+"-worker", "198.18.10.2", "198.18.10.4", "a"),
            ("three-frr-underlay", self.frr, "198.18.10.3", "198.18.10.2", None),
            ("b-underlay", self.cluster+"-worker2", "198.18.20.2", "198.18.20.4", "b"),
            ("remote2-underlay", self.remote2, "198.18.20.3", "198.18.20.2", None)):
            run = (lambda *a: self.rt(side, *a)) if side else self.fr if container == self.frr else self.r2
            interface = next(l["ifname"] for l in json.loads(run("ip", "-j", "-4", "addr"))
                             if any(a.get("local") == address for a in l.get("addr_info", [])))
            pid = self.command(["docker", "inspect", "--format", "{{.State.Pid}}", container]).strip()
            script += f"interop_capture_start_one {name} {int(pid)} {interface} 'udp port 4789 and host {vtep}'\n"
        script += "interop_capture_assert_active three-domains-ready\n"
        self.command(["bash", "-ec", script], "three-captures-launch")
        self.ext_captures["state"].chmod(0o600)
        # Preserve the prior two-domain Geneve evidence before a third capture.
        self.geneve_round = 0
        for side in ("a", "b"):
            self.rt(side, "mv", f"/tmp/geneve-{side}.pcap", f"/tmp/frr-geneve-{side}.pcap")
            (self.directory / f"geneve-{side}.pcap").rename(self.directory / f"frr-geneve-{side}.pcap")
        (self.directory / "geneve-packet-evidence.json").rename(self.directory / "frr-geneve-evidence.json")
        self.capture_start()
        self.rt("b", "ip", "netns", "exec", "ext-b", "ping", "-n", "-c", "3", "-W", "3",
                R2_IP, name="b-r2-ping")
        self.r2("ip", "netns", "exec", "remote2-work", "ping", "-n", "-c", "3", "-W", "3",
                B_IP, name="r2-b-ping")
        self.rt("a", "ip", "netns", "exec", "ext-a", "ping", "-n", "-c", "3", "-W", "3",
                REMOTE_IP, name="simultaneous-a-r1-ping")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "3", "-W", "3",
                LOCAL_IP, name="simultaneous-r1-a-ping")
        self.connectivity()
        self.stop_external_capture()
        self.packets()
        for side in ("a", "b"):
            self.rt(side, "ovs-vsctl", "--format=json", "list", "Interface", name=f"three-{side}-tunnels")
            self.rt(side, "ovs-ofctl", "dump-flows", "br-int", name=f"three-{side}-flows")
        self.r2("ip", "-d", "-j", "link", "show", name="three-r2-links")
        self.r2("bridge", "-j", "fdb", "show", name="three-r2-fdb")
        self.r2("ip", "-j", "neigh", "show", name="three-r2-neighbor")
        self.snapshot("three-domain1")
        self.b_rib("three-b")
        self.b_rib("three-r2", True)

    def stop_external_capture(self, require_packets=True):
        if self.ext_captures["state"].name != "three-capture-state.sh":
            return super().stop_external_capture(require_packets)
        state = self.ext_captures["state"]
        self.command(["bash", "-ec", self.capture_preamble() + f"""
source {shlex.quote(str(state))}
INTEROP_CAPTURE_VERIFY_COMMAND=(python3 "$ROOT/tools/e2e/external_gobgp.py" "$EVIDENCE_DIR")
rc=0
interop_capture_stop || rc=$?
printf '{{"shutdownFailure":%s,"verificationFailure":%s,"exit":%s}}\\n' "$INTEROP_CAPTURE_SHUTDOWN_FAILURE" "$INTEROP_PACKET_VERIFICATION_FAILURE" "$rc" > "$EVIDENCE_DIR/three-capture-stop.json"
exit "$rc"
"""], "three-capture-stop", check=False)
        status = json.loads((self.directory / "three-capture-stop.json").read_text())
        require(status["shutdownFailure"] == 0, "three-domain capture shutdown unsafe")
        self.ext_captures.clear()
        if require_packets:
            require(status["verificationFailure"] == status["exit"] == 0, "three-domain packet evidence missing")

    def owners(self):
        require(self.b_uuid != self.owned_uuid, "A/B export UUID shared")
        require(LOCAL_MAC not in json.dumps(self.b_rib("b-owner")) and
                B_MAC not in json.dumps(self.rib("a-owner")), "cross-node route ownership")
        for side, run in (("b", lambda *a, **kw: self.rt("b", *a, **kw)), ("r2", self.r2)):
            run("ip", "-j", "route", "show", name=f"{side}-no-transit-routes")
            run("sh", "-ec", "cat /proc/sys/net/ipv4/ip_forward", name=f"{side}-ip-forward")
        attachments = json.loads(self.command(["docker", "inspect", self.remote2]))[0]["NetworkSettings"]["Networks"]
        require(set(attachments) == {lab.load(self.directory)["remote2_network"]},
                "Remote2 attached to another domain")
        require(not self.nb("lr-list").strip() and
                self.r2("cat", "/proc/sys/net/ipv4/ip_forward").strip() == "0",
                "unintended tenant routing/transit configured")
        require("ls-ext1" not in json.dumps(self.bread("export.json")),
                "B has A export mapping")
        lab.write(self.directory / "independent-route-owners.json",
                  {"A": self.owned_uuid, "B": self.b_uuid, "R2": self.r_uuid,
                   "fullNegativeIsolation": "PLANNED"})

    def b_uncertainty(self):
        self.nb("lsp-del", self.b_lsp)
        self.wait(lambda: not self.sb("--bare", "--columns=_uuid", "find", "Port_Binding",
                                      "logical_port=ext-b").strip(), "B binding not absent")
        first = self.bwait("export-status.json", lambda s: bool(s.get("Error")) and any(
                          o["State"] == "uncertain" for o in (s.get("Observations") or [])), "b-uncertain")
        time.sleep(3)
        second = self.bread("export-status.json", "b-uncertain-stable")
        require(second["Time"] > first["Time"] and second.get("Error") and
                self.bread("export/journal.json")["Record"]["State"]["Receipts"][0]["Owned"]["PathUUID"]
                == self.b_uuid, "B uncertainty lost exact ownership")
        require(B_MAC in json.dumps(self.b_rib("r2-uncertain", True)), "R2 lost B during uncertainty")

    def projector_uncertainty(self):
        before = self.rread("status.json", "r2-before-uncertainty")["Receipts"]
        for mode in ("incomplete", "conflict"):
            self.bput("observation", mode+"\n", True)
            first = self.bwait("status.json", lambda s: bool(s["Error"]) and
                               not s["Snapshot"]["Authoritative"], "r2-"+mode, True)
            time.sleep(3)
            stable = self.rread("status.json", "r2-"+mode+"-stable")
            require(stable["Time"] > first["Time"] and stable["Error"] and
                    stable["Receipts"] == before, "uncertain/conflicting projector mutated receipts")
            require(B_MAC in self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000") and
                    B_IP in self.r2("ip", "-j", "neigh", "show", "dev", "br2000"),
                    "uncertain/conflicting RIB destructively cleaned owned state")
            self.r2("rm", f"{R_BASE}/observation")
            self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"],
                       "r2-"+mode+"-restored", True)

    def b_withdraw(self):
        self.rt("b", "ovs-vsctl", "del-port", "br-int", "ext-b-host")
        self.rt("b", "ip", "link", "del", "ext-b-host")
        self.rt("b", "ip", "netns", "del", "ext-b")
        self.b_policy(2, False, "deleted")
        self.bwait("export-status.json", lambda s: not s.get("Error") and
                   not (s.get("Result") or {}).get("Applied"), "b-withdrawn")
        self.bwait("status.json", lambda s: not s["Error"] and s["Snapshot"]["Authoritative"] and
                   not s["Receipts"], "r2-b-withdrawn", True)
        require(B_MAC not in json.dumps(self.b_rib("r2-b-absent", True)) and
                B_MAC not in self.r2("bridge", "-j", "fdb", "show", "dev", "vxlan2000") and
                B_IP not in self.r2("ip", "-j", "neigh", "show", "dev", "br2000"),
                "R2 retained B forwarding/route")
        self.bread("export/journal.json", "b-retired-journal")
        self.rread("import/receipts-v1/journal.json", "r2-drained-journal")
        self.a_survives("after-b-withdrawal")

    def r_withdraw(self):
        ep = self.rconfig["Endpoint"]
        self.bput("retire", ep["EndpointID"]+"/"+self.r_incarnation+"\n", True)
        self.bwait("status.json", lambda s: not s["Error"] and not s["Export"]["Applied"],
                   "r2-withdrawn", True)
        self.bwait("import-status.json", lambda s: not s.get("Error") and
                   s["Snapshot"]["Authoritative"] and not s.get("Receipts"), "b-r2-absent")
        self.wait(lambda: R2_MAC not in self.rt("b", "ovs-ofctl", "dump-flows", "br-int"),
                  "B retired R2 OVS flow remains")
        self.bread("import/receipts-v1/journal.json", "b-drained-journal")
        self.rread("export/journal.json", "r2-retired-origin-journal")
        before = json.loads(self.r2("ip", "-j", "link", "show", "dev", "br2000",
                                   name="r2-bridge-before-retirement"))[0]
        self.b_foreign("r2-before-retirement-foreign")
        self.r2("ip", "netns", "del", "remote2-work")
        after = json.loads(self.r2("ip", "-j", "link", "show", "dev", "br2000",
                                  name="r2-bridge-after-retirement"))[0]
        require(before["address"] == after["address"] == R2_BRIDGE_MAC and
                before["ifindex"] == after["ifindex"] and "LOWER_UP" in after["flags"],
                "Remote2 bridge identity/carrier changed during endpoint retirement")
        require(R2_MAC not in json.dumps(self.b_rib("b-r2-retired")), "B retains R2 route")
        self.a_survives("after-r2-withdrawal")

    def a_survives(self, label):
        require(self.read_json(f"{BASE}/export-receipts/journal.json")["Record"]["State"]["Receipts"][0]
                ["Owned"]["PathUUID"] == self.owned_uuid, "Domain1 UUID changed")
        self.rt("a", "ip", "netns", "exec", "ext-a", "ping", "-n", "-c", "2", "-W", "3",
                REMOTE_IP, name=label+"-a-r1")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "2", "-W", "3",
                LOCAL_IP, name=label+"-r1-a")
        self.rib(label+"-domain1")

    def b_foreign(self, label="domain2-foreign-preserved"):
        evidence = {}
        for remote in (False, True):
            side = "r2" if remote else "b"
            vxlan = "vxlan2000" if remote else "evpnvx34"
            bridge = "br2000" if remote else "evpnbr34"
            run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
            fdb = json.loads(run("bridge", "-j", "fdb", "show", "dev", vxlan,
                                 name=f"{label}-{side}-fdb"))
            neighbors = json.loads(run("ip", "-j", "neigh", "show", "dev", bridge,
                                       name=f"{label}-{side}-neighbors"))
            require(any(e.get("mac") == "02:00:00:fb:00:01" and
                        e.get("dst") == "198.18.20.99" and "self" in e.get("flags", []) and
                        e.get("state") == "permanent" for e in fdb),
                    f"{side} foreign Domain2 FDB identity removed or changed")
            require(any(e.get("dst") == "10.100.2.199" and
                        e.get("lladdr") == "02:00:00:fb:00:01" and
                        "PERMANENT" in e.get("state", []) for e in neighbors),
                    f"{side} foreign Domain2 neighbor identity removed or changed")
            evidence[side] = {"FDB": fdb, "Neighbors": neighbors}
        lab.write(self.directory / f"{label}.json", evidence)

    def bstop(self, name, remote=False):
        registry = self.r_processes if remote else self.b_processes
        pid, ticks, executable = registry[name]
        run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
        require(run("sh", "-ec", f"awk '{{print $22}}' /proc/{pid}/stat").strip() == ticks and
                run("sh", "-ec", f"tr '\\0' ' ' </proc/{pid}/cmdline").startswith(executable),
                "owned process incarnation changed")
        run("kill", "-TERM", str(pid))
        run("sh", "-ec", f"for i in $(seq 1 100); do test ! -e /proc/{pid}/stat && exit 0; "
            f"test \"$(awk '{{print $3}}' /proc/{pid}/stat)\" = Z && exit 0; sleep .1; done; exit 1")
        del registry[name]

    def b_cleanup(self):
        self.bstop("agent")
        self.bstop("speaker")
        self.bstop("projector", True)
        self.bstop("speaker", True)
        for remote in (False, True):
            run = self.r2 if remote else lambda *a, **kw: self.rt("b", *a, **kw)
            run("bridge", "fdb", "del", "02:00:00:fb:00:01", "dev",
                "vxlan2000" if remote else "evpnvx34", "dst", "198.18.20.99", "self")
            run("ip", "neigh", "del", "10.100.2.199", "dev", "br2000" if remote else "evpnbr34")
        self.nb("ls-del", self.b_switch)
        for device in ("evpnad34", "evpnvx34", "evpnbr34"):
            self.rt("b", "ip", "link", "del", device)
        self.rt("b", "ovs-vsctl", "remove", "Open_vSwitch", ".", "external_ids",
                "ovn-evpn-local-ip", "ovn-evpn-vxlan-ports")
        self.wait(lambda: not self.rt("b", "ovs-vsctl", "--bare", "--columns=name", "find",
                                      "Interface", "type=vxlan").strip(), "B OVS tunnel remains")
        self.r2("ip", "link", "del", "vxlan2000")
        self.r2("ip", "link", "del", "br2000")
        self.r2("ip", "addr", "del", "198.18.20.4/24", "dev", "eth1")
        self.r2("ip", "route", "del", "prohibit", "198.18.30.0/24")
        lab.write(self.directory / "domain2-owned-cleanup.json",
                  {"processes": [], "routes": [], "receipts": [], "devices": [], "endpoints": []})

    def non_owner(self):
        require(not self.b_processes and not self.r_processes,
                "Domain2 runtime still registered after drain")
        require("198.18.20.2:179" not in self.rt("b", "ss", "-lntp"),
                "Worker B retains a live BGP listener")
        require(not self.bread("export/journal.json")["Record"]["State"]["Receipts"] and
                self.sb("get", "Port_Binding", "ext-a", "chassis").strip() ==
                self.identities["a"]["chassisUUID"], "B retains ownership or A binding changed")
        lab.write(self.directory / "bounded-nonowner.json",
                  {"workerBHasLiveSpeaker": False, "extABoundToA": True,
                   "scope": "B owns only ext2; drained before A retirement, not full B2"})

    def diagnostics(self):
        super().diagnostics()
        if self.r_processes:
            self.r2("cat", f"{R_BASE}/projector.log", name="diagnostic-projector-log")
            self.r2("cat", f"{R_BASE}/status.json", name="diagnostic-projector-status")
        if self.b_processes:
            self.rt("b", "cat", f"{B_BASE}/agent.log", name="diagnostic-b-agent-log")


if __name__ == "__main__":
    import sys
    directory = Path(sys.argv[1])
    lab.write(directory / "domain2-packet-evidence.json", packets(directory))
    lab.write(directory / "three-domain1-packet-evidence.json",
              {name: decode_vxlan(directory / f"{name}.pcap")
               for name in ("three-worker-underlay", "three-frr-underlay")})
