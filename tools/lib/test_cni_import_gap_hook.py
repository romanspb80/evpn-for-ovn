import json
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


class ImportGapHookTests(unittest.TestCase):
    def test_gap_provider_reuses_attestation_without_enabling_remote_writer(self):
        result = subprocess.run(
            ["bash", "-ec", r"""
source tools/lib/cni-evpn-import-test.sh
ROOT="$PWD"
export_setup_provider() { EXPORT_MOUNTS=socket; EXPORT_VOLUMES=socket; }
import_gap_setup_provider
test "$EXPORT_HOST_PID" = '      hostPID: true'
test -z "$REMOTE_FLAGS"
[[ "$EXPORT_MOUNTS" == *provider-attestation* ]]
[[ "$EXPORT_VOLUMES" == *provider-ctr* ]]
[[ "$EXPORT_SERVICE_ACCOUNT" == *evpn-provider-observer* ]]
"""], cwd=ROOT, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_gap_fixture_preserves_attestation_and_seeds_foreign_control(self):
        with tempfile.TemporaryDirectory() as directory:
            config = {"DryRun": True, "ProcessContinuity": True,
                      "AttestationConfigPath": "/etc/evpn/provider-attestation/provider.json"}
            Path(directory, "export-config.json").write_text(json.dumps(config))
            result = subprocess.run(
                ["bash", "-ec", r"""
source tools/lib/cni-evpn-import-test.sh
EVIDENCE_DIR="$1"
export_check() { printf 'check %s foreign=%s\n' "$*" "${EXPORT_FOREIGN:-no}"; }
export_test() { printf 'stage=%s\n' "$EXPORT_STAGE"; }
export_replace_config() { test -s "$1"; }
import_gap_ready
""", "sh", directory], cwd=ROOT, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("check 0 2 gap-dry-ready foreign=no", result.stdout)
            self.assertIn("stage=seed-foreign", result.stdout)
            self.assertIn("check 2 2 gap-fixture-ready foreign=yes", result.stdout)
            actual = json.loads(Path(directory, "export-live.json").read_text())
            self.assertEqual(actual, {**config, "DryRun": False})

    def test_gap_config_attests_provider_before_publishing_export_config(self):
        with tempfile.TemporaryDirectory() as directory:
            config = {"DryRun": True, "ProcessContinuity": True}
            Path(directory, "export-config.json").write_text(json.dumps(config))
            result = subprocess.run(
                ["bash", "-ec", r"""
source tools/lib/cni-evpn-import-test.sh
EVIDENCE_DIR="$1" EVPN_GAP_CLOSURE=1
k() {
  if [[ "$*" == *jsonpath* ]]; then printf 'owned-provider'; fi
}
export_record_provider() { echo record-provider; }
interop_setup_provider_attestation() {
  test "$*" = 'cni-export cni-test'
  echo attest-provider
}
export_replace_config() {
  cp "$1" "$EVIDENCE_DIR/published.json"
  echo publish-export-config
}
import_setup_config
""", "sh", directory], cwd=ROOT, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.splitlines(),
                             ["record-provider", "attest-provider", "publish-export-config"])
            actual = json.loads(Path(directory, "published.json").read_text())
            self.assertEqual(actual, {**config, "AttestationConfigPath":
                                      "/etc/evpn/provider-attestation/provider.json"})
            provider = Path(directory, "provider-config.yaml").read_text()
            self.assertIn('neighborAddress: "127.0.0.2"', provider)
            self.assertIn("receive: true", provider)

    def run_hook(self, test_status=0, copy_status=0, pass_marker=0):
        script = r"""
set -o pipefail
source tools/lib/cni-evpn-import-test.sh
EVIDENCE_DIR=evidence WORKER=owned-worker LOCAL_VTEP=192.0.2.1
ag() {
  printf 'ag %s\n' "$*" >&2
  if [[ "$1" == env ]]; then return "$TEST_STATUS"; fi
}
mkdir() { printf 'mkdir %s\n' "$*" >&2; }
tar() { printf 'tar %s\n' "$*" >&2; cat >/dev/null; return "$COPY_STATUS"; }
tee() { cat; }
grep() { return "$PASS_MARKER"; }
import_gap_negatives
"""
        result = subprocess.run(
            ["bash", "-c", script],
            cwd=ROOT,
            env={"PATH": "/usr/bin:/bin", "TEST_STATUS": str(test_status),
                 "COPY_STATUS": str(copy_status), "PASS_MARKER": str(pass_marker)},
            capture_output=True,
            text=True,
            check=False,
        )
        return result

    def test_runs_opted_in_selector_and_copies_evidence(self):
        result = self.run_hook()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("EVPN_IMPORT_LIVE=disposable", result.stdout)
        self.assertIn("EVPN_GAP_CLOSURE=1", result.stdout)
        self.assertIn("EVPN_GAP_EVIDENCE_DIR=/run/evpn-node-agent/gap-negatives", result.stdout)
        self.assertIn("^TestRemoteType2GapNegatives_Live$", result.stdout)
        self.assertIn("ag tar -cf - -C /run/evpn-node-agent/gap-negatives .", result.stderr)
        self.assertIn("tar -xf - -C evidence/remote-gap-negatives/", result.stderr)

    def test_copies_failure_evidence_and_propagates_test_failure(self):
        result = self.run_hook(test_status=7)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertIn("ag tar -cf -", result.stderr)

    def test_copy_failure_and_missing_pass_marker_fail_closed(self):
        self.assertNotEqual(self.run_hook(copy_status=1).returncode, 0)
        self.assertNotEqual(self.run_hook(pass_marker=1).returncode, 0)


if __name__ == "__main__":
    unittest.main()
