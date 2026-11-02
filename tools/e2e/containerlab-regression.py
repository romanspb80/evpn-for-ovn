#!/usr/bin/env python3
"""Live, disposable partial-bootstrap and unrelated-lab isolation regression."""
import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import sys
import uuid

import containerlab as lab


ALPINE = "alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f"


def snapshot(name, network):
    item = lab.inspect("container", name)
    bridge = lab.inspect("network", network)
    if not item or not bridge:
        raise lab.LabError("isolation witness is missing")
    return {"container": item["Id"], "network": bridge["Id"],
            "labels": lab.labels(item), "running": item["State"]["Running"],
            "members": bridge.get("Containers", {}),
            "attachments": item["NetworkSettings"]["Networks"]}


def regression(directory):
    directory = directory.resolve()
    if directory == lab.ROOT or lab.ROOT in directory.parents:
        raise lab.LabError("regression evidence must be outside source")
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    suffix = uuid.uuid4().hex[:10]
    witness = f"evpn-isolation-{suffix}"
    network = witness + "-mgmt"
    subnet = ipaddress.ip_network("172.30.239.0/24")
    for identity in lab.command(["docker", "network", "ls", "-q"]).stdout.split():
        existing = lab.inspect("network", identity)
        if any(ipaddress.ip_network(config["Subnet"]).overlaps(subnet)
               for config in existing["IPAM"].get("Config", []) or []
               if config.get("Subnet") and ":" not in config["Subnet"]):
            raise lab.LabError("isolation witness subnet overlaps an existing network")
    if lab.inspect("container", witness) or lab.inspect("network", network):
        raise lab.LabError("isolation witness name collision")
    topology = directory / "witness.clab.yaml"
    lab.write(topology, {
        "name": witness, "prefix": "",
        "mgmt": {"network": network, "ipv4-subnet": str(subnet), "ipv6-subnet": ""},
        "topology": {"nodes": {witness: {
            "kind": "linux", "image": ALPINE, "cmd": "sleep infinity",
            "certificate": {"issue": True},
            "labels": {"evpn.e2e.owner": suffix},
        }}},
    })
    env, _ = lab.tls_env(directory)
    receipt = None
    target = directory / "partial"
    report = {"isolation": "NOT_RUN", "partial_bootstrap": "NOT_RUN",
              "owned_cleanup": "NOT_RUN", "witness_cleanup": "NOT_RUN"}
    try:
        try:
            result = lab.command(["containerlab", "deploy", "-t", str(topology),
                                  "--skip-labdir-acl"], env=env, check=False)
            (directory / "witness-deploy.log").write_text(result.stdout + result.stderr)
        finally:
            item = lab.inspect("container", witness)
            bridge = lab.inspect("network", network)
            receipt = {"container": item["Id"] if item else None,
                       "network": bridge["Id"] if bridge else None}
            lab.write(directory / "witness-receipt.json", receipt)
        if result.returncode:
            raise lab.LabError("witness bootstrap failed; see witness-deploy.log")
        before = snapshot(witness, network)
        lab.write(directory / "witness-before.json", before)
        state = lab.init(target, "framework-netlink", f"cni-24b-partial-{suffix}",
                         f"evpn-partial-{suffix}", f"evpn-partial-{suffix}-frr")
        rendered = target / "lab.clab.yaml"
        topology_document = json.loads(rendered.read_text())
        topology_document["topology"]["nodes"][state["frr"]]["entrypoint"] = (
            "/evpn-deliberate-bootstrap-failure")
        lab.write(rendered, topology_document)
        state["topology_sha256"] = hashlib.sha256(rendered.read_bytes()).hexdigest()
        lab.write(target / "lab.json", state)
        try:
            lab.up(target)
        except lab.LabError as error:
            (directory / "injected-failure.txt").write_text(str(error) + "\n")
        else:
            raise lab.LabError("deliberate bootstrap failure was not detected")
        partial = lab.load(target)
        if not partial["resources"]["containers"] or not partial["resources"]["network"]:
            raise lab.LabError("failure did not exercise partial resource creation")
        report["partial_bootstrap"] = "PASS"
        lab.collect(target)
        lab.down(target)
        if lab.inventory(lab.load(target)) != {"containers": {}, "network": None}:
            raise lab.LabError("partial-bootstrap resources remain")
        report["owned_cleanup"] = "PASS"
        after = snapshot(witness, network)
        lab.write(directory / "witness-after.json", after)
        if before != after:
            raise lab.LabError("unrelated witness changed during partial-bootstrap cleanup")
        lab.command(["docker", "exec", witness, "true"])
        report["isolation"] = "PASS"
    finally:
        if (target / "lab.json").exists() and lab.load(target)["phase"] != "down":
            lab.down(target)
        # Never destroy the witness based only on its name.
        item = lab.inspect("container", witness)
        bridge = lab.inspect("network", network)
        if item and (not receipt or item["Id"] != receipt["container"]
                     or lab.labels(item).get("containerlab") != witness
                     or lab.labels(item).get("evpn.e2e.owner") != suffix):
            raise lab.LabError("witness container ownership changed; preserving it")
        if bridge and (not receipt or bridge["Id"] != receipt["network"]
                       or lab.labels(bridge).get("containerlab") != ""
                       or not set(bridge.get("Containers", {})).issubset(
                           {receipt["container"]})):
            raise lab.LabError("witness network ownership changed; preserving it")
        result = lab.command(["containerlab", "destroy", "-t", str(topology)], check=False)
        (directory / "witness-destroy.log").write_text(result.stdout + result.stderr)
        if result.returncode or lab.inspect("container", witness) or lab.inspect("network", network):
            raise lab.LabError("owned isolation witness cleanup failed")
        report["witness_cleanup"] = "PASS"
        lab.write(directory / "report.json", report)
    print(json.dumps(report))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    args = parser.parse_args()
    with lab.exclusive():
        if (lab.STATE_ROOT / "active.json").exists():
            raise lab.LabError("a retained/interrupted E2E lab needs scoped teardown first")
        regression(args.directory)


if __name__ == "__main__":
    try:
        main()
    except (lab.LabError, OSError, ValueError) as error:
        print(f"Containerlab regression: {error}", file=sys.stderr)
        sys.exit(1)
