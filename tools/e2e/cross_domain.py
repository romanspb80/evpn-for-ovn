#!/usr/bin/env python3
"""Bounded E1/E2 acceptance; never F2/F4/H1 or full milestone acceptance."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import signal
import subprocess
import time
import uuid

import containerlab as lab
from external_frr import BASE
from external_gobgp import B_BASE, R_BASE, ThreeDomainBaseline
from external_recovery import complete_probe, rejected_issue, validate_injected_rib
from geneve_baseline import require
from remote2_restart import Remote2Restarts


CASES = tuple(f"{case}-{side}" for side in ("a", "b")
              for case in ("single-rt", "wrong-rt", "dual-rt", "unauthorized-vtep"))


def other_rt_pattern(allowed):
    require(re.fullmatch(r"[0-9]+:[0-9]+", allowed) is not None, "invalid RT policy pin")
    # RE2 has no negative lookahead. Match every string except this exact RT.
    alternatives = [f".{{0,{len(allowed) - 1}}}", allowed + ".+"]
    alternatives += [allowed[:i] + "[^" + char + "].*" for i, char in enumerate(allowed)]
    return "^(" + "|".join(alternatives) + ")$"


def strict_policy(config, allowed):
    require(config.count("\nneighbors:\n") == 1, "unexpected GoBGP config layout")
    config = config.replace("\nneighbors:\n", """
  apply-policy:
    config:
      import-policy-list: [R4-EXACT-RT]
      export-policy-list: [R4-EXACT-RT]
      default-import-policy: reject-route
      default-export-policy: reject-route
neighbors:
""")
    return config + f"""
defined-sets:
  bgp-defined-sets:
    ext-community-sets:
    - ext-community-set-name: R4-OTHER-RT
      ext-community-list: ["RT:{other_rt_pattern(allowed)}"]
    - ext-community-set-name: R4-ALLOWED-RT
      ext-community-list: ["RT:^{allowed}$"]
policy-definitions:
- name: R4-EXACT-RT
  statements:
  - name: reject-any-other-rt
    conditions:
      bgp-conditions:
        match-ext-community-set:
          ext-community-set: R4-OTHER-RT
          match-set-options: any
    actions:
      route-disposition: reject-route
  - name: require-allowed-rt
    conditions:
      bgp-conditions:
        match-ext-community-set:
          ext-community-set: R4-ALLOWED-RT
          match-set-options: any
    actions:
      route-disposition: accept-route
"""


def paths_for(rib, mac):
    return [p for group in rib.values() for p in group if
            p["nlri"].get("type") == 2 and p["nlri"]["value"].get("mac") == mac]


def assert_clean(status, mac, ip, reason=None):
    snapshot = status["Snapshot"]
    require(snapshot["Authoritative"], "incomplete authoritative snapshot")
    require(not status.get("Error"), "importer reports an error")
    require(mac not in json.dumps(status["Receipts"]) and ip not in json.dumps(status["Receipts"]),
            "injected identity acquired an ownership receipt")
    require(mac not in json.dumps(snapshot["Endpoints"]) and ip not in json.dumps(snapshot["Endpoints"]),
            "forbidden desired endpoint")
    if reason:
        require(rejected_issue(status, reason), "missing explicit VTEP authorization rejection")
    else:
        require(not snapshot["Issues"], "policy rejection reached importer validation")


def exact_withdrawal(added, deleted):
    require(added["PathUUID"] and added["PathUUID"] == deleted["PathUUID"] and
            added["Incarnation"] == deleted["Incarnation"],
            "injection cleanup changed UUID or speaker incarnation")


def assert_policy_views(rejected, accepted, injected, legitimate):
    require(injected in rejected and legitimate not in rejected and
            injected not in accepted and legitimate in accepted,
            "actual peer-policy accepted/rejected views disagree with rejection")


def assert_foreign(before, after, mac):
    for field in ("fdb", "neighbors"):
        select = lambda entries: [e for e in entries if e.get("mac", e.get("lladdr")) == mac]
        original = select(before[field])
        require(original and original == select(after[field]),
                "matching foreign kernel entry absent, changed or adopted")


def require_reachable(output):
    require(complete_probe(output), "unauthorized VTEP reachability positive control failed")


class CrossDomain(Remote2Restarts):
    def fixture(self):
        super().fixture()
        bridge = json.loads(self.fr("ip", "-j", "link", "show", "dev", "br1000",
                                    name="frr-bridge-identity"))[0]
        self.frr_bridge_mac = bridge["address"]
        # Pin before attestation/import/forwarding baselines: port removal must
        # not reselect the bridge MAC and invalidate unrelated ARP/EVPN state.
        self.fr("ip", "link", "set", "dev", "br1000", "address", bridge["address"],
                name="frr-pin-existing-bridge-mac")
        self.fr("ip", "netns", "exec", "evpnwork", "ping", "-n", "-c", "2", "-W", "2",
                "10.100.1.1", name="frr-pinned-bridge-warmup")

    def inputs(self):
        super().inputs()
        pins = json.loads((self.directory / "source-inputs.json").read_text())
        for relative in ("tools/e2e/cross_domain.py", "tools/e2e/test_cross_domain.py",
                         "docs/testing/external-evpn-fabric.md"):
            data = (lab.ROOT / relative).read_bytes()
            target = self.directory / "source" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            pins[relative] = hashlib.sha256(data).hexdigest()
        lab.write(self.directory / "source-inputs.json", pins)
        build = json.loads((self.directory / "build-inputs.json").read_text())
        build["sources"] = pins
        lab.write(self.directory / "build-inputs.json", build)

    def put(self, filename, value):
        if filename == "gobgpd.yaml":
            value = strict_policy(value, "64512:1000")
        return super().put(filename, value)

    def bput(self, filename, value, remote=False):
        if filename == "b-gobgpd.yaml" and not remote:
            value = strict_policy(value, "64513:2000")
        return super().bput(filename, value, remote)

    def local_rib(self, side, label, adjacency=False):
        base = BASE if side == "a" else B_BASE
        peer = "198.18.10.3" if side == "a" else "198.18.20.3"
        command = ["neighbor", peer, "adj-in"] if adjacency else ["global", "rib"]
        return json.loads(self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock",
                                 *command, "-a", "evpn", "-j", name=label))

    def import_status(self, side, label):
        return self.local_reader(side)("import-status.json", label)

    def fresh_import(self, side, label):
        first = self.import_status(side, label + "-import-first")
        observed = first

        def complete():
            nonlocal observed
            observed = self.import_status(side, label + "-complete-poll")
            return (observed["Time"] > first["Time"] and
                    observed["Snapshot"]["Authoritative"] and not observed.get("Error"))

        self.wait(complete, "no fresh complete authoritative importer observation")
        candidate = observed

        def stable():
            nonlocal observed
            observed = self.import_status(side, label + "-stable-poll")
            return (observed["Time"] > candidate["Time"] and
                    observed["Snapshot"]["Authoritative"] and not observed.get("Error") and
                    observed["Snapshot"] == candidate["Snapshot"])

        self.wait(stable, "fresh authoritative importer observations not stable")
        lab.write(self.directory / (label + "-import-verified.json"), observed)
        return observed

    def kernel(self, side, label):
        suffix = "33" if side == "a" else "34"
        return {
            "fdb": json.loads(self.rt(side, "bridge", "-j", "fdb", "show", "dev", "evpnvx" + suffix,
                                     name=label + "-fdb")),
            "neighbors": json.loads(self.rt(side, "ip", "-j", "neigh", "show", "dev", "evpnbr" + suffix,
                                           name=label + "-neighbors")),
        }

    def owned_state(self, label):
        self.save_state(label)
        for side in ("a", "b"):
            base = BASE if side == "a" else B_BASE
            self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock",
                    "policy", name=label + "-" + side + "-policy")
            self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock",
                    "global", "policy", "import", name=label + "-" + side + "-import-policy")
        self.vty("show running-config", name=label + "-frr-policy")
        self.foreign_kernel()

    def prepare(self):
        self.command(["docker", "cp", str(self.directory / "build/inject"),
                      f"{self.remote2}:/usr/local/bin/evpn-inject"])
        self.r2("chmod", "755", "/usr/local/bin/evpn-inject")
        for side in ("a", "b"):
            prefix = "198.18.10" if side == "a" else "198.18.20"
            run = self.fr if side == "a" else self.r2
            run("ip", "addr", "add", prefix + ".98/24", "dev", "eth0" if side == "a" else "eth1")
            output = self.rt(side, "ping", "-n", "-c", "3", "-W", "3", prefix + ".98",
                             name=side + "-unauthorized-vtep-reachable")
            require_reachable(output)
        # Restrict FRR to IPv4 MAC/IP routes: no unmodified MAC-only companion
        # may accidentally authorize an injected MAC's FDB slot.
        self.vty("configure terminal", "ip prefix-list R4-LEGIT seq 5 permit 10.100.1.101/32",
                 "bgp extcommunity-list standard R4-LOCAL permit rt 64512:1000",
                 "route-map TYPE2-ONLY permit 10", "match ip address prefix-list R4-LEGIT", "end")
        self.wait(lambda: len(paths_for(self.local_rib("a", "a-macip-only"),
                                       "02:00:00:01:00:65")) == 1,
                  "FRR MAC-only companion not suppressed")
        self.wait(lambda: any(r["Endpoint"]["MAC"] == "02:00:00:01:00:65" and
                  r["Endpoint"]["IP"] == "10.100.1.101" and
                  all(r[n]["ACK"] and r[n]["Status"] == "confirmed" for n in ("FDB", "Neighbor"))
                  for r in self.import_status("a", "a-macip-ready")["Receipts"] or []),
                  "legitimate FRR MAC/IP receipt not confirmed after policy setup")
        self.owned_state("isolation-baseline")
        self.start_phase("isolation-baseline")
        self.probes("isolation-baseline")
        self.stop_phase()

    def frr_add(self, label, config, kind):
        endpoint = config["Endpoint"]
        ip, mac = endpoint["IPAddresses"][0], endpoint["MACAddress"]
        commands = ["configure terminal", f"ip prefix-list R4-INJECT seq 5 permit {ip}/32",
                    "route-map TYPE2-ONLY permit 5", "match evpn route-type macip",
                    "match evpn vni 1000", "match ip address prefix-list R4-INJECT"]
        if kind == "wrong-rt":
            commands += ["set extended-comm-list R4-LOCAL delete"]
        if kind in ("wrong-rt", "dual-rt"):
            commands += ["set extcommunity rt 64513:2000"]
        commands += [f"set ip next-hop {endpoint['LocalVTEP']}", "end"]
        self.vty(*commands, name=label + "-install-route-map")
        self.fr("sh", "-ec",
                "ip netns add r4inj;"
                "ip link add r4-host type veth peer name r4-work;"
                "ip link set r4-host addrgenmode none;"
                "ip link set r4-host master br1000; ip link set r4-host up;"
                "ip link set r4-work netns r4inj;"
                "ip netns exec r4inj ip link set r4-work addrgenmode none;"
                f"ip netns exec r4inj ip link set r4-work address {mac};"
                f"ip netns exec r4inj ip addr add {ip}/24 dev r4-work;"
                "ip netns exec r4inj ip link set r4-work up;"
                f"ip netns exec r4inj ip neigh add 10.100.1.1 lladdr {self.frr_bridge_mac} "
                "dev r4-work nud permanent;"
                "ip netns exec r4inj ping -n -c 2 -W 2 10.100.1.1",
                name=label + "-originate")
        self.vty("show bgp l2vpn evpn route json", name=label + "-origin-rib")
        self.vty("show bgp l2vpn evpn neighbors 198.18.10.2 advertised-routes json",
                 name=label + "-advertised-rib")
        lab.write(self.directory / (label + "-owner.json"), {
            "namespace": "r4inj", "device": "r4-host", "mac": mac, "ip": ip,
            "rd": endpoint["RouteDistinguisher"], "cleanup": "delete exact owned namespace r4inj"})

    def case(self, side, kind, index):
        label = kind + "-" + side
        prefix, domain, vni = ("198.18.10", 1, 1000) if side == "a" else ("198.18.20", 2, 2000)
        allowed, wrong = ("64512:1000", "64513:2000") if side == "a" else ("64513:2000", "64512:1000")
        mac, ip = f"02:00:00:0{domain}:ed:{index:02x}", f"10.100.{domain}.{220 + index}"
        endpoint = {"NetworkID": "r4-test", "EndpointID": str(uuid.uuid4()),
                    "Incarnation": str(uuid.uuid4()), "MACAddress": mac, "IPAddresses": [ip],
                    "VNI": vni, "RouteDistinguisher": prefix + f".3:{1000 if side == 'a' else 3200 + index}",
                    "RouteTargets": [wrong] if kind == "wrong-rt" else
                                    [allowed, wrong] if kind == "dual-rt" else [allowed],
                    "LocalVTEP": prefix + (".98" if kind == "unauthorized-vtep" else ".4"),
                    "Advertise": True}
        if side == "a":
            endpoint["RouteDistinguisher"] = "198.18.10.3:1000"
        config = {"Endpoint": endpoint, "RouterID": prefix + ".3", "ASN": 64512 if side == "a" else 64513}
        if side == "b":
            config.update({"Socket": f"unix://{R_BASE}/gobgp.sock",
                           "Attestation": f"{R_BASE}/r2-speaker.json",
                           "InstallationUID": self.installation, "NodeUID": self.r_node,
                           "State": f"{R_BASE}/{label}-journal"})
        lab.write(self.directory / (label + "-expected.json"), config)
        self.owned_state(label + "-before")
        foreign = kind == "unauthorized-vtep"
        suffix = "33" if side == "a" else "34"
        if foreign:
            self.rt(side, "bridge", "fdb", "add", mac, "dev", "evpnvx" + suffix,
                    "dst", prefix + ".99", "self", "permanent", name=label + "-foreign-fdb-create")
            self.rt(side, "ip", "neigh", "add", ip, "lladdr", mac, "dev", "evpnbr" + suffix,
                    "nud", "permanent", name=label + "-foreign-neighbor-create")
        before_kernel = self.kernel(side, label + "-before")
        self.start_phase(label)
        added = None
        if side == "a":
            self.frr_add(label, config, kind)
        else:
            self.bput(label + ".json", config, True)
            added = json.loads(self.r2("/usr/local/bin/evpn-inject", "-config",
                                     f"{R_BASE}/{label}.json", "-action", "advertise", name=label + "-add"))
            self.rread(label + "-journal/journal.json", label + "-uuid-receipt")
        self.wait(lambda: len(paths_for(self.local_rib(side, label + "-adj-in-poll", True), mac)) == 1,
                  "injected NLRI not received from actual peer")
        adjacency = self.local_rib(side, label + "-adj-in", True)
        encoded = validate_injected_rib(adjacency, config)
        lab.write(self.directory / (label + "-wire-encoding.json"), encoded)
        rejected = kind in ("wrong-rt", "dual-rt")
        self.wait(lambda: bool(paths_for(self.local_rib(side, label + "-global-poll"), mac)) != rejected,
                  "peer import-policy decision differs")
        if rejected:
            base, legitimate = (BASE, "02:00:00:01:00:65") if side == "a" else \
                               (B_BASE, "02:00:00:02:00:66")
            views = {name: self.rt(side, "gobgp", "--target", f"unix://{base}/gobgp.sock",
                                  "neighbor", config["RouterID"], name, "-a", "evpn",
                                  name=label + "-policy-" + name) for name in ("accepted", "rejected")}
            # GoBGP's JSON conversion drops Path.Filtered; the text views
            # explicitly select accepted/rejected paths using the API flag.
            assert_policy_views(views["rejected"], views["accepted"], mac, legitimate)
        status = self.fresh_import(side, label)
        if kind == "single-rt":
            self.wait(lambda: any(r["Endpoint"]["MAC"] == mac and
                      all(r[n]["ACK"] and r[n]["Status"] == "confirmed" for n in ("FDB", "Neighbor"))
                      for r in self.import_status(side, label + "-positive-poll")["Receipts"] or []),
                      "single-RT positive control not imported")
        else:
            assert_clean(status, mac, ip, "remote VTEP not allowlisted" if foreign else None)
            after_kernel = self.kernel(side, label + "-rejected")
            if foreign:
                assert_foreign(before_kernel, after_kernel, mac)
            else:
                require(mac not in json.dumps(after_kernel) and ip not in json.dumps(after_kernel),
                        "peer-rejected route created kernel state")
            flows = self.rt(side, "ovs-ofctl", "dump-flows", "br-int", name=label + "-no-forbidden-flows")
            interfaces = self.rt(side, "ovs-vsctl", "--format=json", "list", "Interface",
                                 name=label + "-no-forbidden-tunnels")
            require(mac not in flows and mac not in interfaces, "forbidden OVN forwarding state")
            if foreign:
                require(endpoint["LocalVTEP"] not in flows and endpoint["LocalVTEP"] not in interfaces,
                        "unauthorized VTEP programmed by OVN")
        self.owned_state(label + "-observed")
        self.probes(label + "-injected")
        if side == "a":
            self.fr("ip", "netns", "del", "r4inj", name=label + "-exact-withdraw")
        else:
            deleted = json.loads(self.r2("/usr/local/bin/evpn-inject", "-config",
                                       f"{R_BASE}/{label}.json", "-action", "withdraw",
                                       name=label + "-exact-withdraw"))
            exact_withdrawal(added, deleted)
            journal = self.rread(label + "-journal/journal.json", label + "-drained-uuid-receipt")
            require(not journal["Record"]["State"]["Receipts"] and journal["Record"]["Pending"] is None,
                    "injection journal not exactly drained")
        self.wait(lambda: not paths_for(self.local_rib(side, label + "-withdraw-adj", True), mac) and
                  not paths_for(self.local_rib(side, label + "-withdraw-global"), mac),
                  "exact injected NLRI remains after withdrawal")
        self.wait(lambda: mac not in json.dumps(self.import_status(side, label + "-drain")["Receipts"]),
                  "injected importer receipts not drained")
        if side == "a":
            self.vty("configure terminal", "no route-map TYPE2-ONLY permit 5",
                     "no ip prefix-list R4-INJECT", "end", name=label + "-policy-cleanup")
        if foreign:
            final_kernel = self.kernel(side, label + "-foreign-survived-withdrawal")
            assert_foreign(before_kernel, final_kernel, mac)
            self.rt(side, "bridge", "fdb", "del", mac, "dev", "evpnvx" + suffix,
                    "dst", prefix + ".99", "self", "permanent", name=label + "-sentinel-owner-cleanup")
            self.rt(side, "ip", "neigh", "del", ip, "lladdr", mac, "dev", "evpnbr" + suffix,
                    name=label + "-neighbor-owner-cleanup")
        self.wait(lambda: mac not in json.dumps(self.kernel(side, label + "-clean-kernel")),
                  "injection kernel state remains")
        self.wait(lambda: self.import_status(side, label + "-restore-poll")["Snapshot"]["Authoritative"] and
                  not self.import_status(side, label + "-restore-issues")["Snapshot"]["Issues"],
                  "authoritative importer not restored")
        restored = self.import_status(side, label + "-restored")
        self.wait(lambda: (fresh := self.import_status(side, label + "-restored-fresh"))["Time"] >
                  restored["Time"] and fresh["Snapshot"]["Authoritative"] and
                  fresh["Snapshot"] == restored["Snapshot"] and not fresh.get("Error"),
                  "restored complete observations not stable")
        self.owned_state(label + "-after")
        self.probes(label + "-restored")
        self.stop_phase()
        lab.write(self.directory / (label + "-result.json"), {
            "status": "PASS", "kind": kind, "peer": config["RouterID"],
            "policy": "R4-EXACT-RT", "boundary": "actual GoBGP global import from sole configured peer",
            "rt": endpoint["RouteTargets"], "vni": vni, "vtep": endpoint["LocalVTEP"],
            "authoritative": True, "foreignPreserved": True, "exactCleanup": True,
            "completed": time.time()})


def run(directory, cluster):
    require(not directory.exists(), "evidence directory already exists")
    require(directory != lab.ROOT and lab.ROOT not in directory.parents, "evidence must be outside repository")
    directory.mkdir(mode=0o700, parents=True)
    fixture = CrossDomain(directory, cluster)
    cases = {n: {"status": "NOT_RUN"} for n in ("inputs", "baseline", *CASES, "owned-cleanup", "cleanup")}
    current = "inputs"

    def interrupted(signum, _frame):
        raise lab.LabError(f"R4 interrupted by signal {signum}")

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
                           fixture.prepare):
                print("R4 prerequisite=" + action.__name__, flush=True)
                action()
            cases[current] = {"status": "PASS"}
            for index, current in enumerate(CASES, 1):
                kind, side = current.rsplit("-", 1)
                print("R4 scenario=" + current, flush=True)
                fixture.case(side, kind, index)
                cases[current] = {"status": "PASS"}
            current = "owned-cleanup"
            fixture.nb("lsp-del", fixture.b_lsp)
            ThreeDomainBaseline.b_withdraw(fixture)
            ThreeDomainBaseline.r_withdraw(fixture)
            fixture.b_foreign()
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
            print(f"R4 FAIL {current}: {error}", flush=True)
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
                    lab.down(directory)
                    inventory = lab.inventory(lab.load(directory))
                    require(not any(inventory.values()), "owned resources remain after scoped teardown")
                    require(not (lab.STATE_ROOT / "active.json").exists(), "coordinator lease not released")
                    require(not errors, "capture cleanup failed: " + "; ".join(errors))
                    cases["cleanup"] = {"status": "PASS", "inventory": inventory}
                except (lab.LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
                    cases["cleanup"] = {"status": "BLOCKED", "detail": str(error), "errors": errors}
            else:
                cases["cleanup"] = {"status": "PASS", "detail": "no provisioned resources"}
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
            status = "PASS" if all(c["status"] == "PASS" for c in cases.values()) else "BLOCKED"
            lab.write(directory / "r4-result.json", {
                "status": status, "cases": cases, "fullMilestoneAccepted": False})
            print(json.dumps({"status": status, "cases": cases}), flush=True)
    return 0 if status == "PASS" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    raise SystemExit(run(args.directory.resolve(), args.cluster))
