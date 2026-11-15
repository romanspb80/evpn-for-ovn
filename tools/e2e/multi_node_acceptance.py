#!/usr/bin/env python3
"""Explicit multi-suite historical acceptance; incompatible BGP policies stay isolated."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time

import containerlab as lab
from final_drain import check_barrier
from geneve_baseline import require


SUITES = (("r4", "cross_domain.py", "r4-result.json"),
          ("r3", "remote2_restart.py", "r3-result.json"),
          ("h1", "final_drain.py", "r6-result.json"))
MATRIX = {
    "A1": ("h1", "baseline"), "A2": ("h1", "A2"),
    "B1": ("h1", "B1"), "B2": ("h1", "B2"),
    **{n: ("h1", "baseline") for n in ("C1", "C2", "C3", "D1", "D2", "D3")},
    "E1": ("r4", "all symmetric policy cases"), "E2": ("r4", "both unauthorized VTEPs"),
    **{n: ("h1", n) for n in ("E3", "E4", "F1", "F3", "F4", "F5", "G1", "G2", "G3")},
    "F2": ("h1", "F2-r1 + F2-r2"),
    **{"R2-" + n: ("r3", n) for n in ("projector-only", "speaker-only", "combined")},
    "H1": ("h1", "pre-destruction barrier"), "C0": ("all", "scoped cleanup"),
}


def read(path):
    return json.loads(path.read_text())


def validate(directory):
    results, builds = {}, {}
    for suite, _, filename in SUITES:
        folder = directory / suite
        results[suite] = read(folder / filename)
        require(results[suite]["status"] == "PASS" and
                all(c["status"] == "PASS" for c in results[suite]["cases"].values()),
                suite + " has a mandatory non-passing scenario")
        require(not any(results[suite]["cases"]["cleanup"]["inventory"].values()),
                "nonempty scoped cleanup inventory")
        require(read(folder / "lab.json")["phase"] == "down", "suite topology still alive")
        builds[suite] = read(folder / "build-inputs.json")
        for name, digest in builds[suite]["sources"].items():
            path = Path(name) if name.startswith("/") else lab.ROOT / name
            require(hashlib.sha256(path.read_bytes()).hexdigest() == digest, "source changed: " + name)
        for name, digest in builds[suite]["binaries"].items():
            require(hashlib.sha256((folder / "build" / name).read_bytes()).hexdigest() == digest,
                    "binary evidence changed")
    require(builds["h1"]["binaries"] == builds["r3"]["binaries"] == builds["r4"]["binaries"],
            "suites used different binary inputs")
    pins = builds["h1"]["sources"]
    for build in builds.values():
        require(all(pins.get(k) == v for k, v in build["sources"].items()),
                "suite source inputs differ")
    scenarios = read(directory / "h1/integrated-scenarios.json")
    for name in ("A2", "B1", "B2", "E3", "E4", "F5", "G1", "G2", "G3"):
        require(scenarios[name]["status"] == "PASS", "historical gate not executed: " + name)
    for side in ("a", "b"):
        require(read(directory / f"h1/F3-{side}-ownership.json")["replayRejected"]["Error"],
                "F3 stale replay was not rejected")
    barrier = read(directory / "h1/H1-barrier.json")
    check_barrier(read(directory / "h1/H1-before.json"), read(directory / "h1/H1-after.json"))
    require(barrier["status"] == "PASS", "H1 barrier not accepted")
    shutdown = read(directory / "h1/H1-shutdown-events.json")
    require(len(shutdown) == 6 and all(
        e["stoppedAt"] >= e["requestedAt"] > barrier["acceptedAt"] for e in shutdown),
        "owner shutdown did not follow independently accepted H1")
    require(not (lab.STATE_ROOT / "active.json").exists(), "coordinator lease remains")
    return {"status": "PASS", "fullMilestoneAccepted": True, "H1": "PASS",
            "matrix": {n: {"status": "PASS", "suite": suite, "gate": gate}
                       for n, (suite, gate) in MATRIX.items()},
            "sources": pins, "binaries": builds["h1"]["binaries"],
            "policySeparation": "R4 exact-RT policy and R3/H1 foreign IPv4 witnesses require separate fixtures",
            "limitations": "bounded independent pairs only; G.8 limitations retained; no shared VTEP, "
                           "generalized multi-chassis, Type-3/5 or production CNI acceptance"}


def run(directory, cluster):
    require(not directory.exists() and directory != lab.ROOT and lab.ROOT not in directory.parents,
            "new evidence directory outside repository required")
    directory.mkdir(parents=True, mode=0o700)
    provenance = {name: lab.command(command).stdout for name, command in (
        ("head", ["git", "rev-parse", "HEAD"]), ("branch", ["git", "branch", "--show-current"]),
        ("status", ["git", "status", "--porcelain=v1"]))}
    lab.write(directory / "git.json", provenance)
    with lab.exclusive():
        for suite, script, filename in SUITES:
            print("R6 suite=" + suite, flush=True)
            started = time.time()
            with (directory / (suite + ".log")).open("w") as log:
                result = subprocess.run([sys.executable, str(lab.ROOT / "tools/e2e" / script),
                    "--directory", str(directory / suite), "--cluster", cluster + suite],
                    stdout=log, stderr=subprocess.STDOUT, pass_fds=lab.lock_descriptors())
            lab.write(directory / (suite + "-execution.json"),
                      {"startedAt": started, "finishedAt": time.time(), "exit": result.returncode})
            if result.returncode:
                lab.write(directory / "acceptance.json", {"status": "BLOCKED",
                    "fullMilestoneAccepted": False, "failedSuite": suite,
                    "reason": "retain all suite evidence and any active lease; no aggregate promotion"})
                return 1
        manifest = validate(directory)
        lab.write(directory / "acceptance.json", manifest)
    return 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    raise SystemExit(run(args.directory.resolve(), args.cluster))
