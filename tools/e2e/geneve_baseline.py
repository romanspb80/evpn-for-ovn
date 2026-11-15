"""Opt-in A1 fixture for the existing operator Type-2 runner."""

import collections
import hashlib
import importlib.util
import json
from pathlib import Path
import signal
import struct
import subprocess
import time

import containerlab as lab


OVN_IMAGE = "evpn4ovn/ovn-ovs:kind-26.09"
OVN_ID = "sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
CASES = (
    "local_inputs", "two_ready_workers", "independent_ovs_controllers",
    "distinct_geneve_chassis", "shared_local_switch", "exact_bindings",
    "bidirectional_connectivity", "attributed_geneve", "no_external_evpn",
    "owned_endpoint_cleanup", "foreign_preservation", "coordinator_teardown",
)
ENDPOINTS = {
    "a": ("02:00:00:30:00:0a", "10.100.30.10", "198.18.30.2"),
    "b": ("02:00:00:30:00:0b", "10.100.30.20", "198.18.30.3"),
}


def require(condition, message):
    if not condition:
        raise lab.LabError(message)


def decode_geneve(path, tunnel_key):
    spec = importlib.util.spec_from_file_location(
        "pcap", lab.ROOT / "tools/lib/cni-vxlan-pcap-verify.py")
    decoder = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(decoder)
    _, records = decoder.read_pcap(path)
    counts = collections.Counter()
    identities = collections.defaultdict(set)
    evidence = []
    for record in records:
        frame = record["frame"]
        ether = decoder.ethernet_packet(frame)
        if not ether or ether[2] != 0x0800:
            continue
        outer = decoder.ipv4_packet(frame, ether[3])
        if not outer or outer["protocol"] != 17:
            continue
        udp = outer["payloadOffset"]
        if outer["end"] < udp + 16:
            continue
        _, port, length = struct.unpack_from("!HHH", frame, udp)
        require(port != 4789, "external VXLAN packet observed during Geneve baseline")
        if port != 6081 or length < 16 or udp + length > outer["end"]:
            continue
        geneve = udp + 8
        require(frame[geneve] >> 6 == 0, "unsupported Geneve version")
        require(struct.unpack_from("!H", frame, geneve + 2)[0] == 0x6558,
                "Geneve payload is not transparent Ethernet")
        key = int.from_bytes(frame[geneve + 4:geneve + 7], "big")
        inner_offset = geneve + 8 + (frame[geneve] & 0x3f) * 4
        inner = decoder.inner_packet(frame[:udp + length], inner_offset)
        if not inner or "icmp" not in inner:
            continue
        for source, destination in (("a", "b"), ("b", "a")):
            smac, sip, svtep = ENDPOINTS[source]
            dmac, dip, dvtep = ENDPOINTS[destination]
            if (outer["source"], outer["destination"], inner.get("sourceMac"),
                    inner.get("destinationMac"), inner.get("sourceIp"),
                    inner.get("destinationIp")) != (svtep, dvtep, smac, dmac, sip, dip):
                continue
            require(key == tunnel_key, "packet Geneve key differs from OVN datapath")
            icmp = inner["icmp"]
            require(icmp["code"] == 0, "unexpected ICMP error")
            if icmp["type"] not in (0, 8):
                continue
            label = f"{source}-{destination}-{icmp['type']}"
            counts[label] += 1
            identities[label].add((icmp["identifier"], icmp["sequence"]))
            evidence.append({"timestamp": record["timestamp"], "outerSource": svtep,
                             "outerDestination": dvtep, "tunnelKey": key,
                             "sourceMac": smac, "destinationMac": dmac,
                             "sourceIp": sip, "destinationIp": dip,
                             "type": icmp["type"], "identifier": icmp["identifier"],
                             "sequence": icmp["sequence"]})
    for direction in ("a-b", "b-a"):
        for kind in (0, 8):
            require(len(identities[f"{direction}-{kind}"]) >= 3,
                    f"{path}: missing attributed {direction} ICMP {kind}")
    return {"sha256": hashlib.sha256(Path(path).read_bytes()).hexdigest(),
            "counts": dict(counts), "packets": evidence}


class Baseline:
    def __init__(self, directory, cluster):
        self.directory = directory
        self.cluster = cluster
        self.active = CASES[0]
        self.cases = {name: {"status": "BLOCKED", "detail": "not reached"} for name in CASES}
        self.index = 0
        self.pods = {}
        self.captures = {}
        self.switch_uuid = None
        self.port_ids = {}
        self.foreign_uuid = None

    def command(self, args, name=None, check=True):
        self.index += 1
        name = name or f"command-{self.index:04d}"
        result = lab.command(args, check=False, timeout=240)
        (self.directory / f"{name}.stdout").write_text(result.stdout)
        (self.directory / f"{name}.stderr").write_text(result.stderr)
        lab.write(self.directory / f"{name}.command.json",
                  {"args": args, "exit": result.returncode})
        if check and result.returncode:
            raise lab.LabError(f"{name} failed ({result.returncode}); see retained stderr")
        return result.stdout

    def k(self, *args, name=None):
        binary = "/snap/kubectl/current/kubectl"
        if not Path(binary).exists():
            binary = "kubectl"
        return self.command([binary, "--kubeconfig", str(self.directory / "kubeconfig"),
                             "--context", f"kind-{self.cluster}", *args], name)

    def rt(self, side, *args, name=None):
        return self.k("-n", "geneve-test", "exec", self.pods[side], "--", *args, name=name)

    def nb(self, *args, name=None):
        return self.rt("a", "ovn-nbctl", *args, name=name)

    def sb(self, *args, name=None):
        return self.rt("a", "ovn-sbctl", *args, name=name)

    def assertion(self, name, action):
        self.active = name
        action()
        self.cases[name] = {"status": "PASS"}
        print(f"acceptance-case={name} status=PASS", flush=True)

    def wait(self, action, message):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if action():
                return
            time.sleep(1)
        raise lab.LabError(message)

    def inputs(self):
        require(not (lab.STATE_ROOT / "active.json").exists(),
                "retained lab requires scoped teardown before starting")
        for binary in ("kind", "docker", "containerlab", "python3"):
            self.command(["which", binary])
        self.command(["sudo", "-n", "true"])
        image = json.loads(self.command(["docker", "image", "inspect", OVN_IMAGE],
                                       "ovn-image"))[0]
        require(image["Id"] == OVN_ID, "unexpected local OVN image")
        self.command(["docker", "image", "inspect", lab.NODE_IMAGE], "kind-image")
        self.command(["kind", "version"], "kind-version")
        self.command(["uname", "-a"], "kernel-version")
        self.command(["git", "-C", str(lab.ROOT), "rev-parse", "HEAD"], "source-head")
        self.command(["git", "-C", str(lab.ROOT), "diff"], "source-diff")
        self.command(["git", "-C", str(lab.ROOT), "status", "--porcelain=v1"], "source-status")
        source = self.directory / "source"
        source.mkdir()
        hashes = {}
        for relative in ("tools/e2e/containerlab.py", "tools/e2e/geneve_baseline.py",
                         "tools/e2e/test_geneve_baseline.py", "tools/run-operator-type2-e2e.sh",
                         "deploy/containerlab/topologies/kind-two-worker.yaml"):
            data = (lab.ROOT / relative).read_bytes()
            target = source / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            hashes[relative] = hashlib.sha256(data).hexdigest()
        lab.write(self.directory / "source-inputs.json", hashes)
        lab.write(self.directory / "endpoint-plan.json", ENDPOINTS)

    def provision(self, profile="geneve", frr=None):
        lab.init(self.directory, profile, self.cluster, f"evpn33b3-{self.cluster}", frr)
        lab.up(self.directory)
        self.k("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=180s",
               name="nodes-ready")
        nodes = json.loads(self.k("get", "nodes", "-o", "json", name="nodes"))["items"]
        expected = {self.cluster + suffix for suffix in ("-control-plane", "-worker", "-worker2")}
        require({n["metadata"]["name"] for n in nodes} == expected, "wrong Kubernetes node set")
        require(len({n["metadata"]["uid"] for n in nodes}) == 3, "duplicate Kubernetes UID")
        for node in nodes:
            require(any(c["type"] == "Ready" and c["status"] == "True"
                        for c in node["status"]["conditions"]), "node not Ready")
            if node["metadata"]["name"].endswith(("-worker", "-worker2")):
                require("node-role.kubernetes.io/control-plane" not in node["metadata"]["labels"],
                        "expected worker is a control-plane node")
        self.nodes = {n["metadata"]["name"]: n["metadata"]["uid"] for n in nodes}
        self.command(["kind", "load", "docker-image", "--name", self.cluster, OVN_IMAGE],
                     "load-ovn-image")

    def ovs(self):
        self.k("create", "namespace", "geneve-test")
        for side, worker in (("a", "-worker"), ("b", "-worker2")):
            pod = f"ovn-{side}"
            self.pods[side] = pod
            db = ("/usr/share/ovn/scripts/ovn-ctl "
                  "--db-nb-create-insecure-remote=yes --db-sb-create-insecure-remote=yes "
                  "start_northd\n") if side == "a" else ""
            startup = (
                "set -eu\n"
                "/usr/share/openvswitch/scripts/ovs-ctl --system-id=random start\n"
                "ovs-vsctl add-br br-int\nip link set br-int up\n" + db +
                "ovs-vsctl set Open_vSwitch . "
                "external_ids:ovn-remote=tcp:198.18.30.2:6642 "
                "external_ids:ovn-encap-type=geneve "
                f"external_ids:ovn-encap-ip={ENDPOINTS[side][2]}\n"
                "/usr/share/ovn/scripts/ovn-ctl start_controller\nexec sleep infinity\n")
            manifest = {"apiVersion": "v1", "kind": "Pod",
                        "metadata": {"name": pod, "namespace": "geneve-test",
                                     "labels": {"evpn.e2e.owner": self.directory.name}},
                        "spec": {"nodeName": self.cluster + worker, "hostNetwork": True,
                                 "automountServiceAccountToken": False,
                                 "containers": [{"name": "ovn", "image": OVN_IMAGE,
                                                 "imagePullPolicy": "Never",
                                                 "securityContext": {"privileged": True},
                                                 "readinessProbe": {"exec": {"command": [
                                                     "/bin/sh", "-ec",
                                                     "test -s /var/run/ovn/ovn-controller.pid && "
                                                     "ovs-vsctl br-exists br-int"]}},
                                                 "command": ["/bin/sh", "-ec", startup]}]}}
            path = self.directory / f"{pod}.json"
            lab.write(path, manifest)
            self.k("apply", "-f", str(path))
            self.k("-n", "geneve-test", "wait", "--for=condition=Ready", f"pod/{pod}",
                   "--timeout=120s")
        identities = {}
        for side in ("a", "b"):
            system_id = self.rt(side, "ovs-vsctl", "get", "Open_vSwitch", ".",
                                "external_ids:system-id").strip().strip('"')
            database = self.rt(side, "ovs-vsctl", "--bare", "--columns=_uuid",
                               "list", "Open_vSwitch").strip()
            pid = self.rt(side, "cat", "/var/run/ovn/ovn-controller.pid").strip()
            cmd = self.rt(side, "sh", "-ec", f"tr '\\0' ' ' < /proc/{int(pid)}/cmdline")
            require("ovn-controller" in cmd, "controller PID is not live")
            pod = json.loads(self.k("-n", "geneve-test", "get", "pod", self.pods[side],
                                    "-o", "json", name=f"{side}-runtime-pod"))
            identities[side] = {"systemId": system_id, "ovsUUID": database,
                                "controllerPID": pid, "controllerCommand": cmd,
                                "controllerStat": self.rt(side, "cat", f"/proc/{int(pid)}/stat"),
                                "nodeName": pod["spec"]["nodeName"],
                                "nodeUID": self.nodes[pod["spec"]["nodeName"]],
                                "podUID": pod["metadata"]["uid"],
                                "containerID": pod["status"]["containerStatuses"][0]["containerID"]}
            self.rt(side, "ovs-vsctl", "--version", name=f"{side}-ovs-version")
            self.rt(side, "ovn-controller", "--version", name=f"{side}-ovn-version")
            self.rt(side, "tcpdump", "--version", name=f"{side}-tcpdump-version")
            self.rt(side, "ps", "-eo", "pid,args", name=f"{side}-processes")
        require(identities["a"]["systemId"] != identities["b"]["systemId"],
                "duplicate chassis identity")
        require(identities["a"]["ovsUUID"] != identities["b"]["ovsUUID"],
                "shared OVS database")
        require(identities["a"]["containerID"] != identities["b"]["containerID"],
                "shared controller container")
        self.identities = identities
        lab.write(self.directory / "ovs-controller-identities.json", identities)

    def chassis(self):
        self.wait(lambda: all(self.sb("--bare", "--columns=_uuid", "find", "Chassis",
                                     f"name={self.identities[s]['systemId']}").strip()
                              for s in ("a", "b")), "chassis registrations absent")
        chassis = json.loads(self.sb("--format=json", "list", "Chassis", name="sb-chassis"))
        require(len(chassis["data"]) == 2, "unexpected chassis count")
        self.sb("--format=json", "list", "Encap", name="sb-encap")
        for side in ("a", "b"):
            require(self.sb("--bare", "--columns=_uuid", "find", "Encap",
                            f"chassis_name={self.identities[side]['systemId']}",
                            "type=geneve", f"ip={ENDPOINTS[side][2]}").strip(),
                    "actual Geneve Encap missing")
            self.identities[side]["chassisUUID"] = self.sb(
                "--bare", "--columns=_uuid", "find", "Chassis",
                f"name={self.identities[side]['systemId']}").strip()
        lab.write(self.directory / "ovs-controller-identities.json", self.identities)

    def endpoints(self):
        self.nb("ls-add", "ls-local")
        self.switch_uuid = self.nb("--bare", "--columns=_uuid", "find",
                                   "Logical_Switch", "name=ls-local").strip()
        self.nb("set", "Logical_Switch", self.switch_uuid,
                f"external_ids:evpn.e2e.owner={self.directory.name}")
        # A sentinel has its own exact receipt and is removed only by its fixture owner.
        self.nb("ls-add", "foreign-witness")
        self.foreign_uuid = self.nb("--bare", "--columns=_uuid", "find",
                                    "Logical_Switch", "name=foreign-witness").strip()
        self.foreign_before = self.nb("--format=json", "list", "Logical_Switch",
                                      self.foreign_uuid, name="foreign-before")
        for side, (mac, ip, _) in ENDPOINTS.items():
            port = f"local-{side}"
            self.nb("lsp-add", "ls-local", port)
            self.nb("lsp-set-addresses", port, f"{mac} {ip}")
            self.port_ids[side] = self.nb("--bare", "--columns=_uuid", "find",
                                          "Logical_Switch_Port", f"name={port}").strip()
            self.rt(side, "sh", "-ec",
                    f"ip netns add local-{side}\n"
                    f"ip link add local-{side}-host type veth peer name local-{side}-eth\n"
                    f"ip link set local-{side}-eth netns local-{side}\n"
                    f"ip link set local-{side}-host up\n"
                    f"ip netns exec local-{side} ip link set lo up\n"
                    f"ip netns exec local-{side} ip link set local-{side}-eth address {mac} mtu 1400\n"
                    f"ip netns exec local-{side} ip addr add {ip}/24 dev local-{side}-eth\n"
                    f"ip netns exec local-{side} ip link set local-{side}-eth up\n"
                    f"ovs-vsctl add-port br-int local-{side}-host -- set Interface "
                    f"local-{side}-host external_ids:iface-id={port}")
        options = self.nb("get", "Logical_Switch", self.switch_uuid, "other_config").strip()
        require(options == "{}", "ls-local unexpectedly enables dynamic routing")
        members = self.nb("get", "Logical_Switch", self.switch_uuid, "ports")
        require(all(identity in members for identity in self.port_ids.values()),
                "endpoints are not members of ls-local")
        self.nb("--format=json", "list", "Logical_Switch", self.switch_uuid, name="nb-switch")
        self.nb("--format=json", "list", "Logical_Switch_Port", name="nb-ports")
        for side in ("a", "b"):
            resolved = self.rt(side, "ovn-nbctl", "--db=tcp:198.18.30.2:6641",
                               "get", "Logical_Switch", "ls-local", "_uuid",
                               name=f"{side}-shared-switch").strip()
            require(resolved == self.switch_uuid, "worker resolved a different NB switch")
            self.rt(side, "ovn-appctl", "-t", "ovn-controller", "connection-status",
                    name=f"{side}-controller-connection")
        lab.write(self.directory / "owned-endpoints.json",
                  {"switch": self.switch_uuid, "ports": self.port_ids,
                   "foreignWitness": self.foreign_uuid})

    def bindings(self):
        for side in ("a", "b"):
            chassis = self.identities[side]["chassisUUID"]
            self.wait(lambda s=side, c=chassis: self.sb(
                "--bare", "--columns=_uuid", "find", "Port_Binding",
                f"logical_port=local-{s}", f"chassis={c}", "up=true").strip(),
                f"endpoint {side} not ready on expected chassis")
        self.sb("--format=json", "list", "Port_Binding", name="sb-bindings")
        dp_a = self.sb("get", "Port_Binding", "local-a", "datapath").strip()
        dp_b = self.sb("get", "Port_Binding", "local-b", "datapath").strip()
        require(dp_a == dp_b, "endpoints on different SB datapaths")
        external = self.sb("get", "Datapath_Binding", dp_a, "external_ids").strip()
        require(self.switch_uuid in external, "SB datapath not bound to ls-local UUID")
        self.tunnel_key = int(self.sb("get", "Datapath_Binding", dp_a, "tunnel_key").strip())
        self.sb("--format=json", "list", "Datapath_Binding", name="sb-datapaths")
        self.capture_start()

    def capture_start(self):
        for side in ("a", "b"):
            links = json.loads(self.rt(side, "ip", "-j", "-4", "addr", "show"))
            devices = [entry["ifname"] for entry in links if any(
                a.get("local") == ENDPOINTS[side][2] for a in entry.get("addr_info", []))]
            require(len(devices) == 1, "Geneve VTEP interface not unique")
            path = f"/tmp/geneve-{side}.pcap"
            pid = self.rt(side, "sh", "-ec",
                          f"tcpdump --immediate-mode -U -n -i {devices[0]} -s 0 -c 256 -w {path} "
                          "'udp and host 198.18.30.2 and host 198.18.30.3 "
                          "and (port 6081 or port 4789)' "
                          f"> /tmp/geneve-{side}.log 2>&1 </dev/null & echo $!").strip()
            self.captures[side] = int(pid)
            self.rt(side, "sh", "-ec", f"kill -0 {int(pid)}")
            lab.write(self.directory / f"capture-{side}.json",
                      {"pid": int(pid), "interface": devices[0], "path": path})
        time.sleep(2)

    def connectivity(self):
        for source, destination in (("a", "b"), ("b", "a")):
            self.rt(source, "ip", "netns", "exec", f"local-{source}", "ping",
                    "-n", "-c", "3", "-W", "3", ENDPOINTS[destination][1],
                    name=f"ping-{source}-{destination}")
        self.capture_stop()

    def capture_stop(self):
        for side, pid in list(self.captures.items()):
            cmd = self.rt(side, "sh", "-ec", f"tr '\\0' ' ' < /proc/{pid}/cmdline")
            require("tcpdump" in cmd and f"/tmp/geneve-{side}.pcap" in cmd,
                    "capture PID identity changed")
            self.rt(side, "kill", "-INT", str(pid))
            self.rt(side, "sh", "-ec",
                    f"for i in $(seq 1 20); do "
                    f"test ! -e /proc/{pid}/stat && exit 0; "
                    f"test \"$(awk '{{print $3}}' /proc/{pid}/stat)\" = Z && exit 0; "
                    "sleep 0.1; done; exit 1")
            self.captures.pop(side)
            self.k("-n", "geneve-test", "cp", f"{self.pods[side]}:/tmp/geneve-{side}.pcap",
                   str(self.directory / f"geneve-{side}.pcap"))
            self.rt(side, "cat", f"/tmp/geneve-{side}.log", name=f"capture-{side}-log")

    def packets(self):
        report = {side: decode_geneve(self.directory / f"geneve-{side}.pcap",
                                      self.tunnel_key) for side in ("a", "b")}
        lab.write(self.directory / "geneve-packet-evidence.json", report)
        self.diagnostics()

    def diagnostics(self):
        for side in self.pods:
            for name, args in (
                ("interfaces", ("ovs-vsctl", "--format=json", "list", "Interface")),
                ("flows", ("ovs-ofctl", "dump-flows", "br-int")),
                ("ports", ("ovs-vsctl", "show")),
                ("sockets", ("ss", "-lntup")),
                ("links", ("ip", "-d", "-j", "link", "show")),
                ("addresses", ("ip", "-j", "addr", "show"))):
                self.rt(side, *args, name=f"{side}-{name}")
        self.k("get", "pods", "-A", "-o", "json", name="all-pods")
        for side in self.port_ids:
            self.rt(side, "ip", "netns", "exec", f"local-{side}", "ip", "-j", "addr",
                    name=f"{side}-endpoint-addresses")
            self.rt(side, "ip", "netns", "exec", f"local-{side}", "ip", "-j", "route",
                    name=f"{side}-endpoint-routes")
            self.rt(side, "ip", "netns", "exec", f"local-{side}", "ip", "-j", "neigh",
                    name=f"{side}-endpoint-neighbors")

    def no_evpn(self):
        state = lab.load(self.directory)
        require(state["frr"] is None and not state.get("auxiliary"),
                "unexpected external containers")
        require(set(state["containers"]) == set(state["kind_nodes"]),
                "unexpected non-Kubernetes lab container")
        for side in ("a", "b"):
            interfaces = json.loads(self.rt(side, "ovs-vsctl", "--format=json",
                                           "--columns=type", "list", "Interface"))
            require(not any(row[0] == "vxlan" for row in interfaces["data"]),
                    "external OVS VXLAN present")
            processes = self.rt(side, "ps", "-eo", "pid,args")
            require(not any(name in processes for name in ("gobgpd", "/zebra", "/bgpd")),
                    "BGP process present")
            sockets = self.rt(side, "ss", "-lntup")
            require(not any(":179 " in line or ":4789 " in line
                            for line in sockets.splitlines()), "external BGP/VXLAN socket")
        lab.write(self.directory / "external-evpn-absence.json",
                  {"externalDomains": [], "bgpSpeakers": [], "type2Dependency": False,
                   "ovsVxlan": False, "scope": "A1 only; A2 re-enable/disable NOT EXECUTED"})

    def remove_endpoints(self):
        self.nb("get", "Logical_Switch", self.switch_uuid, "external_ids")
        require(self.directory.name in self.nb("get", "Logical_Switch",
                                               self.switch_uuid, "external_ids"),
                "switch ownership changed")
        for side in self.port_ids:
            require(self.nb("get", "Logical_Switch_Port", self.port_ids[side],
                            "name").strip('" \n') == f"local-{side}", "port identity changed")
            self.nb("lsp-del", self.port_ids[side])
            self.rt(side, "ovs-vsctl", "del-port", "br-int", f"local-{side}-host")
            self.rt(side, "ip", "link", "del", f"local-{side}-host")
            self.rt(side, "ip", "netns", "del", f"local-{side}")
        self.nb("ls-del", self.switch_uuid)
        self.wait(lambda: all(not self.sb("--bare", "--columns=_uuid", "find", "Port_Binding",
                                          f"logical_port=local-{s}").strip() for s in ("a", "b")),
                  "local Port_Binding remains")
        for side in self.port_ids:
            require(not self.nb("--bare", "--columns=_uuid", "find", "Logical_Switch_Port",
                                f"name=local-{side}").strip(), "NB endpoint remains")
            require(f"local-{side}" not in self.rt(side, "ip", "netns", "list"),
                    "endpoint namespace remains")
        self.sb("--format=json", "list", "Port_Binding", name="sb-bindings-cleaned")

    def foreign(self):
        after = self.nb("--format=json", "list", "Logical_Switch",
                        self.foreign_uuid, name="foreign-after")
        require(json.loads(after) == json.loads(self.foreign_before), "foreign sentinel changed")
        self.nb("ls-del", self.foreign_uuid)
        self.foreign_uuid = None


def run(directory, cluster):
    directory = directory.resolve()
    require(not directory.exists(), "evidence directory already exists")
    require(directory != lab.ROOT and lab.ROOT not in directory.parents,
            "evidence must stay outside repository")
    directory.mkdir(parents=True, mode=0o700)
    recovery = __import__("os").environ.get("EVPN_EXTERNAL_RECOVERY") == "1"
    full = recovery or __import__("os").environ.get("EVPN_EXTERNAL_GOBGP_SMOKE") == "1"
    external = full or __import__("os").environ.get("EVPN_EXTERNAL_FRR_SMOKE") == "1"
    if recovery:
        from external_recovery import RecoveryBaseline
        baseline = RecoveryBaseline(directory, cluster)
    elif full:
        from external_gobgp import ThreeDomainBaseline
        baseline = ThreeDomainBaseline(directory, cluster)
    elif external:
        from external_frr import FRRBaseline
        baseline = FRRBaseline(directory, cluster)
    else:
        baseline = Baseline(directory, cluster)
    names = tuple(baseline.cases)
    old_handlers = {}
    def interrupted(signum, _frame):
        raise lab.LabError(f"Geneve test interrupted by signal {signum}")
    for sig in (signal.SIGINT, signal.SIGTERM):
        old_handlers[sig] = signal.signal(sig, interrupted)
    with lab.exclusive():
        try:
            actions = (
                baseline.inputs, baseline.provision, baseline.ovs, baseline.chassis,
                baseline.endpoints, baseline.bindings, baseline.connectivity,
                baseline.packets, baseline.no_evpn,
            )
            if external:
                actions += baseline.external_actions()
            actions += (baseline.remove_endpoints, baseline.foreign)
            require(len(actions) == len(names) - 1, "assertion/action matrix differs")
            for name, action in zip(names[:-1], actions):
                baseline.assertion(name, action)
        except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
            baseline.cases[baseline.active] = {"status": "FAIL", "detail": str(error)}
            (directory / "first-failure.txt").write_text(str(error) + "\n")
            print(f"FAIL {baseline.active}: {error}", flush=True)
        finally:
            try:
                if (directory / "lab.json").exists():
                    state = lab.load(directory)
                    if state["phase"] not in ("reserved", "down"):
                        # The coordinator verifies exact cluster/container receipts before deletion.
                        # Failure-path endpoint/process state is confined to this disposable cluster.
                        capture_errors = []
                        if recovery:
                            for present, stop in (
                                (baseline.ext_captures, lambda: baseline.stop_external_capture(require_packets=False)),
                                (baseline.phase_capture, lambda: baseline.stop_phase(verify=False)),
                            ):
                                if present:
                                    try:
                                        stop()
                                    except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                                        capture_errors.append(str(error))
                            if capture_errors:
                                lab.write(directory / "recovery-capture-cleanup-errors.json", capture_errors)
                        elif external and baseline.ext_captures:
                            baseline.stop_external_capture(require_packets=False)
                        if baseline.cases[baseline.active]["status"] == "FAIL":
                            try:
                                if baseline.captures:
                                    baseline.capture_stop()
                                if baseline.pods:
                                    baseline.diagnostics()
                                    baseline.nb("--format=json", "list", "Logical_Switch",
                                                name="failure-nb-switches")
                                    baseline.sb("--format=json", "list", "Port_Binding",
                                                name="failure-sb-bindings")
                            except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                                (directory / "fixture-diagnostics-error.txt").write_text(str(error) + "\n")
                        try:
                            lab.collect(directory)
                        except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                            (directory / "diagnostic-collection-error.txt").write_text(str(error) + "\n")
                        lab.down(directory)
                        inventory = lab.inventory(lab.load(directory))
                        require(not inventory["containers"] and
                                not inventory["network"] and not inventory.get("geneve_network") and
                                not inventory.get("remote2_network"),
                                "owned resources leaked")
                        require(not capture_errors, "capture shutdown evidence failed; scoped lab teardown completed")
                baseline.cases["coordinator_teardown"] = {"status": "PASS"}
            except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                baseline.cases["coordinator_teardown"] = {"status": "BLOCKED",
                                                         "detail": str(error)}
                print(f"BLOCKED cleanup: {error}", flush=True)
            for sig, handler in old_handlers.items():
                signal.signal(sig, handler)
            observed = collections.Counter(c["status"] for c in baseline.cases.values())
            counts = {status: observed[status] for status in ("PASS", "FAIL", "SKIP", "BLOCKED")}
            status = "PASS" if counts["PASS"] == len(names) else "FAIL"
            if baseline.cases["coordinator_teardown"]["status"] == "BLOCKED":
                status = "BLOCKED"
            if recovery:
                baseline.finish(status)
            lab.write(directory / "result.json",
                      {"suite": "three-domain-isolation-recovery" if recovery else
                       "three-domain-external-evpn" if full else
                       "two-worker-external-frr" if external else "two-worker-ovn-geneve",
                       "status": status,
                       "cases": baseline.cases, "counts": counts,
                       "testExit": 0 if all(baseline.cases[n]["status"] == "PASS"
                                           for n in names[:-1]) else 1,
                       "cleanupExit": 0 if baseline.cases[names[-1]]["status"] == "PASS" else 1,
                       "evidenceDirectory": str(directory),
                       "A2": baseline.scenarios["A2"] if recovery else "PLANNED / NOT EXECUTED"})
            (directory / "cases.tsv").write_text("".join(
                f"{name}\t{value['status']}\n" for name, value in baseline.cases.items()))
            print(json.dumps({"status": status, "counts": counts,
                              "evidenceDirectory": str(directory)}), flush=True)
    return 0 if status == "PASS" else 1
