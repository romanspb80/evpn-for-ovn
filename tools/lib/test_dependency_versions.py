"""Offline release-pin and managed-provider evidence regression tests."""
import copy
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
FRR = "quay.io/frrouting/frr:10.7.1@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2"
PROVIDER = "ghcr.io/purelb/k8gobgp@sha256:55da7e8c3d09fef10fc819203904acb84e43bce1011dde66031dad501e1185de"
IMAGE_ID = "sha256:5210e18534979372154487954bad4fc00ad14758923b4ae058e6b0a5be10a7d0"
SPEAKER = "gobgpd version PureLB-fork:01.03.08 (commit: 6b99046) [base: gobgp-4.9.0]"


class DependencyVersionsTest(unittest.TestCase):
    def test_active_pins_are_consistent(self):
        helper = (ROOT / "tools/lib/cni-evpn-frr-interop-test.sh").read_text()
        self.assertIn(f"FRR_IMAGE='{FRR}'", helper)
        self.assertIn("frr version 10.7.1", (ROOT / "frr/frr.conf").read_text())
        for path in ("deploy/operator/examples/evpncore.yaml",
                     "tools/lib/cni-evpn-export-test.sh"):
            text = (ROOT / path).read_text()
            self.assertIn(PROVIDER, text)
            self.assertNotIn("releases/latest", text)
            self.assertNotIn("k8gobgp:0.2.5", text)
        self.assertIn(FRR, (ROOT / "tools/e2e/containerlab.py").read_text())
        for path in ("tools/run-framework-netlink-e2e.sh",
                     "tools/run-framework-ovsdb-e2e.sh",
                     "tools/lib/cni-evpn-frr-interop-test.sh"):
            self.assertIn("\n" + ("  " if "/lib/" in path else "") + "export_record_provider\n",
                          (ROOT / path).read_text())

    def test_frr_startup_has_explicit_configuration(self):
        helper = (ROOT / "tools/lib/cni-evpn-frr-interop-test.sh").read_text()
        lifecycle = (ROOT / "tools/e2e/containerlab.py").read_text()
        for name in ("daemons", "vtysh.conf", "frr.conf"):
            self.assertIn(f'"{name}"', lifecycle)
        self.assertIn(':/etc/frr/{target}:ro', lifecycle)
        self.assertIn("FRR runtime is not the required 10.7.1 release", helper)
        pattern = r"^FRRouting 10\.7\.1(_git)?[ (]"
        for version in ("FRRouting 10.7.1 (frr)", "FRRouting 10.7.1_git (frr)"):
            self.assertRegex(version, pattern)
        for version in ("FRRouting 8.4_git (frr)", "FRRouting 10.7.10 (frr)"):
            self.assertNotRegex(version, pattern)

    def evidence(self):
        return {
            "provider-pod.json": {
                "spec": {"containers": [{"name": "k8gobgp", "image": PROVIDER}]},
                "status": {"containerStatuses": [{"name": "k8gobgp", "ready": True,
                                                   "imageID": IMAGE_ID}]}},
            "provider-daemonset.json": {"status": {"desiredNumberScheduled": 1, "numberReady": 1}},
            "provider-image.json": [{"Id": IMAGE_ID, "RepoDigests": [PROVIDER],
                                      "Config": {"Labels": {"org.opencontainers.image.version": "0.2.8"}}}],
            "provider-crd.json": {"spec": {"versions": [{"name": "v1", "served": True, "storage": True}]}},
            "provider-configs.json": {"items": [{"spec": {"global": {"asn": 64512}}}]},
        }

    def validate(self, evidence, processes="/usr/local/bin/manager\n/usr/local/bin/gobgpd",
                 version=SPEAKER):
        helper = (ROOT / "tools/lib/cni-evpn-export-test.sh").read_text()
        script = re.search(r"<<'PY'\n(.*?)\nPY", helper, re.S).group(1)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for name, value in evidence.items():
                (path / name).write_text(json.dumps(value))
            (path / "provider-processes.txt").write_text(processes)
            (path / "speaker-version.txt").write_text(version)
            return subprocess.run([sys.executable, "-c", script, directory, PROVIDER, IMAGE_ID],
                                  capture_output=True, text=True)

    def test_managed_provider_evidence_passes(self):
        result = self.validate(self.evidence())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("managed-k8gobgp-v0.2.8-runtime=PASS", result.stdout)

    def test_pipe_only_kubectl_captures_and_propagates_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for name, value in self.evidence().items():
                (path / name).write_text(json.dumps(value))
            (path / "provider-processes.txt").write_text("/usr/local/bin/manager /usr/local/bin/gobgpd")
            (path / "speaker-version.txt").write_text(SPEAKER)
            (path / "speaker-global.txt").write_text("AS: 64512")
            (path / "provider-startup.log").write_text("started")
            captures = path / "capture"
            captures.mkdir()
            body = r'''
source "$1"
EVIDENCE_DIR="$2/capture"; PROVIDER_POD=test; FIXTURES="$2"
k() {
  case "$*" in *"rollout status"*) return 0;; esac
  [[ -p /dev/stdout ]] || return 79
  local file
  case "$*" in
    *"get ds"*) [[ "${FAIL_CAPTURE:-0}" == 0 ]] || return 23; file=provider-daemonset.json;;
    *"get pod"*) file=provider-pod.json;;
    *"get crd"*) file=provider-crd.json;;
    *"get bgpconfig"*) file=provider-configs.json;;
    *"gobgpd --version"*) file=speaker-version.txt;;
    *"ps -o"*) file=provider-processes.txt;;
    *"gobgp global"*) file=speaker-global.txt;;
    *"logs"*) file=provider-startup.log;;
    *) return 99;;
  esac
  cat "$FIXTURES/$file"
}
docker() { cat "$FIXTURES/provider-image.json"; }
export_record_provider
'''
            helper = str(ROOT / "tools/lib/cni-evpn-export-test.sh")
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", body, "_", helper, directory],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            broken = "FAIL_CAPTURE=1\n" + body
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", broken, "_", helper, directory],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 23, result.stderr)

    def test_mismatch_unready_standalone_and_competing_writer_fail(self):
        base = self.evidence()
        for case in ("image", "ready", "image-id", "release", "daemonset", "crd", "configs", "netlink"):
            data = copy.deepcopy(base)
            status = data["provider-pod.json"]["status"]["containerStatuses"][0]
            if case == "image":
                data["provider-pod.json"]["spec"]["containers"][0]["image"] = "unverified"
            elif case == "ready":
                status["ready"] = False
            elif case == "image-id":
                status["imageID"] = "sha256:other"
            elif case == "release":
                data["provider-image.json"][0]["Config"]["Labels"]["org.opencontainers.image.version"] = "0.2.5"
            elif case == "daemonset":
                data["provider-daemonset.json"]["status"]["numberReady"] = 0
            elif case == "crd":
                data["provider-crd.json"]["spec"]["versions"][0]["served"] = False
            elif case == "configs":
                data["provider-configs.json"]["items"] *= 2
            elif case == "netlink":
                data["provider-configs.json"]["items"][0]["spec"]["netlinkExport"] = {"enabled": True}
            with self.subTest(case=case):
                self.assertNotEqual(self.validate(data).returncode, 0)
        self.assertNotEqual(self.validate(base, processes="/usr/local/bin/gobgpd").returncode, 0)
        self.assertNotEqual(self.validate(base, version="gobgpd version 4.5.0").returncode, 0)


if __name__ == "__main__":
    unittest.main()
