#!/usr/bin/env python3
"""Containerlab lifecycle for disposable EVPN test environments."""

import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
FRR_IMAGE = "quay.io/frrouting/frr:10.7.1@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2"
NODE_IMAGE = "kindest/node:v1.32.3@sha256:b36e76b4ad37b88539ce5e07425f77b29f73a8eaaebf3f1a8bc9c764401d118c"
PROFILES = ("framework-netlink", "framework-ovsdb", "cni", "type2")
STATE_ROOT = Path.home() / ".local/state/evpn-for-ovn/containerlab"


class LabError(RuntimeError):
    pass


def command(args, *, check=True, env=None, timeout=600):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, env=env,
                            pass_fds=lock_descriptors())
    if check and result.returncode:
        raise LabError(f"command failed ({result.returncode}): {' '.join(map(str, args))}\n"
                       f"{result.stdout}\n{result.stderr}")
    return result


def lock_descriptors():
    descriptor = os.environ.get("EVPN_E2E_LOCK_FD")
    return (int(descriptor),) if descriptor is not None else ()


@contextmanager
def exclusive():
    STATE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    inherited = os.environ.get("EVPN_E2E_LOCK_FD")
    lock_path = STATE_ROOT / "e2e.lock"
    with lock_path.open("a") as lock:
        descriptor = lock.fileno() if inherited is None else int(inherited)
        if os.fstat(descriptor).st_ino != lock_path.stat().st_ino:
            raise LabError("invalid inherited E2E lock")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise LabError("another Containerlab lifecycle operation is active") from exc
        os.environ["EVPN_E2E_LOCK_FD"] = str(descriptor)
        try:
            yield
        finally:
            if inherited is None:
                os.environ.pop("EVPN_E2E_LOCK_FD", None)
            else:
                os.environ["EVPN_E2E_LOCK_FD"] = inherited


def write(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def inspect(kind, name):
    result = command(["docker", kind, "inspect", name], check=False)
    if result.returncode:
        if "No such" in result.stderr or f"network {name} not found" in result.stderr:
            return None
        raise LabError(result.stderr)
    return json.loads(result.stdout)[0]


def labels(item):
    return item.get("Config", {}).get("Labels", {}) or item.get("Labels", {}) or {}


def load(directory):
    directory = Path(directory).resolve()
    state = json.loads((directory / "lab.json").read_text())
    topology = directory / "lab.clab.yaml"
    if state["directory"] != str(directory) or state["repository"] != str(ROOT):
        raise LabError("lab manifest repository/directory mismatch")
    if hashlib.sha256(topology.read_bytes()).hexdigest() != state["topology_sha256"]:
        raise LabError("lab topology changed since reservation; refusing lifecycle operations")
    return state


def inventory(state):
    containers = {}
    for name in state["containers"]:
        item = inspect("container", name)
        if item:
            if name in state["kind_nodes"]:
                if labels(item).get("io.x-k8s.kind.cluster") != state["cluster"]:
                    raise LabError(f"unexpected kind owner: {name}")
            elif (labels(item).get("evpn.e2e.owner") != state["owner"] or
                  labels(item).get("containerlab") != state["name"]):
                raise LabError(f"unexpected Containerlab owner: {name}")
            containers[name] = item["Id"]
    network = inspect("network", state["network"])
    # Containerlab 0.79 labels management networks with an empty marker, not a lab name.
    # The unique network name, absent-before reservation and exact ID receipt bind ownership.
    if network and labels(network).get("containerlab") != "":
        raise LabError("unexpected Containerlab management network owner")
    if network and [entry.get("Subnet") for entry in network["IPAM"]["Config"]
                    if ":" not in entry.get("Subnet", "")] != [state["subnet"]]:
        raise LabError("management subnet differs from reserved topology")
    if network and state["profile"] == "type2" and (
            network.get("Internal") is not True or
            labels(network).get("evpn.e2e.owner") != state["owner"]):
        raise LabError("fixture network is not the reserved internal network")
    return {"containers": containers, "network": network["Id"] if network else None}


def verify_owned(state):
    current = inventory(state)
    recorded = state["resources"]
    if any(recorded["containers"].get(name) != identity
           for name, identity in current["containers"].items()):
        raise LabError("container identity differs from ownership receipt; refusing teardown")
    if current["network"] and current["network"] != recorded["network"]:
        raise LabError("network identity differs from ownership receipt; refusing teardown")
    # Native k8s-kind deletion is cluster-scoped; reject unexpected extra members.
    if state["cluster"]:
        result = command(["docker", "ps", "-aq", "--no-trunc", "--filter",
                          f"label=io.x-k8s.kind.cluster={state['cluster']}"])
        if set(result.stdout.split()) != {
                identity for name, identity in current["containers"].items()
                if name in state["kind_nodes"]}:
            raise LabError("kind cluster has unexpected members; refusing teardown")
    network = inspect("network", state["network"])
    if network and not set(network.get("Containers", {})).issubset(current["containers"].values()):
        raise LabError("lab network contains an unowned container; refusing teardown")
    return current


def init(directory, profile, cluster, network, frr=None, fixture=None):
    directory = Path(directory).resolve()
    if directory == ROOT or ROOT in directory.parents:
        raise LabError("lab evidence and generated credentials must stay outside the source tree")
    if (profile not in PROFILES or not isinstance(cluster, str)
            or not re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,48}", cluster)):
        raise LabError("invalid suite profile or cluster/lab identity")
    if any(not isinstance(value, str) or not re.fullmatch(r"[a-z0-9][a-z0-9.-]{0,62}", value)
           for value in (network, *([frr] if frr else []))):
        raise LabError("invalid network/FRR identity")
    if profile.startswith("framework-") and not frr:
        raise LabError("Framework profiles require the external FRR peer")
    if profile == "type2" and not fixture:
        raise LabError("Type-2 topology must be initialized by the standalone fixture")
    if (directory / "lab.json").exists():
        raise LabError("lab manifest already exists; use status/down for recovery")
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    owner = directory.name
    name = f"evpn-{profile}-{hashlib.sha256(str(directory).encode()).hexdigest()[:12]}"
    node_labels = {"evpn.e2e.owner": owner, "evpn.e2e.profile": profile}
    nodes = {}
    kind_nodes = []
    containers = []
    if profile != "type2":
        kind_nodes = [cluster + "-control-plane", cluster + "-worker"]
        nodes[cluster] = {
            "kind": "k8s-kind", "image": NODE_IMAGE,
            "startup-config": str(ROOT / "deploy/containerlab/topologies/kind.yaml"),
            "extras": {"k8s_kind": {"deploy": {
                "kubeconfig": str(directory / "kubeconfig"), "wait": "180s"}}},
        }
        containers.extend(kind_nodes)
    subnet = "198.18.0.0/24" if frr else None
    if frr:
        for source, target in (("daemons", "frr-daemons"), ("vtysh.conf", "frr-vtysh.conf")):
            (directory / target).write_bytes((ROOT / "frr" / source).read_bytes())
        (directory / "frr.conf").write_text("frr defaults traditional\nhostname frr\n")
        nodes[frr] = {
            "kind": "linux", "image": FRR_IMAGE, "mgmt-ipv4": "198.18.0.3",
            "privileged": False,
            "certificate": {"issue": True},
            "cap-add": ["NET_ADMIN", "NET_RAW", "SYS_ADMIN"],
            "security-opts": ["seccomp=unconfined", "apparmor=unconfined"],
            "labels": {**node_labels, "evpn4ovn.test-run": cluster},
            "binds": [f"{directory / source}:/etc/frr/{target}:ro"
                      for source, target in (("frr-daemons", "daemons"),
                                             ("frr-vtysh.conf", "vtysh.conf"),
                                             ("frr.conf", "frr.conf"))],
        }
        containers.append(frr)
    if fixture:
        subnet = fixture["subnet"]
        for service in ("gobgp", "frr"):
            container = f"{name}-{service}"
            node = {"kind": "linux", "image": fixture["images"][service],
                    "privileged": False,
                    "certificate": {"issue": True},
                    "mgmt-ipv4": fixture[f"{service}_address"],
                    "labels": {**node_labels, "evpn.e2e.owner": fixture["run_id"],
                               "evpn.e2e.fixture": "type2", "evpn.e2e.service": service}}
            if service == "gobgp":
                node.update({"entrypoint": "/usr/local/bin/gobgpd",
                             "cmd": "--config-file /etc/gobgp/gobgp.toml --config-type toml "
                                    "--api-hosts 0.0.0.0:50051 --api-insecure-allow-remote",
                             "binds": [f"{directory / 'gobgp.toml'}:/etc/gobgp/gobgp.toml:ro"]})
            else:
                node["cap-add"] = ["NET_ADMIN", "SYS_ADMIN"]
                node["binds"] = [f"{directory / source}:/etc/frr/{target}:ro" for source, target in
                                 (("frr.conf", "frr.conf"), ("daemons", "daemons"),
                                  ("vtysh.conf", "vtysh.conf"))]
            nodes[container] = node
            containers.append(container)
        owner = fixture["run_id"]
    management = {"network": network, "ipv4-subnet": subnet or "172.30.240.0/24",
                  "ipv6-subnet": ""}
    if fixture:
        management["external-access"] = False
    topology = {"name": name, "prefix": "", "mgmt": management,
                "topology": {"nodes": nodes}}
    write(directory / "lab.clab.yaml", topology)
    state = {"schema": 1, "repository": str(ROOT), "directory": str(directory),
             "name": name, "owner": owner, "profile": profile,
             "cluster": cluster if kind_nodes else None, "kind_nodes": kind_nodes,
             "containers": containers, "network": network, "subnet": management["ipv4-subnet"],
             "frr": frr, "phase": "reserved", "resources": {"containers": {}, "network": None},
             "topology_sha256": hashlib.sha256((directory / "lab.clab.yaml").read_bytes()).hexdigest()}
    write(directory / "lab.json", state)
    return state


def tls_env(directory):
    tls = STATE_ROOT / "tls"
    command(["bash", str(ROOT / "tools/generate-lab-certs.sh"), "--output-dir", str(tls)])
    env = dict(os.environ, CLAB_CA_CERT_FILE=str(tls / "ca.pem"),
               CLAB_CA_KEY_FILE=str(tls / "ca.key"))
    return env, tls


def lease(directory, *, release=False):
    """Keep retained/interrupted labs from overlapping another suite's inventories."""
    path = STATE_ROOT / "active.json"
    if path.exists():
        active = json.loads(path.read_text())
        if active["directory"] != str(directory):
            raise LabError(f"another E2E lab is reserved: {active['directory']}; use its down command")
    if release:
        path.unlink(missing_ok=True)
    else:
        write(path, {"directory": str(directory)})


def up(directory):
    directory = Path(directory).resolve()
    state = load(directory)
    if state["phase"] != "reserved":
        raise LabError("lab was already started; inspect/down before creating a new run")
    version = command(["containerlab", "version"]).stdout
    if not re.search(r"version:\s+0\.79\.0\b", version):
        raise LabError("validated Containerlab 0.79.0 is required")
    (directory / "containerlab-version.txt").write_text(version)
    for name in state["containers"]:
        if inspect("container", name):
            raise LabError(f"refusing pre-existing container {name}")
    if inspect("network", state["network"]):
        raise LabError("refusing pre-existing management network")
    for identity in command(["docker", "network", "ls", "-q"]).stdout.split():
        network = inspect("network", identity)
        for config in network.get("IPAM", {}).get("Config", []) or []:
            subnet = config.get("Subnet")
            if subnet and ":" not in subnet and ipaddress.ip_network(subnet).overlaps(
                    ipaddress.ip_network(state["subnet"])):
                raise LabError(f"lab subnet overlaps existing network {network['Name']}")
    env, tls = tls_env(directory)
    env["KUBECONFIG"] = str(directory / "kubeconfig")
    lease(directory)
    state["phase"] = "deploying"
    write(directory / "lab.json", state)
    try:
        if state["profile"] == "type2":
            # Containerlab 0.79 hardcodes Internal=false when creating a bridge.
            # Reserve an explicitly owned internal network for its Linux nodes.
            command(["docker", "network", "create", "--internal", "--subnet", state["subnet"],
                     "--label", "containerlab=", "--label", f"evpn.e2e.owner={state['owner']}",
                     state["network"]])
        result = command(["containerlab", "deploy", "-t", str(directory / "lab.clab.yaml"),
                          "--skip-labdir-acl"], check=False, env=env)
        (directory / "containerlab-deploy.log").write_text(result.stdout + result.stderr)
    finally:
        # Record partial native-kind and Linux creation even when deploy fails.
        state["resources"] = inventory(state)
        write(directory / "lab.json", state)
    if result.returncode:
        raise LabError(f"Containerlab deploy failed; see {directory / 'containerlab-deploy.log'}")
    if set(state["resources"]["containers"]) != set(state["containers"]):
        raise LabError("Containerlab deployment is incomplete; required nodes are missing")
    for name in state["containers"]:
        if inspect("container", name).get("State", {}).get("Running") is not True:
            raise LabError(f"Containerlab node is not running: {name}")
    if state["cluster"]:
        # Privileged Containerlab's native kind provider writes a root-owned
        # kubeconfig. Export through kind as the invoking user without sudo.
        kubeconfig = command(["kind", "get", "kubeconfig", "--name", state["cluster"]]).stdout
        path = directory / "kubeconfig"
        path.unlink(missing_ok=True)
        with os.fdopen(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "w") as stream:
            stream.write(kubeconfig)
    if state["frr"]:
        command(["docker", "network", "connect", "--ip", "198.18.0.2",
                 state["network"], state["cluster"] + "-worker"])
    # External CA files stay outside the lab directory. Verify the actual node
    # certificates generated by Containerlab, not a presumed copy of the CA.
    pki = directory / f"clab-{state['name']}" / ".tls"
    if state["frr"] or state["profile"] == "type2":
        ca = tls / "ca.pem"
        for name in set(state["containers"]) - set(state["kind_nodes"]):
            certificate = pki / name / f"{name}.pem"
            key = pki / name / f"{name}.key"
            key.chmod(0o600)
            command(["openssl", "verify", "-purpose", "sslserver", "-verify_hostname", name,
                     "-CAfile", str(ca), str(certificate)])
        (directory / "containerlab-ca-verification.txt").write_text(
            "management-ca=PASS\nsha256=" + hashlib.sha256(ca.read_bytes()).hexdigest() + "\n"
            "framework-grpc-tls=NOT_ENABLED\n")
    else:
        (directory / "containerlab-ca-verification.txt").write_text(
            "management-ca=NOT_USED (kind manages Kubernetes PKI)\n"
            "framework-grpc-tls=NOT_ENABLED\n")
    state["phase"] = "ready"
    write(directory / "lab.json", state)
    verify_owned(state)
    collect(directory)
    print(f"containerlab-ready={state['name']} topology={directory / 'lab.clab.yaml'}")


def collect(directory):
    directory = Path(directory).resolve()
    state = load(directory)
    current = verify_owned(state)
    write(directory / "lab-resources.json", current)
    for name in current["containers"]:
        result = command(["docker", "logs", "--tail", "300", name], check=False)
        (directory / f"lab-{name}.log").write_text(result.stdout + result.stderr)
    result = command(["containerlab", "inspect", "-t", str(directory / "lab.clab.yaml"),
                      "--format", "json"], check=False)
    (directory / "containerlab-inspect.json").write_text(result.stdout)
    if result.returncode:
        raise LabError(result.stderr)
    print(json.dumps({"phase": state["phase"], "name": state["name"], **current}))


def register_auxiliary(directory, identity):
    """Record a suite-owned test process container before it joins the lab."""
    state = load(directory)
    lease(Path(directory).resolve())
    item = inspect("container", identity)
    if not item or (labels(item).get("containerlab"), labels(item).get("evpn.e2e.owner")) != (
            state["name"], state["owner"]):
        raise LabError("auxiliary container has no matching lab/run ownership labels")
    name = item["Name"].lstrip("/")
    if name in state["containers"]:
        if state["resources"]["containers"].get(name) != item["Id"]:
            raise LabError("auxiliary container identity changed")
        return
    state["containers"].append(name)
    state.setdefault("auxiliary", []).append(name)
    state["resources"]["containers"][name] = item["Id"]
    verify_owned(state)
    write(Path(directory) / "lab.json", state)


def down(directory):
    directory = Path(directory).resolve()
    state = load(directory)
    if state["phase"] == "reserved":
        # A refused preflight never owns existing resources.
        return
    lease(directory)
    current = verify_owned(state)
    for name in state.get("auxiliary", []):
        if name in current["containers"]:
            command(["docker", "rm", "-f", current["containers"][name]])
    if current["containers"] or current["network"]:
        result = command(["containerlab", "destroy", "-t", str(directory / "lab.clab.yaml")],
                         check=False, env=dict(os.environ, KUBECONFIG=str(directory / "kubeconfig")))
        (directory / "containerlab-destroy.log").write_text(result.stdout + result.stderr)
        if result.returncode:
            raise LabError(f"Containerlab destroy failed: {result.stderr}")
    if inventory(state)["containers"] or inventory(state)["network"]:
        raise LabError("owned resources remain after Containerlab teardown")
    state["phase"] = "down"
    write(directory / "lab.json", state)
    lease(directory, release=True)
    print(f"containerlab-cleanup=PASS lab={state['name']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("up", "status", "collect", "down"))
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--profile", choices=PROFILES)
    parser.add_argument("--cluster")
    parser.add_argument("--network")
    parser.add_argument("--frr")
    args = parser.parse_args()
    with exclusive():
        if args.action == "up":
            if not (args.directory / "lab.json").exists():
                init(args.directory, args.profile, args.cluster, args.network, args.frr)
            else:
                state = load(args.directory)
                requested_cluster = args.cluster if args.profile != "type2" else None
                if (state["profile"], state["cluster"], state["network"], state["frr"]) != (
                        args.profile, requested_cluster, args.network, args.frr):
                    raise LabError("existing lab reservation differs from requested profile")
            up(args.directory)
        elif args.action == "down":
            down(args.directory)
        else:
            collect(args.directory)


if __name__ == "__main__":
    def interrupted(signum, _frame):
        raise LabError(f"lifecycle interrupted by signal {signum}; run down for this directory")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        main()
    except (LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        print(f"Containerlab: {error}", file=sys.stderr)
        sys.exit(1)
