"""External FRR extension of the existing two-worker acceptance fixture."""

import hashlib
import json
from pathlib import Path
import shlex
import struct
import time
import uuid

import containerlab as lab
from geneve_baseline import Baseline, CASES, require


EXTERNAL_CASES = (
    "external_fixture", "attested_runtime", "C1_export", "C2_import",
    "C3_vxlan", "simultaneous_geneve", "non_owner_b", "uncertainty_retention",
    "local_exact_withdrawal", "external_exact_withdrawal", "foreign_kernel_preservation",
    "external_cleanup",
)
LOCAL_MAC = "02:00:00:01:00:0a"
REMOTE_MAC = "02:00:00:01:00:65"
LOCAL_IP = "10.100.1.10"
REMOTE_IP = "10.100.1.101"
BASE = "/run/evpn-external"


class FRRBaseline(Baseline):
    def __init__(self, directory, cluster):
        super().__init__(directory, cluster)
        names = CASES[:-3] + EXTERNAL_CASES + CASES[-3:]
        self.cases = {n: {"status": "BLOCKED", "detail": "not reached"} for n in names}
        self.frr = cluster + "-frr"
        self.processes = {}
        self.ext_captures = {}
        self.phase = "initial"
        self.geneve_round = 0

    def external_actions(self):
        return (self.fixture, self.runtime, self.export, self.import_route, self.forwarding,
                self.coexistence, self.non_owner, self.uncertainty, self.local_withdraw,
                self.remote_withdraw, self.foreign_kernel, self.external_cleanup)

    def inputs(self):
        super().inputs()
        self.command(["docker", "image", "inspect", lab.FRR_IMAGE], "frr-image")
        self.command(["docker", "image", "inspect", "ghcr.io/purelb/k8gobgp:0.2.8"],
                     "speaker-image")
        build = self.directory / "build"
        build.mkdir()
        self.command(["go", "build", "-o", str(build / "evpn-node-agent"),
                      "./cmd/evpn-node-agent"], "build-agent")
        image = json.loads(self.command(["docker", "image", "inspect",
                                         "ghcr.io/purelb/k8gobgp:0.2.8"]))[0]
        require(image["Id"] == "sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0",
                "speaker image pin differs")
        # Copy pinned binaries without creating another container/resource owner.
        self.command(["docker", "image", "save", "-o", str(build / "speaker.tar"),
                      "ghcr.io/purelb/k8gobgp:0.2.8"], "save-speaker-image")
        import tarfile
        with tarfile.open(build / "speaker.tar") as archive:
            manifest = json.load(archive.extractfile("manifest.json"))[0]
            found = {}
            for layer in manifest["Layers"]:
                with tarfile.open(fileobj=archive.extractfile(layer)) as content:
                    for name in ("gobgp", "gobgpd"):
                        member = next((m for m in content.getmembers()
                                       if m.name.lstrip("./") == f"usr/local/bin/{name}"), None)
                        if member:
                            found[name] = content.extractfile(member).read()
            require(set(found) == {"gobgp", "gobgpd"}, "pinned speaker binaries missing")
            for name, data in found.items():
                (build / name).write_bytes(data)
        (build / "speaker.tar").unlink()
        self.command(["go", "version"], "go-version")
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/external_frr.py", "tools/lib/cni-evpn-frr-interop-test.sh",
                         "tools/lib/cni-vxlan-pcap-verify.py", "tools/e2e/test_external_frr.py"):
            target = self.directory / "source" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((lab.ROOT / relative).read_bytes())
            pins[relative] = hashlib.sha256((lab.ROOT / relative).read_bytes()).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)

    def provision(self):
        super().provision("geneve-frr", self.frr)

    def no_evpn(self):
        require(not self.rt("a", "sh", "-ec", "pgrep -x gobgpd || test $? = 1").strip(),
                "unexpected local BGP process before external setup")
        require(not self.rt("b", "sh", "-ec", "pgrep -x gobgpd || test $? = 1").strip(),
                "unexpected Worker B BGP speaker")
        for side in ("a", "b"):
            data = json.loads(self.rt(side, "ovs-vsctl", "--format=json", "--columns=type",
                                     "list", "Interface"))
            require(not any(row[0] == "vxlan" for row in data["data"]),
                    "baseline requires external VXLAN")
        lab.write(self.directory / "external-evpn-absence.json",
                  {"scope": "local A1 before configuring external session",
                   "localSpeakers": [], "localVxlan": False})

    def fr(self, *args, name=None):
        return self.command(["docker", "exec", self.frr, *args], name)

    def vty(self, *commands, name=None):
        args = ["vtysh"]
        for command in commands:
            args.extend(("-c", command))
        return self.fr(*args, name=name)

    def put(self, filename, value):
        source = self.directory / filename
        if isinstance(value, dict):
            lab.write(source, value)
        else:
            source.write_text(value)
        source.chmod(0o600)
        self.k("-n", "geneve-test", "cp", str(source),
               f"{self.pods['a']}:{BASE}/{filename}")
        self.rt("a", "sh", "-ec",
                f"chown root:root {BASE}/{filename}; chmod 600 {BASE}/{filename}")

    def read_json(self, path, name=None):
        return json.loads(self.rt("a", "cat", path, name=name))

    def wait_json(self, path, predicate, label):
        def poll():
            data = self.read_json(path, f"{label}-poll")
            return predicate(data)
        self.wait(poll, f"{label} did not converge")
        return self.read_json(path, label)

    def fixture(self):
        self.connections = {}
        for name, query in (("nb", self.nb), ("sb", self.sb)):
            rows = json.loads(query("--format=json", "--columns=target", "list", "Connection",
                                     name=f"{name}-connections-before"))
            self.connections[name] = [r[0] for r in rows["data"]]
        lab.write(self.directory / "original-db-connections.json", self.connections)
        self.rt("a", "sh", "-ec",
                "ip link add evpnbr33 type bridge\nip link set evpnbr33 up\n"
                "ip link add evpnvx33 type vxlan id 1000 local 198.18.10.2 dstport 60100 nolearning\n"
                "ip link set evpnvx33 master evpnbr33\nip link set evpnvx33 up\n"
                "ip link add evpnad33 type dummy\nip link set evpnad33 master evpnbr33\n"
                "ip link set evpnad33 up\n"
                "ovs-vsctl set Open_vSwitch . external_ids:ovn-evpn-local-ip=198.18.10.2 "
                "external_ids:ovn-evpn-vxlan-ports=4789")
        self.nb("set-connection", "ptcp:6641:198.18.30.2")
        self.sb("set-connection", "ptcp:6642:198.18.30.2")
        self.fr("ip", "route", "add", "prohibit", "198.18.30.0/24")
        self.nb("ls-add", "ls-ext1")
        self.ext_switch = self.nb("--bare", "--columns=_uuid", "find",
                                  "Logical_Switch", "name=ls-ext1").strip()
        self.nb("set", "Logical_Switch", self.ext_switch,
                f"external_ids:evpn.e2e.owner={self.directory.name}",
                "other_config:dynamic-routing-vni=1000",
                "other_config:dynamic-routing-bridge-ifname=evpnbr33",
                "other_config:dynamic-routing-vxlan-ifname=evpnvx33",
                "other_config:dynamic-routing-advertise-ifname=evpnad33",
                "other_config:dynamic-routing-redistribute=fdb,ip")
        self.nb("lsp-add", "ls-ext1", "ext-a")
        self.nb("lsp-set-addresses", "ext-a", f"{LOCAL_MAC} {LOCAL_IP}")
        self.lsp_uuid = self.nb("--bare", "--columns=_uuid", "find",
                                "Logical_Switch_Port", "name=ext-a").strip()
        self.endpoint = {"networkID": "ext1", "endpointID": str(uuid.uuid4()),
                         "attachmentID": str(uuid.uuid4()), "attachmentGeneration": 1,
                         "logicalSwitchPort": "ext-a", "logicalSwitchPortUUID": self.lsp_uuid,
                         "mac": LOCAL_MAC, "ip": LOCAL_IP}
        self.nb("set", "Logical_Switch_Port", self.lsp_uuid,
                "external_ids:evpn.framework/network-id=ext1",
                f"external_ids:evpn.framework/endpoint-id={self.endpoint['endpointID']}",
                f"external_ids:evpn.framework/attachment-id={self.endpoint['attachmentID']}",
                "external_ids:evpn.framework/attachment-generation=1")
        self.rt("a", "sh", "-ec",
                "ip netns add ext-a\nip link add ext-a-host type veth peer name ext-a-eth\n"
                "ip link set ext-a-eth netns ext-a\nip link set ext-a-host up\n"
                f"ip netns exec ext-a ip link set ext-a-eth address {LOCAL_MAC} mtu 1400\n"
                f"ip netns exec ext-a ip addr add {LOCAL_IP}/24 dev ext-a-eth\n"
                "ip netns exec ext-a ip link set lo up\n"
                "ip netns exec ext-a ip link set ext-a-eth up\n"
                "ovs-vsctl add-port br-int ext-a-host -- set Interface ext-a-host external_ids:iface-id=ext-a")
        self.wait(lambda: self.sb("--bare", "--columns=_uuid", "find", "Port_Binding",
                                  "logical_port=ext-a",
                                  f"chassis={self.identities['a']['chassisUUID']}",
                                  "up=true").strip(), "external local LSP not ready on A")
        self.nb("--format=json", "list", "Logical_Switch_Port", self.lsp_uuid, name="ext-lsp")
        self.sb("--format=json", "list", "Port_Binding", name="ext-bindings")
        ports = self.nb("get", "Logical_Switch", self.ext_switch, "ports")
        require(self.lsp_uuid in ports, "external LSP not in ls-ext1")
        self.fr("sh", "-ec",
                "ip addr add 198.18.10.4/24 dev eth0\n"
                "ip link add br1000 type bridge\nip addr add 10.100.1.1/24 dev br1000\n"
                "ip link set br1000 up\n"
                "ip link add vxlan1000 type vxlan id 1000 local 198.18.10.4 dstport 4789 nolearning\n"
                "ip link set vxlan1000 master br1000\nip link set vxlan1000 up\n"
                "bridge fdb append 00:00:00:00:00:00 dev vxlan1000 dst 198.18.10.2 self permanent\n"
                "ip netns add evpnwork\nip link add frr-host0 type veth peer name frr-work0\n"
                "ip link set frr-host0 master br1000\nip link set frr-host0 up\n"
                "ip link set frr-work0 netns evpnwork\n"
                f"ip netns exec evpnwork ip link set frr-work0 address {REMOTE_MAC} mtu 1400\n"
                f"ip netns exec evpnwork ip addr add {REMOTE_IP}/24 dev frr-work0\n"
                "ip netns exec evpnwork ip link set lo up\n"
                "ip netns exec evpnwork ip link set frr-work0 up")
        self.vty("configure terminal", "router bgp 64512",
                 "bgp router-id 198.18.10.3", "neighbor 198.18.10.2 remote-as 64512",
                 "address-family l2vpn evpn", "neighbor 198.18.10.2 activate",
                 "advertise-all-vni", "vni 1000", "rd 198.18.10.3:1000",
                 "route-target import 64512:1000", "route-target export 64512:1000",
                 "exit-vni", "end")
        self.vty("configure terminal", "route-map TYPE2-ONLY permit 10",
                 "match evpn route-type macip", "match evpn vni 1000", "exit",
                 "router bgp 64512", "address-family l2vpn evpn",
                 "neighbor 198.18.10.2 route-map TYPE2-ONLY out", "end")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "2", "-W", "2",
                "10.100.1.1", name="frr-workload-warmup")
        self.fr("ip", "route", "add", "blackhole", "203.0.113.0/24")
        self.vty("configure terminal", "router bgp 64512", "address-family ipv4 unicast",
                 "network 203.0.113.0/24", "end")
        self.foreign_route_before = json.loads(self.vty("show bgp ipv4 unicast 203.0.113.0/24 json",
                                                        name="foreign-frr-route-before"))
        require(self.foreign_route_before.get("paths"), "foreign FRR route absent")
        self.vty("show version", name="frr-version")
        self.vty("show running-config", name="frr-config")
        self.fr("bridge", "-j", "fdb", "show", name="frr-fdb-before-import")
        lab.write(self.directory / "static-fixture-state.json",
                  {"flood": {"mac": "00:00:00:00:00:00", "vtep": "198.18.10.2",
                             "device": "vxlan1000"},
                   "workloadNeighbors": {"localA": {"ip": REMOTE_IP, "mac": REMOTE_MAC},
                                         "remote1": {"ip": LOCAL_IP, "mac": LOCAL_MAC}},
                   "foreignRoute": {"frrIPv4": "203.0.113.0/24", "advertisedToEVPNPeer": False},
                   "note": "workload static neighbors only; tunnel FDB comes from BGP"})

    def launch(self, name, executable, arguments):
        pid = int(self.rt("a", "sh", "-ec",
                         f"{executable} \"$@\" >{BASE}/{name}.log 2>&1 </dev/null & echo $!",
                         name, *arguments).strip())
        self.processes[name] = (pid, executable)
        self.rt("a", "sh", "-ec", f"kill -0 {pid}")
        lab.write(self.directory / f"{name}-process.json",
                  {"pid": pid, "executable": executable, "arguments": arguments})
        return pid

    def runtime(self):
        self.rt("a", "install", "-d", "-m", "700", BASE)
        for name in ("gobgp", "gobgpd", "evpn-node-agent"):
            self.k("-n", "geneve-test", "cp", str(self.directory / "build" / name),
                   f"{self.pods['a']}:/usr/local/bin/{name}")
            self.rt("a", "chmod", "755", f"/usr/local/bin/{name}")
        self.put("gobgpd.yaml", """global:
  config:
    as: 64512
    router-id: "198.18.10.2"
    port: 179
    local-address-list: ["198.18.10.2"]
neighbors:
- config:
    neighbor-address: "198.18.10.3"
    peer-as: 64512
  afi-safis:
  - config:
      afi-safi-name: l2vpn-evpn
""")
        args = ["-f", f"{BASE}/gobgpd.yaml", "--api-hosts", f"unix://{BASE}/gobgp.sock"]
        pid = self.launch("speaker", "/usr/local/bin/gobgpd", args)
        self.wait(lambda: "READY" in self.rt("a", "sh", "-ec",
                  f"if test -S {BASE}/gobgp.sock; then echo READY; else echo STARTING; fi"),
                  "speaker API socket absent")
        self.wait(lambda: "ESTABLISHED" in self.rt(
            "a", "gobgp", "--target", f"unix://{BASE}/gobgp.sock",
            "neighbor", "198.18.10.3", name="gobgp-peer-poll").upper(), "BGP not established")
        cgroup = self.rt("a", "cat", f"/proc/{pid}/cgroup", name="speaker-cgroup")
        path = next(line[3:] for line in cgroup.splitlines() if line.startswith("0::"))
        require(path != "/", "speaker has unpinned private root cgroup")
        status = self.rt("a", "cat", f"/proc/{pid}/status", name="speaker-status")
        uid = int(next(line.split()[1] for line in status.splitlines() if line.startswith("Uid:")))
        self.installation = json.loads(self.k("get", "namespace", "geneve-test", "-o", "json"))["metadata"]["uid"]
        self.node = self.identities["a"]["nodeUID"]
        self.service = self.identities["a"]["podUID"]
        speaker = {"mode": "standalone", "standalone": {
            "installationUID": self.installation, "nodeUID": self.node, "serviceUID": self.service,
            "expectedUID": uid, "cgroupPath": path, "executablePath": "/usr/local/bin/gobgpd",
            "executableSHA256": hashlib.sha256((self.directory / "build/gobgpd").read_bytes()).hexdigest(),
            "arguments": ["/usr/local/bin/gobgpd", *args],
            "goBGPConfigPath": f"{BASE}/gobgpd.yaml",
            "goBGPConfigSHA256": hashlib.sha256((self.directory / "gobgpd.yaml").read_bytes()).hexdigest(),
            "asn": 64512, "routerID": "198.18.10.2"}}
        self.put("speaker.json", speaker)
        self.policy(1, True)
        recovery = {"InstallationUID": self.installation, "NodeUID": self.node}
        common = {"Enabled": True, "GRPCEndpoint": f"unix://{BASE}/gobgp.sock",
                  "AttestationConfigPath": f"{BASE}/speaker.json",
                  "ASN": 64512, "RouterID": "198.18.10.2"}
        export = {**common, "DryRun": False, "ProcessContinuity": True,
                  "NodeName": self.identities["a"]["nodeName"],
                  "LocalChassis": self.identities["a"]["systemId"],
                  "Recovery": {"StateDirectory": f"{BASE}/export-receipts", "Domain": recovery},
                  "Networks": [{"NetworkID": "ext1", "LogicalSwitch": "ls-ext1", "Enabled": True,
                                "ExporterSource": "framework", "VNI": 1000,
                                "RouteDistinguisher": "198.18.10.2:1000",
                                "RouteTargets": ["64512:1000"], "LocalVTEP": "198.18.10.2"}]}
        remote = {**common, "Import": {"LocalVTEP": "198.18.10.2", "Networks": [{
            "NetworkID": "ext1", "VNI": 1000, "ImportRouteTargets": ["64512:1000"],
            "AllowedVTEPs": ["198.18.10.4"]}]},
            "Netlink": {"Enabled": True, "ExclusiveWriter": True,
                        "StateDirectory": f"{BASE}/import-receipts",
                        "Recovery": {"Domain": recovery},
                        "Networks": [{"NetworkID": "ext1", "LogicalSwitch": "ls-ext1", "VNI": 1000,
                                      "BridgeDevice": "evpnbr33", "VXLANDevice": "evpnvx33"}]}}
        runtime = {"enabled": True, "enableRemoteImport": True,
                   "installationUID": self.installation, "nodeUID": self.node,
                   "localChassis": self.identities["a"]["systemId"],
                   "exporterConfigPath": f"{BASE}/export.json",
                   "authorizationPolicyPath": f"{BASE}/policy.json",
                   "authorizationFenceDirectory": f"{BASE}/authorization-fence"}
        for name, value in (("export.json", export), ("import.json", remote), ("runtime.json", runtime)):
            self.put(name, value)
        # Journal directories must be freshly created by their owner, not preseeded empty.
        self.rt("a", "install", "-d", "-m", "700", f"{BASE}/import-receipts")
        first = self.rib("before-reconcile-1")
        time.sleep(2)
        second = self.rib("before-reconcile-2")
        validate_raw_rib(first, require_local=False)
        validate_raw_rib(second, require_local=False)
        normalize = lambda rib: {key: [(p["nlri"], p["attrs"], p.get("peer-address"),
                                        p.get("best"), p.get("stale")) for p in paths]
                                 for key, paths in rib.items()}
        require(normalize(first) == normalize(second),
                "two pre-reconciliation complete RIB reads differ")
        self.rt("a", "sh", "-ec", f"test ! -e {BASE}/import-receipts/receipts-v1")
        require(REMOTE_MAC not in self.rt("a", "bridge", "-j", "fdb", "show", "dev", "evpnvx33") and
                REMOTE_IP not in self.rt("a", "ip", "-j", "neigh", "show", "dev", "evpnbr33"),
                "tested kernel path was preseeded before importer startup")
        self.launch("agent", "/usr/local/bin/evpn-node-agent", [
            "--mode=real-ovn-ovs", "--network-backend=linux-veth",
            "--ovn-nb-endpoint=unix:/var/run/ovn/ovnnb_db.sock",
            "--ovn-sb-endpoint=unix:/var/run/ovn/ovnsb_db.sock",
            "--ovsdb-endpoint=unix:/var/run/openvswitch/db.sock",
            f"--standalone-evpn-export-config={BASE}/runtime.json",
            f"--evpn-export-status={BASE}/export-status.json",
            "--evpn-export-interval=1s", "--evpn-export-timeout=3s",
            f"--evpn-remote-config={BASE}/import.json",
            f"--evpn-remote-status={BASE}/import-status.json",
            "--evpn-remote-interval=1s", "--evpn-remote-timeout=3s"])
        self.wait(lambda: "READY" in self.rt("a", "sh", "-ec",
                  f"if test -f {BASE}/export-status.json; then echo READY; else echo STARTING; fi"),
                  "export status missing")
        self.rt("a", "gobgpd", "--version", name="gobgp-version")

    def policy(self, revision, active, reason=None):
        value = {"version": 2, "revision": revision, "installationUID": self.installation,
                 "nodeUID": self.node, "localChassis": self.identities["a"]["systemId"],
                 "endpoints": [self.endpoint] if active else [],
                 "tombstones": [] if not reason else [{"reason": reason, "record": self.endpoint}]}
        self.put(f"policy-{revision}.json", value)
        self.rt("a", "sh", "-ec",
                f"cp {BASE}/policy-{revision}.json {BASE}/policy.next && "
                f"chmod 600 {BASE}/policy.next && mv {BASE}/policy.next {BASE}/policy.json")

    def rib(self, phase):
        raw = self.rt("a", "gobgp", "--target", f"unix://{BASE}/gobgp.sock",
                      "global", "rib", "-a", "evpn", "-j", name=f"{phase}-gobgp-rib")
        self.vty("show bgp l2vpn evpn route json", name=f"{phase}-frr-rib")
        return json.loads(raw)

    def snapshot(self, phase):
        for name, path in (("export", "export-receipts/journal.json"),
                           ("import", "import-receipts/receipts-v1/journal.json"),
                           ("fence", "authorization-fence/journal.json"),
                           ("export-status", "export-status.json"),
                           ("import-status", "import-status.json")):
            self.read_json(f"{BASE}/{path}", f"{phase}-{name}")
        self.rib(phase)
        for name, args in (("fdb", ("bridge", "-j", "fdb", "show", "dev", "evpnvx33")),
                           ("neighbor", ("ip", "-j", "neigh", "show", "dev", "evpnbr33")),
                           ("ovs", ("ovs-vsctl", "--format=json", "list", "Interface")),
                           ("flows", ("ovs-ofctl", "dump-flows", "br-int"))):
            self.rt("a", *args, name=f"{phase}-{name}")
        self.fr("bridge", "-j", "fdb", "show", name=f"{phase}-frr-fdb")
        self.fr("ip", "-j", "neigh", "show", name=f"{phase}-frr-neighbor")
        self.rt("a", "cat", f"{BASE}/agent.log", name=f"{phase}-agent-log")

    def export(self):
        status = self.wait_json(f"{BASE}/export-status.json",
                                lambda s: not s.get("Error") and
                                len((s.get("Result") or {}).get("Applied") or []) == 1,
                                "export-advertised")
        self.owned_uuid = status["Result"]["Applied"][0]["PathUUID"]
        journal = self.read_json(f"{BASE}/export-receipts/journal.json", "export-ack")
        receipt = journal["Record"]["State"]["Receipts"][0]["Owned"]
        require(journal["Record"]["Pending"] is None and
                receipt["PathUUID"] == self.owned_uuid, "export lacks exact acknowledged receipt")
        require(status["SpeakerProcess"]["PID"] == self.processes["speaker"][0] and
                status["SpeakerProcess"]["StartTicks"], "speaker was not locally attested")
        fence = self.read_json(f"{BASE}/authorization-fence/journal.json", "accepted-fence")
        require(fence["Record"]["Pending"] is None and
                fence["Record"]["State"]["revision"] == 1 and
                fence["Record"]["State"]["digest"], "authorization not durably accepted")
        self.wait(lambda: LOCAL_MAC in self.vty("show bgp l2vpn evpn route json"),
                  "FRR did not receive framework export")
        self.rib("exported")
        frr = json.loads((self.directory / "exported-frr-rib.stdout").read_text())
        routes = [route for route in frr["198.18.10.2:1000"].values()
                  if isinstance(route, dict) and route.get("prefix")]
        require(len(routes) == 1, "FRR export has unexpected route identities")
        path = routes[0]["paths"][0][0]
        require((path["routeType"], path["mac"], path["ip"], path["ethTag"],
                 path["peerId"], path["nexthops"][0]["ip"]) ==
                (2, LOCAL_MAC, LOCAL_IP, 0, "198.18.10.2", "198.18.10.2") and
                "RT:64512:1000" in path["extendedCommunity"]["string"] and
                "ET:8" in path["extendedCommunity"]["string"],
                "FRR received route attributes/provenance differ")
        validate_raw_rib(json.loads((self.directory / "exported-gobgp-rib.stdout").read_text()))
        lab.write(self.directory / "owned-export-uuid.json", {"uuid": self.owned_uuid})

    def import_route(self):
        self.wait_json(f"{BASE}/import-status.json",
                       lambda s: not s.get("Error") and len(s.get("Receipts") or []) == 1 and
                       s["Snapshot"]["Authoritative"] and s.get("RecoveryState") == "RUNNING",
                       "import-installed")
        self.wait(lambda: REMOTE_MAC in self.rt("a", "bridge", "-j", "fdb", "show",
                                               "dev", "evpnvx33") and REMOTE_IP in self.rt(
                                                   "a", "ip", "-j", "neigh", "show", "dev", "evpnbr33"),
                  "owned importer kernel state missing")
        self.snapshot("converged")
        self.import_before = self.read_json(f"{BASE}/import-receipts/receipts-v1/journal.json")
        record = self.import_before["Record"]
        require(record["Pending"] is None and not record["State"]["Quarantine"] and
                len(record["State"]["Receipts"]) == 1, "import acknowledgment pending/quarantined")
        receipt = next(iter(record["State"]["Receipts"].values()))
        require((receipt["Endpoint"]["MAC"], receipt["Endpoint"]["IP"],
                 receipt["Endpoint"]["RemoteVTEP"], receipt["Endpoint"]["ValidationState"]) ==
                (REMOTE_MAC, REMOTE_IP, "198.18.10.4", "validated"),
                "import endpoint identity differs")
        require(all(receipt[name]["ACK"] and receipt[name]["Status"] == "confirmed"
                    for name in ("FDB", "Neighbor")), "import kernel receipts not acknowledged")
        require(all(r["Route"]["Classification"] == "remote" and
                    r["Route"]["Source"]["NeighborIP"] == "198.18.10.3"
                    for r in receipt["Endpoint"]["Routes"]), "import was not sourced from FRR")
        first = self.read_json(f"{BASE}/import-status.json", "complete-observation-1")
        time.sleep(3)
        second = self.read_json(f"{BASE}/import-status.json", "complete-observation-2")
        require(first["Time"] != second["Time"] and first["Snapshot"]["Authoritative"] and
                second["Snapshot"]["Authoritative"] and
                first["Snapshot"]["Endpoints"] == second["Snapshot"]["Endpoints"] and
                first["Snapshot"]["Issues"] == second["Snapshot"]["Issues"],
                "complete RIB observations not equivalent")
        validate_raw_rib(self.rib("complete-observations"))
        # Workload neighbors avoid bootstrap; never seed tested tunnel FDB or sync neighbors.
        self.rt("a", "ip", "netns", "exec", "ext-a", "ip", "neigh", "replace", REMOTE_IP,
                "lladdr", REMOTE_MAC, "nud", "permanent", "dev", "ext-a-eth")
        self.fr("ip", "netns", "exec", "evpnwork", "ip", "neigh", "replace", LOCAL_IP,
                "lladdr", LOCAL_MAC, "nud", "permanent", "dev", "frr-work0")
        self.wait(lambda: LOCAL_MAC in self.fr("bridge", "-j", "fdb", "show", "dev", "vxlan1000"),
                  "FRR did not install received local endpoint FDB")
        self.rt("a", "bridge", "fdb", "add", "02:00:00:fa:00:01", "dev", "evpnvx33",
                "dst", "198.18.10.99", "self", "permanent")
        self.rt("a", "ip", "neigh", "add", "10.100.1.199", "lladdr", "02:00:00:fa:00:01",
                "dev", "evpnbr33", "nud", "permanent")
        self.fr("bridge", "fdb", "add", "02:00:00:fa:00:02", "dev", "vxlan1000",
                "dst", "198.18.10.99", "self", "permanent")

    def start_external_capture(self):
        interface = next(link["ifname"] for link in json.loads(self.rt("a", "ip", "-j", "-4", "addr"))
                         if any(a.get("local") == "198.18.10.2" for a in link.get("addr_info", [])))
        worker_pid = self.command(["docker", "inspect", "--format", "{{.State.Pid}}",
                                   self.cluster + "-worker"]).strip()
        frr_pid = self.command(["docker", "inspect", "--format", "{{.State.Pid}}", self.frr]).strip()
        self.ext_captures["state"] = self.directory / "external-capture-state.sh"
        script = self.capture_preamble() + f"""
INTEROP_CAPTURE_OUTPUT_NAMES=(worker-underlay frr-underlay)
trap 'declare -p INTEROP_CAPTURE_STAGING_DIR INTEROP_CAPTURE_UNITS INTEROP_CAPTURE_NAMES INTEROP_CAPTURE_MAIN_PIDS INTEROP_CAPTURE_CGROUPS INTEROP_CAPTURE_OUTPUT_NAMES > {shlex.quote(str(self.ext_captures["state"]))}' EXIT
interop_capture_create_staging_dir
interop_capture_start_one worker-underlay {int(worker_pid)} {shlex.quote(interface)} 'udp port 4789 and host 198.18.10.2 and host 198.18.10.4'
interop_capture_start_one frr-underlay {int(frr_pid)} eth0 'udp port 4789 and host 198.18.10.2 and host 198.18.10.4'
interop_capture_assert_active external-ready
"""
        self.command(["bash", "-ec", script], "external-capture-launch")
        self.ext_captures["state"].chmod(0o600)

    def capture_preamble(self):
        return f"""set -eu
ROOT={shlex.quote(str(lab.ROOT))}
KIND_CLUSTER={shlex.quote(self.cluster)}
EVIDENCE_DIR={shlex.quote(str(self.directory))}
source "$ROOT/tools/lib/cni-evpn-frr-interop-test.sh"
"""

    def capture_start(self):
        if self.geneve_round:
            for side in ("a", "b"):
                self.rt(side, "mv", f"/tmp/geneve-{side}.pcap", f"/tmp/initial-geneve-{side}.pcap")
                (self.directory / f"geneve-{side}.pcap").rename(
                    self.directory / f"initial-geneve-{side}.pcap")
            (self.directory / "geneve-packet-evidence.json").rename(
                self.directory / "initial-geneve-packet-evidence.json")
        super().capture_start()
        self.geneve_round += 1

    def stop_external_capture(self, require_packets=True):
        state = self.ext_captures["state"]
        self.command(["bash", "-ec", self.capture_preamble() +
                      f"""source {shlex.quote(str(state))}
INTEROP_CAPTURE_VERIFY_COMMAND=(python3 "$ROOT/tools/e2e/external_frr.py" "$EVIDENCE_DIR")
rc=0
interop_capture_stop || rc=$?
printf '{{"shutdownFailure":%s,"verificationFailure":%s,"exit":%s}}\\n' "$INTEROP_CAPTURE_SHUTDOWN_FAILURE" "$INTEROP_PACKET_VERIFICATION_FAILURE" "$rc" > "$EVIDENCE_DIR/external-capture-stop.json"
exit "$rc"
"""], "external-capture-stop", check=False)
        result = json.loads((self.directory / "external-capture-stop.json").read_text())
        require(result["shutdownFailure"] == 0, "external capture shutdown not proven safe")
        self.ext_captures.clear()
        if require_packets:
            require(result["verificationFailure"] == 0 and result["exit"] == 0,
                    "external capture packet verification failed")

    def forwarding(self):
        self.start_external_capture()
        self.capture_start()
        self.rt("a", "ip", "netns", "exec", "ext-a", "ping", "-n", "-c", "3", "-W", "3",
                REMOTE_IP, name="external-a-frr-ping")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "3", "-W", "3",
                LOCAL_IP, name="external-frr-a-ping")
        self.connectivity()
        self.stop_external_capture()
        lab.write(self.directory / "external-packet-evidence.json", {
            side: decode_vxlan(self.directory / f"{name}-underlay.pcap")
            for side, name in (("a", "worker"), ("frr", "frr"))})
        self.snapshot("forwarded")
        tunnels = self.rt("a", "ovs-vsctl", "--bare", "--columns=name", "find", "Interface",
                          "type=vxlan", "options:dst_port=4789", name="forwarded-vxlan-tunnel").split()
        require(len(tunnels) == 1, "expected exactly one external OVS VXLAN interface")
        self.rt("a", "ovs-vsctl", "get", "Interface", tunnels[0], "options", name="external-ovs-options")
        require(int(self.rt("a", "ovs-vsctl", "get", "Interface", tunnels[0], "ofport")) > 0,
                "external VXLAN has no installed OpenFlow port")
        self.rt("a", "ovs-ofctl", "dump-ports", "br-int", name="external-ovs-port-counters")
        self.fr("ip", "-d", "-j", "link", "show", "vxlan1000", name="external-frr-vxlan-link")

    def coexistence(self):
        self.packets()
        self.snapshot("simultaneous")

    def non_owner(self):
        require(not self.rt("b", "sh", "-ec", "pgrep -x gobgpd || test $? = 1").strip(),
                "B unexpectedly has a BGP writer")
        bindings = self.sb("get", "Port_Binding", "ext-a", "chassis").strip()
        require(bindings == self.identities["a"]["chassisUUID"], "external endpoint moved to B")
        lab.write(self.directory / "bounded-nonowner.json",
                  {"workerBHasSpeaker": False, "extABoundToA": True,
                   "scope": "supporting isolation only, not full B2 authorization injection"})

    def uncertainty(self):
        self.nb("lsp-del", self.lsp_uuid)
        self.wait(lambda: not self.sb("--bare", "--columns=_uuid", "find", "Port_Binding",
                                      "logical_port=ext-a").strip(), "retired local binding remains")
        first = self.wait_json(f"{BASE}/export-status.json",
                       lambda s: bool(s.get("Error")) and any(
                           o.get("State") == "uncertain" for o in (s.get("Observations") or [])),
                       "uncertain-export")
        time.sleep(3)
        second = self.read_json(f"{BASE}/export-status.json", "uncertain-export-stable")
        require(second["Time"] > first["Time"] and second["Error"] and
                any(o["State"] == "uncertain" for o in second["Observations"]),
                "uncertain observation did not remain fail-closed")
        journal = self.read_json(f"{BASE}/export-receipts/journal.json", "uncertain-export-journal")
        require(journal["Record"]["State"]["Receipts"][0]["Owned"]["PathUUID"] == self.owned_uuid,
                "uncertainty withdrew valid owned route")
        require(LOCAL_MAC in self.vty("show bgp l2vpn evpn route json"),
                "FRR lost route during uncertainty")

    def local_withdraw(self):
        require(not self.nb("--bare", "--columns=_uuid", "find", "Logical_Switch_Port",
                            "name=ext-a").strip(), "local NB identity not absent")
        self.rt("a", "ovs-vsctl", "del-port", "br-int", "ext-a-host")
        self.rt("a", "ip", "link", "del", "ext-a-host")
        self.rt("a", "ip", "netns", "del", "ext-a")
        self.wait(lambda: not self.sb("--bare", "--columns=_uuid", "find",
                                      "Port_Binding", "logical_port=ext-a").strip(),
                  "retired local binding remains")
        self.policy(2, False, "deleted")
        self.wait_json(f"{BASE}/export-status.json",
                       lambda s: not s.get("Error") and
                       not (s.get("Result") or {}).get("Applied"),
                       "local-withdrawn")
        self.wait(lambda: LOCAL_MAC not in self.vty("show bgp l2vpn evpn route json") and
                  LOCAL_MAC not in self.fr("bridge", "-j", "fdb", "show", "dev", "vxlan1000"),
                  "FRR retained retired local route or FDB")
        self.snapshot("local-retired")
        require(REMOTE_MAC in json.dumps(self.rib("foreign-remote-survives-local-retirement")),
                "local UUID withdrawal removed independent FRR route")
        journal = self.read_json(f"{BASE}/export-receipts/journal.json")
        require(not journal["Record"]["State"]["Receipts"], "owned export receipt remains")

    def remote_withdraw(self):
        self.fr("ip", "netns", "del", "evpnwork")
        self.wait(lambda: REMOTE_MAC not in self.vty("show bgp l2vpn evpn route json"),
                  "FRR-owned external route remained")
        self.wait(lambda: REMOTE_MAC not in self.rt("a", "bridge", "-j", "fdb", "show",
                                                   "dev", "evpnvx33") and REMOTE_IP not in self.rt(
                                                       "a", "ip", "-j", "neigh", "show", "dev", "evpnbr33"),
                  "receipt-owned remote kernel state remained")
        self.wait(lambda: REMOTE_MAC not in self.rt("a", "ovs-ofctl", "dump-flows", "br-int"),
                  "OVS retains retired remote MAC flow")
        self.snapshot("remote-retired")
        journal = self.read_json(f"{BASE}/import-receipts/receipts-v1/journal.json")
        require(not journal["Record"]["State"]["Receipts"] and journal["Record"]["Pending"] is None and
                journal["Record"]["State"]["Snapshot"]["Authoritative"],
                "importer did not durably acknowledge complete route absence")

    def foreign_kernel(self):
        require("02:00:00:fa:00:01" in self.rt("a", "bridge", "-j", "fdb", "show",
                                             "dev", "evpnvx33"), "foreign local FDB removed")
        require("10.100.1.199" in self.rt("a", "ip", "-j", "neigh", "show",
                                         "dev", "evpnbr33"), "foreign local neighbor removed")
        require("02:00:00:fa:00:02" in self.fr("bridge", "-j", "fdb", "show",
                                             "dev", "vxlan1000"), "foreign remote FDB removed")
        after = json.loads(self.vty("show bgp ipv4 unicast 203.0.113.0/24 json",
                                    name="foreign-frr-route-after"))
        require(after["prefix"] == self.foreign_route_before["prefix"] and
                after["paths"][0]["nexthops"] == self.foreign_route_before["paths"][0]["nexthops"],
                "foreign FRR route changed")

    def stop_process(self, name):
        pid, executable = self.processes[name]
        cmd = self.rt("a", "sh", "-ec", f"tr '\\0' ' ' < /proc/{pid}/cmdline")
        require(cmd.startswith(executable), f"{name} PID changed")
        self.rt("a", "kill", "-TERM", str(pid))
        self.rt("a", "sh", "-ec", f"for i in $(seq 1 100); do "
                f"test ! -e /proc/{pid}/stat && exit 0; "
                f"test \"$(awk '{{print $3}}' /proc/{pid}/stat)\" = Z && exit 0; "
                "sleep 0.1; done; exit 1")
        del self.processes[name]

    def external_cleanup(self):
        self.stop_process("agent")
        self.stop_process("speaker")
        self.rt("a", "bridge", "fdb", "del", "02:00:00:fa:00:01", "dev", "evpnvx33",
                "dst", "198.18.10.99", "self")
        self.rt("a", "ip", "neigh", "del", "10.100.1.199", "dev", "evpnbr33")
        self.fr("bridge", "fdb", "del", "02:00:00:fa:00:02", "dev", "vxlan1000",
                "dst", "198.18.10.99", "self")
        self.nb("ls-del", self.ext_switch)
        for dev in ("evpnad33", "evpnvx33", "evpnbr33"):
            self.rt("a", "ip", "link", "del", dev)
        self.rt("a", "ovs-vsctl", "remove", "Open_vSwitch", ".", "external_ids",
                "ovn-evpn-local-ip", "ovn-evpn-vxlan-ports")
        self.wait(lambda: not self.rt("a", "ovs-vsctl", "--bare", "--columns=name", "find",
                                      "Interface", "type=vxlan").strip(),
                  "owned external OVS tunnel remains")
        self.fr("ip", "link", "del", "vxlan1000")
        self.fr("ip", "link", "del", "br1000")
        self.fr("ip", "addr", "del", "198.18.10.4/24", "dev", "eth0")
        self.fr("ip", "route", "del", "prohibit", "198.18.30.0/24")
        self.vty("configure terminal", "router bgp 64512", "no neighbor 198.18.10.2",
                 "address-family l2vpn evpn", "no vni 1000", "exit-address-family",
                 "address-family ipv4 unicast", "no network 203.0.113.0/24",
                 "exit-address-family", "exit", "no route-map TYPE2-ONLY", "end")
        self.fr("ip", "route", "del", "blackhole", "203.0.113.0/24")
        for name, query in (("nb", self.nb), ("sb", self.sb)):
            if self.connections[name]:
                query("set-connection", *self.connections[name])
            else:
                query("del-connection")
            restored = json.loads(query("--format=json", "--columns=target", "list", "Connection",
                                         name=f"{name}-connections-restored"))
            require(sorted(r[0] for r in restored["data"]) == sorted(self.connections[name]),
                    "fixture database listener not restored")
        require(not self.processes and not self.ext_captures, "owned processes still registered")
        lab.write(self.directory / "external-owned-cleanup.json",
                  {"processes": [], "endpoints": [], "ownedRoutes": [],
                   "syncDevices": [], "ovsVxlan": [], "fixtureConnectionsRestored": True})
        self.phase = "cleaned"

    def diagnostics(self):
        if self.ext_captures:
            self.stop_external_capture(require_packets=False)
        super().diagnostics()
        for name in ("speaker", "agent"):
            self.command([self.k_binary(), "--kubeconfig", str(self.directory / "kubeconfig"),
                          "--context", f"kind-{self.cluster}", "-n", "geneve-test", "exec",
                          self.pods["a"], "--", "cat", f"{BASE}/{name}.log"],
                         f"diagnostic-{name}-log", check=False)
        self.vty("show bgp l2vpn evpn summary json", name="diagnostic-frr-peer")
        self.vty("show bgp l2vpn evpn route json", name="diagnostic-frr-rib")

    @staticmethod
    def k_binary():
        from pathlib import Path
        return "/snap/kubectl/current/kubectl" if Path("/snap/kubectl/current/kubectl").exists() else "kubectl"


def decode_vxlan(path, vni=1000, local_vtep="198.18.10.2", remote_vtep="198.18.10.4",
                 local_mac=LOCAL_MAC, remote_mac=REMOTE_MAC, local_ip=LOCAL_IP, remote_ip=REMOTE_IP):
    import importlib.util
    spec = importlib.util.spec_from_file_location("pcap", lab.ROOT / "tools/lib/cni-vxlan-pcap-verify.py")
    decoder = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(decoder)
    _, records = decoder.read_pcap(path)
    identities = {}
    packets = []
    for record in records:
        frame = record["frame"]
        eth = decoder.ethernet_packet(frame)
        if not eth or eth[2] != 0x0800:
            continue
        outer = decoder.ipv4_packet(frame, eth[3])
        if not outer or outer["protocol"] != 17:
            continue
        udp = outer["payloadOffset"]
        if outer["end"] < udp + 16:
            continue
        _, port, length = struct.unpack_from("!HHH", frame, udp)
        if port != 4789:
            continue
        require(length >= 16 and udp + length <= outer["end"], "truncated VXLAN UDP")
        vxlan = udp + 8
        require(frame[vxlan] & 8 and int.from_bytes(frame[vxlan + 4:vxlan + 7], "big") == vni,
                "wrong VXLAN VNI/header")
        inner = decoder.inner_packet(frame[:udp + length], vxlan + 8)
        if not inner or "icmp" not in inner:
            continue
        for source, destination, smac, dmac, sip, dip in (
            (local_vtep, remote_vtep, local_mac, remote_mac, local_ip, remote_ip),
            (remote_vtep, local_vtep, remote_mac, local_mac, remote_ip, local_ip)):
            if (outer["source"], outer["destination"], inner.get("sourceMac"),
                    inner.get("destinationMac"), inner.get("sourceIp"), inner.get("destinationIp")) != (
                        source, destination, smac, dmac, sip, dip):
                continue
            icmp = inner["icmp"]
            require(icmp["code"] == 0 and icmp["type"] in (0, 8), "not an endpoint echo packet")
            key = f"{source}-{icmp['type']}"
            identities.setdefault(key, set()).add((icmp["identifier"], icmp["sequence"]))
            packets.append({"outerSource": source, "outerDestination": destination,
                            "sourceMac": smac, "destinationMac": dmac,
                            "sourceIp": sip, "destinationIp": dip, "vni": vni,
                            "type": icmp["type"], "id": icmp["identifier"], "seq": icmp["sequence"]})
    for vtep in (local_vtep, remote_vtep):
        for kind in (0, 8):
            require(len(identities.get(f"{vtep}-{kind}", ())) >= 3,
                    f"{path}: missing attributed external VXLAN packets")
    return {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "packets": packets}


def validate_raw_rib(rib, require_local=True):
    matched = set()
    for paths in rib.values():
        for path in paths:
            nlri = path["nlri"]
            remote = path.get("peer-address") == "198.18.10.3"
            if nlri["type"] != 2:
                require(not remote, "FRR advertised a route outside the allowed Type-2 identity")
                continue
            route = nlri["value"]
            mac = REMOTE_MAC if remote else LOCAL_MAC
            ip = REMOTE_IP if remote else LOCAL_IP
            admin = "198.18.10.3" if remote else "198.18.10.2"
            vtep = "198.18.10.4" if remote else "198.18.10.2"
            require(route["mac"] == mac and route.get("ip", "") in
                    ((ip, "") if remote else (ip,)), "unexpected Type-2 endpoint")
            require(route["rd"] == {"type": 1, "admin": admin, "assigned": 1000} and
                    route["etag"] == 0 and route["esi"] == "single-homed" and
                    route["labels"] == [1000], "Type-2 NLRI attributes differ")
            attributes = {a["type"]: a for a in path["attrs"]}
            require(attributes[14]["nexthop"] == vtep and attributes[14]["afi"] == 25 and
                    attributes[14]["safi"] == 70, "wrong MP_REACH next hop/family")
            communities = attributes[16]["value"]
            require(any(c.get("value") == "64512:1000" for c in communities) and
                    any(c.get("tunnel_type") == 8 for c in communities),
                    "wrong RT or VXLAN encapsulation")
            matched.add((mac, route.get("ip", "")))
    expected = {(REMOTE_MAC, REMOTE_IP)}
    if require_local:
        expected.add((LOCAL_MAC, LOCAL_IP))
    require(expected <= matched,
            "missing required local/remote Type-2")


if __name__ == "__main__":
    import sys
    directory = Path(sys.argv[1])
    lab.write(directory / "external-packet-evidence.json", {
        side: decode_vxlan(directory / f"{name}-underlay.pcap")
        for side, name in (("a", "worker"), ("frr", "frr"))})
