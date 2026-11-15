#!/usr/bin/env python3
"""Manage a separate Vagrant instance without touching the historical VM."""

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import time
import uuid
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
TEMPLATE = ROOT / "deploy/containerlab/vagrant-secondary"
VIRSH = ["virsh", "-c", "qemu:///system"]
NAME = re.compile(r"evpn-vm-[0-9a-f]{12}\Z")
INVENTORY_SCENARIO = "operator-inventory-type2-two-worker"
BOX_ARCHIVE_SHA256 = "c6252ddb99513c8a4cfc729ea496d2d793e9f7e3eb68f47504b4fa955664a399"
BOX_DISK_SHA256 = "65db5e4be7ca68a84fe29d3f356f2189bb4b8a43116c358d764a126d80f90ae4"
BOX_CACHE_PATH = Path(
    "boxes/generic-VAGRANTSLASH-ubuntu2204/4.3.12/amd64/libvirt/box.img")
KERNEL_RELEASE = "6.8.0-149-generic"
KERNEL_PACKAGE_VERSION = "6.8.0-149.149~22.04.1"
KERNEL_PACKAGES = (
    "linux-image-6.8.0-149-generic",
    "linux-modules-6.8.0-149-generic",
    "linux-modules-extra-6.8.0-149-generic",
)


class LabError(Exception):
    pass


def command(args, *, cwd=None, env=None, capture=True, check=True):
    return subprocess.run(args, cwd=cwd, env=env, check=check, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def virsh(*args):
    return command([*VIRSH, *args])


def properties(xml, name, pool_path, network):
    domain = ET.fromstring(xml)
    if domain.findtext("name") != name:
        raise LabError("domain name changed")
    devices = domain.find("devices")
    if devices is None or devices.findall("filesystem") or devices.findall("hostdev"):
        raise LabError("unexpected host filesystem or host device")
    disks = []
    for disk in devices.findall("disk"):
        if disk.get("device") != "disk" or disk.get("type") != "file":
            raise LabError("unexpected disk device")
        source = disk.find("source")
        path = source.get("file") if source is not None else None
        if not path or Path(path).parent != Path(pool_path) or not Path(path).name.startswith(name):
            raise LabError("disk is not a uniquely named file in the dedicated pool")
        disks.append(path)
    if not disks:
        raise LabError("domain has no disk")
    interfaces = devices.findall("interface")
    if len(interfaces) != 1 or interfaces[0].get("type") != "network":
        raise LabError("unexpected network interfaces")
    source = interfaces[0].find("source")
    if source is None or source.get("network") != network:
        raise LabError("unexpected network")
    return sorted(disks)


def environment(config):
    env = {key: value for key, value in os.environ.items()
           if key not in ("VAGRANT_CWD", "VAGRANT_DOTFILE_PATH", "VAGRANT_VAGRANTFILE")}
    return {**env, "EVPN_VM_NAME": config["name"],
            "EVPN_VM_POOL": config["pool"], "EVPN_VM_NETWORK": config["network"],
            "EVPN_VM_GATEWAY": config["gateway"], "EVPN_VM_NETWORK_ADDRESS": config["network_address"],
            "EVPN_VM_CPUS": str(config["cpus"]), "EVPN_VM_MEMORY_MB": str(config["memory"]),
            "EVPN_VM_DISK_GB": str(config["disk"]), "LIBVIRT_DEFAULT_URI": "qemu:///system"}


def read_config(directory):
    path = directory / "lab.json"
    if not path.is_file() or path.is_symlink():
        raise LabError("missing lab.json")
    config = json.loads(path.read_text())
    if not NAME.fullmatch(config.get("name", "")):
        raise LabError("invalid lab identity")
    return config


def checked_domain(directory, config):
    if "uuid" not in config or "disks" not in config:
        raise LabError("no completed ownership receipt; inspect manually, do not destroy")
    for name, digest in config["templates"].items():
        file = directory / name
        if file.is_symlink() or hashlib.sha256(file.read_bytes()).hexdigest() != digest:
            raise LabError(f"Vagrant configuration changed: {name}")
    machine_id = directory / ".vagrant/machines" / config["name"] / "libvirt/id"
    if not machine_id.is_file() or machine_id.read_text().strip() != config["uuid"]:
        raise LabError("Vagrant machine ID does not match the receipt")
    if virsh("domuuid", config["name"]).strip() != config["uuid"]:
        raise LabError("libvirt domain UUID does not match the receipt")
    if virsh("pool-uuid", config["pool"]).strip() != config["pool_uuid"]:
        raise LabError("libvirt pool UUID changed")
    if virsh("net-uuid", config["network"]).strip() != config["network_uuid"]:
        raise LabError("libvirt network UUID changed")
    pool = ET.fromstring(virsh("pool-dumpxml", config["pool"]))
    if pool.get("type") != "dir" or pool.findtext("target/path") != config["pool_path"]:
        raise LabError("libvirt pool configuration changed")
    network = ET.fromstring(virsh("net-dumpxml", config["network"]))
    ip, forward = network.find("ip"), network.find("forward")
    if (ip is None or ip.get("address") != config["gateway"] or
            forward is None or forward.get("mode") != "nat" or
            str(ipaddress.IPv4Network(f"{ip.get('address')}/{ip.get('netmask')}", strict=False))
            != config["network_address"]):
        raise LabError("libvirt network configuration changed")
    for domain_uuid in virsh("list", "--all", "--uuid").split():
        if domain_uuid == config["uuid"]:
            continue
        domain = ET.fromstring(virsh("dumpxml", domain_uuid))
        if any(source.get("network") == config["network"]
               for source in domain.findall("./devices/interface/source")):
            raise LabError("another VM is attached to the lab network")
    disks = properties(virsh("dumpxml", config["uuid"]), config["name"],
                       config["pool_path"], config["network"])
    if disks != config["disks"]:
        raise LabError("domain disks changed since creation")


def verify_base_box(vagrant_home=None):
    home = Path(vagrant_home or os.environ.get(
        "VAGRANT_HOME", Path.home() / ".vagrant.d")).expanduser()
    image = home / BOX_CACHE_PATH
    if image.is_symlink() or not image.is_file():
        raise LabError(f"verified Ubuntu base box cache is missing: {image}")
    with image.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    if digest != BOX_DISK_SHA256:
        raise LabError(f"cached Ubuntu base disk SHA256 mismatch: {digest}")
    return {"archiveSHA256": BOX_ARCHIVE_SHA256,
            "cachedDisk": str(image), "cachedDiskSHA256": digest}


def source_archive_provenance(archive_path, repository=None):
    repository = Path(repository or ROOT)
    if archive_path.is_symlink() or not archive_path.is_file():
        raise LabError("source archive must be a regular file")
    status = command(
        ["git", "status", "--porcelain", "--untracked-files=all"], cwd=repository)
    if status:
        raise LabError("source checkout must be clean before creating a committed-tree archive")
    commit = command(["git", "rev-parse", "HEAD"], cwd=repository).strip().lower()
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise LabError("unable to resolve source commit")
    archive = subprocess.run(
        ["git", "archive", "--format=tar", commit], cwd=repository,
        check=True, stdout=subprocess.PIPE,
    ).stdout
    expected_sha256 = hashlib.sha256(archive).hexdigest()
    with archive_path.open("rb") as stream:
        observed_sha256 = hashlib.file_digest(stream, "sha256").hexdigest()
    if observed_sha256 != expected_sha256:
        raise LabError("source archive SHA256 does not match the clean committed tree")
    with tarfile.open(archive_path, "r:") as source:
        for member in source:
            path = Path(member.name)
            if (path.is_absolute() or ".." in path.parts or
                    not (member.isfile() or member.isdir())):
                raise LabError(f"unsafe source archive entry: {member.name}")
    return {
        "commit": commit,
        "archiveSHA256": expected_sha256,
        "archiveCommand": f"git archive --format=tar {commit}",
        "archiveFormat": "tar",
    }


def inventory_kernel_install_script():
    packages = " ".join(
        f"{package}={KERNEL_PACKAGE_VERSION}" for package in KERNEL_PACKAGES)
    return f"""set -eu
apt-get update -o APT::Update::Error-Mode=any
apt-cache policy {' '.join(KERNEL_PACKAGES)}
apt-get install -y --allow-downgrades {packages}
apt-mark hold {' '.join(KERNEL_PACKAGES)}
dpkg-query -W -f='${{Package}} ${{Version}}\\n' {' '.join(KERNEL_PACKAGES)}
if [ "$(uname -r)" != "{KERNEL_RELEASE}" ]; then
    systemd-run --unit=evpn-hwe-kernel-reboot --on-active=5s /usr/bin/systemctl reboot
    echo REBOOT_SCHEDULED
else
    echo RUNNING_KERNEL_ALREADY_PINNED
fi
"""


def guest_command(directory, config, remote, *, timeout=600):
    return subprocess.run(
        ["vagrant", "ssh", config["name"], "-c", remote],
        cwd=directory, env=environment(config), text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        timeout=timeout, check=False)


def wait_for_inventory_kernel(directory, config, evidence):
    log_path = evidence / "kernel-reconnect.log"
    remote = (
        f"printf 'running-kernel=%s\\n' \"$(uname -r)\"; "
        f"test \"$(uname -r)\" = {shlex.quote(KERNEL_RELEASE)} && "
        "echo EVPN_GUEST_KERNEL_VERIFIED")
    deadline = time.monotonic() + 600
    with log_path.open("w") as log:
        while time.monotonic() < deadline:
            try:
                check = guest_command(
                    directory, config, remote,
                    timeout=min(30, max(1, int(deadline - time.monotonic()))))
            except subprocess.TimeoutExpired as error:
                output = error.stdout or ""
                if isinstance(output, bytes):
                    output = output.decode(errors="replace")
                log.write(f"SSH attempt timed out\n{output}\n")
                log.flush()
            else:
                log.write(f"SSH attempt exit={check.returncode}\n{check.stdout}\n")
                log.flush()
                if (check.returncode == 0 and
                        "EVPN_GUEST_KERNEL_VERIFIED" in check.stdout):
                    return
            time.sleep(min(5, max(0, deadline - time.monotonic())))
    raise LabError(f"guest did not boot the pinned kernel; see {log_path}")


def peerpidfd_probe_result(output):
    results = []
    for line in output.splitlines():
        try:
            candidate = json.loads(line)
        except ValueError:
            continue
        if (isinstance(candidate, dict) and
                candidate.get("operation") == "getsockopt(SOL_SOCKET, SO_PEERPIDFD)"):
            results.append(candidate)
    if len(results) != 1:
        raise LabError("SO_PEERPIDFD probe did not emit exactly one JSON result")
    return results[0]


def evidence_collection_command():
    pipeline = (
        "find /tmp -maxdepth 1 -type d "
        "\\( -name 'cni-sec-*-evidence' -o -name 'evpn-vm-run-*' \\) "
        "-printf '%f\\0' | tar -C /tmp --null -T - -cf -")
    return "sudo bash -ec " + shlex.quote(pipeline)


def ensure_inventory_kernel(directory, config):
    evidence = directory / "kernel-prerequisite"
    evidence.mkdir(mode=0o700, exist_ok=True)
    install = guest_command(
        directory, config, "sudo bash -ec " + shlex.quote(inventory_kernel_install_script()),
        timeout=1800)
    (evidence / "install-and-reboot.log").write_text(install.stdout)
    if install.returncode:
        raise LabError(f"pinned signed HWE kernel installation failed; see {evidence / 'install-and-reboot.log'}")
    if "REBOOT_SCHEDULED" in install.stdout:
        wait_for_inventory_kernel(directory, config, evidence)
    verify_script = (
        "set -eu; "
        f"test \"$(uname -r)\" = {shlex.quote(KERNEL_RELEASE)}; "
        "for package in " + " ".join(KERNEL_PACKAGES) + "; do "
        f"test \"$(dpkg-query -W -f='${{Version}}' \"$package\")\" = {shlex.quote(KERNEL_PACKAGE_VERSION)}; "
        "apt-mark showhold | grep -Fx \"$package\" >/dev/null; "
        "done; dpkg-query -W -f='${Package} ${Version}\\n' " +
        " ".join(KERNEL_PACKAGES))
    verify = guest_command(directory, config, "sudo bash -ec " + shlex.quote(verify_script))
    (evidence / "post-boot-kernel.txt").write_text(verify.stdout)
    if verify.returncode:
        raise LabError(f"running kernel or package pins do not match; see {evidence / 'post-boot-kernel.txt'}")
    probe = guest_command(
        directory, config,
        "set -eu; cd ~/evpn-for-ovn; umask 077; "
        "go build -o ~/evpn-trust/peerpidfd-probe ./tools/e2e/peerpidfdprobe; "
        "~/evpn-trust/peerpidfd-probe",
        timeout=900)
    probe_log = evidence / "guest-socket-probe.log"
    probe_json = evidence / "guest-socket-probe.json"
    probe_log.write_text(probe.stdout)
    if probe.returncode:
        raise LabError(f"exact guest SO_PEERPIDFD socket probe failed; see {probe_log}")
    try:
        observed = peerpidfd_probe_result(probe.stdout)
    except LabError as error:
        raise LabError(f"{error}; see {probe_log}") from error
    probe_json.write_text(json.dumps(observed, sort_keys=True) + "\n")
    if (observed.get("operation") != "getsockopt(SOL_SOCKET, SO_PEERPIDFD)" or
            observed.get("socket") != "AF_UNIX/SOCK_STREAM" or
            observed.get("descriptor") != "anon_inode:[pidfd]" or
            not isinstance(observed.get("peerPidFD"), int) or observed["peerPidFD"] < 0):
        raise LabError("guest SO_PEERPIDFD probe did not verify the required socket operation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True, help="private, dedicated lab directory")
    subs = parser.add_subparsers(dest="action", required=True)
    create = subs.add_parser("create")
    create.add_argument("--pool", required=True, help="existing dedicated active libvirt directory pool")
    create.add_argument("--network", required=True, help="existing dedicated active libvirt NAT network")
    create.add_argument("--cpus", type=int, default=4)
    create.add_argument("--memory-mb", type=int, default=12288)
    create.add_argument("--disk-gb", type=int, default=128)
    for action in ("status", "ssh", "start", "stop", "destroy"):
        subs.add_parser(action)
    source = subs.add_parser("source")
    source.add_argument("--archive", required=True, type=Path)
    image = subs.add_parser("image")
    image.add_argument("--archive", required=True, type=Path)
    ctr = subs.add_parser("ctr")
    ctr.add_argument("--file", required=True, type=Path)
    ctr.add_argument("--sha256", required=True)
    gobgpd = subs.add_parser("gobgpd")
    gobgpd.add_argument("--file", required=True, type=Path)
    gobgpd.add_argument("--sha256", required=True)
    run = subs.add_parser("run")
    run.add_argument("scenario", choices=("baseline", "continuity", "both", INVENTORY_SCENARIO))
    run.add_argument("--ctr-sha256")
    run.add_argument("--gobgpd-sha256")
    collect = subs.add_parser("collect")
    collect.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    directory = args.directory.absolute()

    if args.action == "create":
        if args.cpus < 4 or args.memory_mb < 12288 or args.disk_gb < 128:
            raise LabError("resource values are below the historical working configuration")
        if not re.fullmatch(r"[a-zA-Z0-9_.-]+", args.pool) or not re.fullmatch(r"[a-zA-Z0-9_.-]+", args.network):
            raise LabError("invalid pool or network name")
        if args.pool == "default" or args.network in ("default", "vagrant-libvirt"):
            raise LabError("default pools and networks are not dedicated resources")
        if args.pool == "evpn-cni-e2e-709bc99" or args.network == "evpn-cni-e2e-nat":
            raise LabError("historical VM resources are not available to this lab")
        boxes = command(["vagrant", "box", "list"])
        if not re.search(r"^generic/ubuntu2204\s+\(libvirt,\s*4\.3\.12(?:,|\))", boxes, re.M):
            raise LabError("import the independently verified generic/ubuntu2204 4.3.12 libvirt box first")
        verify_base_box()
        pool = ET.fromstring(virsh("pool-dumpxml", args.pool))
        pool_path = pool.findtext("target/path")
        if pool.get("type") != "dir" or not pool_path or not os.path.isabs(pool_path):
            raise LabError("pool must be a dedicated directory pool")
        if "running" not in virsh("pool-info", args.pool):
            raise LabError("pool is not active")
        network = ET.fromstring(virsh("net-dumpxml", args.network))
        ip = network.find("ip")
        forward = network.find("forward")
        if (ip is None or not ip.get("address") or not ip.get("netmask") or
                forward is None or forward.get("mode") != "nat" or
                not re.search(r"^Active:\s+yes\s*$", virsh("net-info", args.network), re.M)):
            raise LabError("network must be active IPv4 NAT with an address and netmask")
        subnet = ipaddress.IPv4Network(f"{ip.get('address')}/{ip.get('netmask')}", strict=False)
        if not subnet.is_private or subnet.prefixlen != 24:
            raise LabError("network must be a private IPv4 /24")
        for domain_uuid in virsh("list", "--all", "--uuid").split():
            domain = ET.fromstring(virsh("dumpxml", domain_uuid))
            for interface in domain.findall("./devices/interface"):
                source = interface.find("source")
                if source is not None and source.get("network") == args.network:
                    raise LabError("network is already attached to another VM")
            for source in domain.findall("./devices/disk/source"):
                disk = source.get("file")
                if disk and Path(disk).resolve().is_relative_to(Path(pool_path).resolve()):
                    raise LabError("pool directory contains a disk attached to another VM")
        # A pool shared with another domain is not a disposable storage boundary.
        if virsh("vol-list", args.pool).strip().splitlines()[2:]:
            raise LabError("dedicated pool must be empty")
        name = "evpn-vm-" + uuid.uuid4().hex[:12]
        if subprocess.run([*VIRSH, "domuuid", name], stdout=subprocess.DEVNULL,
                          stderr=subprocess.DEVNULL).returncode == 0:
            raise LabError("generated domain name already exists")
        directory.mkdir(mode=0o700, parents=False, exist_ok=False)
        config = {"name": name, "pool": args.pool, "pool_path": pool_path,
                  "pool_uuid": virsh("pool-uuid", args.pool).strip(),
                  "network_uuid": virsh("net-uuid", args.network).strip(),
                  "gateway": ip.get("address"), "network_address": str(subnet),
                  "network": args.network, "cpus": args.cpus,
                  "memory": args.memory_mb, "disk": args.disk_gb}
        (directory / "lab.json").write_text(json.dumps(config, indent=2) + "\n")
        for file in ("Vagrantfile", "provision.sh"):
            shutil.copyfile(TEMPLATE / file, directory / file)
        config["templates"] = {
            file: hashlib.sha256((directory / file).read_bytes()).hexdigest()
            for file in ("Vagrantfile", "provision.sh")
        }
        (directory / "lab.json").write_text(json.dumps(config, indent=2) + "\n")
        command(["vagrant", "up", "--provider=libvirt"], cwd=directory,
                env=environment(config), capture=False)
        machine_id = directory / ".vagrant/machines" / name / "libvirt/id"
        config["uuid"] = machine_id.read_text().strip()
        uuid.UUID(config["uuid"])
        if virsh("domuuid", name).strip() != config["uuid"]:
            raise LabError("created domain UUID mismatch")
        config["disks"] = properties(virsh("dumpxml", config["uuid"]), name,
                                     pool_path, args.network)
        (directory / "lab.json").write_text(json.dumps(config, indent=2) + "\n")
        print(f"Created {name} ({config['uuid']}); receipt: {directory / 'lab.json'}")
        return

    config = read_config(directory)
    checked_domain(directory, config)
    if args.action in ("source", "image", "ctr", "gobgpd", "run", "collect"):
        ssh = ["vagrant", "ssh", config["name"], "-c"]
        env = environment(config)
        if args.action == "source":
            source = source_archive_provenance(args.archive)
            source_json = json.dumps(source, sort_keys=True, separators=(",", ":"))
            remote = (
                "set -eu; umask 077; "
                "archive=\"$HOME/evpn-for-ovn-source.tar\"; "
                "receipt=\"$HOME/evpn-for-ovn-source-provenance.json\"; "
                "test ! -e \"$HOME/evpn-for-ovn\" && test ! -e \"$archive\" && "
                "test ! -e \"$receipt\"; "
                "cat > \"$archive\"; chmod 600 \"$archive\"; "
                f"printf '%s  %s\\n' {shlex.quote(source['archiveSHA256'])} \"$archive\" | sha256sum -c; "
                "mkdir \"$HOME/evpn-for-ovn\"; "
                "tar -xf \"$archive\" -C \"$HOME/evpn-for-ovn\"; "
                f"printf '%s\\n' {shlex.quote(source_json)} > \"$receipt\"; chmod 600 \"$receipt\""
            )
            with args.archive.open("rb") as stream:
                subprocess.run([*ssh, remote, "--", "-T"], cwd=directory, env=env, stdin=stream, check=True)
            config["source"] = source
            (directory / "lab.json").write_text(json.dumps(config, indent=2) + "\n")
            return
        if args.action == "image":
            with args.archive.open("rb") as stream:
                subprocess.run([*ssh, "docker load", "--", "-T"], cwd=directory, env=env, stdin=stream, check=True)
            return
        if args.action in ("ctr", "gobgpd"):
            kind = args.action
            if not re.fullmatch(r"[0-9a-f]{64}", args.sha256):
                raise LabError(f"{kind} pin must be a lowercase SHA256")
            if (args.file.is_symlink() or not args.file.is_file() or
                    not os.access(args.file, os.X_OK) or
                    hashlib.sha256(args.file.read_bytes()).hexdigest() != args.sha256):
                raise LabError(f"{kind} does not match the administrator pin")
            destination = f"~/evpn-trust/{kind}"
            remote = ("umask 077; mkdir -p ~/evpn-trust; "
                      f"test ! -e {destination} && cat > {destination} && "
                      f"chmod 700 {destination} && "
                      f'echo "{args.sha256}  $HOME/evpn-trust/{kind}" | sha256sum -c')
            with args.file.open("rb") as stream:
                subprocess.run([*ssh, remote, "--", "-T"], cwd=directory, env=env, stdin=stream, check=True)
            return
        if args.action == "run":
            needs_ctr = args.scenario in ("continuity", "both", INVENTORY_SCENARIO)
            if needs_ctr and not re.fullmatch(r"[0-9a-f]{64}", args.ctr_sha256 or ""):
                raise LabError(f"{args.scenario} requires an explicit --ctr-sha256")
            if not needs_ctr and args.ctr_sha256:
                raise LabError(f"{args.scenario} does not use a ctr pin")
            if args.scenario == INVENTORY_SCENARIO:
                if not re.fullmatch(r"[0-9a-f]{64}", args.gobgpd_sha256 or ""):
                    raise LabError(f"{args.scenario} requires an explicit --gobgpd-sha256")
                source = config.get("source") or {}
                if not re.fullmatch(r"[0-9a-f]{40}", str(source.get("commit", "")).lower()):
                    raise LabError("inventory scenario requires source commit provenance from vm-e2e-lab source")
                if not re.fullmatch(r"[0-9a-f]{64}", str(source.get("archiveSHA256", "")).lower()):
                    raise LabError("inventory scenario requires source archive SHA256 provenance from vm-e2e-lab source")
                if source.get("archiveFormat") != "tar" or source.get("archiveCommand") != (
                        f"git archive --format=tar {str(source['commit']).lower()}"):
                    raise LabError("inventory scenario source archive provenance is inconsistent")
                ensure_inventory_kernel(directory, config)
            elif args.gobgpd_sha256:
                raise LabError("--gobgpd-sha256 is only valid for the inventory scenario")
            values = [config["uuid"], args.scenario, args.ctr_sha256 or "-",
                      args.gobgpd_sha256 or "-"]
            prefix = ""
            if args.scenario == INVENTORY_SCENARIO:
                source = config["source"]
                prefix = (
                    "EVPN_SOURCE_COMMIT=" + shlex.quote(str(source["commit"]).lower()) + " "
                    "EVPN_SOURCE_ARCHIVE_SHA256=" + shlex.quote(str(source["archiveSHA256"]).lower()) + " "
                    "EVPN_SOURCE_ARCHIVE_FORMAT=" + shlex.quote(str(source["archiveFormat"])) + " "
                    "EVPN_SOURCE_ARCHIVE_COMMAND=" + shlex.quote(
                        str(source["archiveCommand"])
                    ) + " "
                )
            remote = prefix + "bash ~/evpn-for-ovn/tools/e2e/vm_lab_guest.sh " + " ".join(map(shlex.quote, values))
            command([*ssh, remote], cwd=directory, env=env, capture=False)
            return
        # Evidence can contain credentials and packet payloads; create it privately and exclusively.
        fd = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        try:
            with os.fdopen(fd, "wb") as stream:
                remote = evidence_collection_command()
                subprocess.run([*ssh, remote, "--", "-T"], cwd=directory, env=env,
                               stdout=stream, check=True)
        except Exception:
            args.output.unlink(missing_ok=True)
            raise
        return
    action = {"status": "status", "ssh": "ssh", "start": "up",
              "stop": "halt", "destroy": "destroy"}[args.action]
    cmd = ["vagrant", action, config["name"]]
    if args.action == "destroy":
        cmd.append("--force")
    if args.action == "start":
        cmd.append("--no-provision")
    command(cmd, cwd=directory, env=environment(config), capture=False)


if __name__ == "__main__":
    try:
        main()
    except (LabError, OSError, ValueError, ET.ParseError, subprocess.CalledProcessError) as error:
        print(f"vm-e2e-lab: {error}", file=sys.stderr)
        sys.exit(1)
