#!/usr/bin/env python3
"""Offline regressions for packet-capture lifecycle and retained PCAP evidence."""

import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest
import test_cni_vxlan_pcap_verify as packet_fixtures


ROOT = Path(__file__).resolve().parents[2]
LIB = ROOT / "tools/lib"
HELPER = LIB / "cni-evpn-frr-interop-test.sh"
CAPTURE_FILES = {
    "pod-facing": ("pod-net1-peer.pcap", "inner"),
    "worker-underlay": ("worker-underlay.pcap", "vxlan"),
    "frr-underlay": ("frr-underlay.pcap", "vxlan"),
    "frr-vxlan": ("frr-vxlan.pcap", "inner"),
    "remote-workload": ("remote-workload-peer.pcap", "inner"),
}


def replay_manifest(directory, source_manifest=None):
    evidence = Path(directory)
    if source_manifest is None:
        # Synthetic regression: reverse sequence 2 is absent at three observation
        # points, while complete endpoint captures and advancing OVS counters remain.
        fixture = packet_fixtures.PacketTests()
        fixture.setUp()

        def omit_reverse_sequence(manifest, inner_pcap, vxlan_pcap):
            inner_offset = 14 + 20 + 8 + 8  # Synthetic Ethernet/IPv4/UDP/VXLAN headers.
            for source, points in (
                (inner_pcap, ("pod-facing",)),
                (vxlan_pcap, ("worker-underlay", "frr-underlay")),
            ):
                _, records = packet_fixtures.VERIFY.read_pcap(source)
                partial = evidence / f"partial-{source.name}"
                packet_fixtures.write_pcap(
                    partial,
                    [(int(r["timestamp"]), r["frame"]) for r in records
                     if r["timestamp"] != 22 or
                     packet_fixtures.VERIFY.inner_packet(
                         r["frame"], inner_offset if source == vxlan_pcap else 0
                     )["icmp"]["sequence"] != 2],
                )
                for point in points:
                    manifest["captures"][point]["path"] = str(partial)

        return fixture.replay(
            directory, after_count=9, mutate=omit_reverse_sequence, verify=False
        )
    manifest = json.loads((Path(source_manifest) / "packet-path-manifest.json").read_text())
    pcap_dir = Path(source_manifest)
    manifest["captures"] = {
        name: {"path": str(pcap_dir / filename), "kind": kind}
        for name, (filename, kind) in CAPTURE_FILES.items()
    }
    manifest["ovs"].update({
        "tunnelConfig": str(pcap_dir / "ovs-tunnel-before.txt"),
        "portStatsBefore": str(pcap_dir / "ovs-ports-before.txt"),
        "portStatsAfter": str(pcap_dir / "ovs-ports-after.txt"),
        "flowsBefore": str(pcap_dir / "ovs-flows-before.txt"),
        "flowsAfter": str(pcap_dir / "ovs-flows-after.txt"),
    })
    path = evidence / "packet-path-manifest.json"
    path.write_text(json.dumps(manifest))
    return path


def run_verifier(manifest, output):
    return subprocess.run(
        ["python3", str(LIB / "cni-vxlan-pcap-verify.py"),
         "--manifest", str(manifest), "--output", str(output)],
        text=True, capture_output=True, check=False,
    )


def has_capture_preflight_privileges():
    if not shutil.which("tcpdump") or not Path("/usr/bin/systemd-run").exists():
        return False
    if subprocess.run(["getent", "passwd", "tcpdump"], capture_output=True).returncode:
        return False
    return subprocess.run(["sudo", "-n", "true"], capture_output=True).returncode == 0


def privileged_capture_tests_enabled():
    return (
        os.environ.get("EVPN_RUN_PRIVILEGED_CAPTURE_TEST") == "1"
        and has_capture_preflight_privileges()
    )


class CaptureLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.source = HELPER.read_text()

    def run_status_writer(self, evidence, *, exit_code, cleanup_code, original_code, values):
        assignments = "\n".join(
            f"{name}={shlex.quote(value)}" for name, value in values.items()
        )
        script = f"""
set -euo pipefail
KIND_CLUSTER=cni-status-test
EVIDENCE_DIR={shlex.quote(str(evidence))}
source {shlex.quote(str(HELPER))}
CLUSTER_CREATION_STATUS=NOT_STARTED
CLUSTER_CLEANUP_STATUS=SKIPPED
CLUSTER_CREATE_ATTEMPTED=0
CREATED=0
{assignments}
interop_write_status {exit_code} {cleanup_code} {original_code}
"""
        return subprocess.run(
            ["bash", "-c", script],
            text=True,
            capture_output=True,
            check=False,
        )

    def test_interop_status_cannot_claim_full_pass_without_shared_vtep_lifecycle(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory)
            result = self.run_status_writer(
                evidence,
                exit_code=0,
                cleanup_code=0,
                original_code=0,
                values={
                    "INTEROP_CONTROL_PLANE_STATUS": "PASS",
                    "INTEROP_FORWARDING_STATE_STATUS": "PASS",
                    "INTEROP_CAPTURE_STARTUP_STATUS": "PASS",
                    "INTEROP_FORWARD_ICMP_STATUS": "PASS",
                    "INTEROP_REVERSE_ICMP_STATUS": "PASS",
                    "INTEROP_PACKET_VERIFICATION_STATUS": "PASS",
                    "INTEROP_CAPTURE_SHUTDOWN_STATUS": "PASS",
                    "INTEROP_CAPTURE_PREFLIGHT_STATUS": "PASS",
                    "INTEROP_LIFECYCLE_STATUS": "PASS",
                    "INTEROP_SHARED_VTEP_STATUS": "NOT_RUN",
                },
            )
            status = json.loads((evidence / "interop-status.json").read_text())

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(status["overall_status"], "PARTIAL")
        self.assertEqual(status["lifecycle_status"], "PASS")
        self.assertEqual(status["shared_vtep_lifecycle_status"], "NOT_RUN")
        self.assertEqual(
            status["topology_scope"],
            "single-ovn-chassis-external-frr-peer",
        )

    def test_capture_preflight_failure_is_classified_and_retains_prior_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory)
            earlier = evidence / "control-plane-evidence.txt"
            earlier.write_text("GoBGP route snapshot retained\n", encoding="utf-8")
            result = self.run_status_writer(
                evidence,
                exit_code=1,
                cleanup_code=0,
                original_code=1,
                values={
                    "INTEROP_CAPTURE_PREFLIGHT_STATUS": "FAIL",
                    "INTEROP_FAILURE_CLASSIFICATION": "HARNESS FAILURE",
                    "INTEROP_FAILURE_GATE": "CAPTURE PREFLIGHT",
                },
            )
            status = json.loads((evidence / "interop-status.json").read_text())
            retained = earlier.read_text(encoding="utf-8")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(status["overall_status"], "FAIL")
        self.assertEqual(status["failure_classification"], "HARNESS FAILURE")
        self.assertEqual(status["failure_gate"], "CAPTURE PREFLIGHT")
        self.assertEqual(retained, "GoBGP route snapshot retained\n")

    def test_all_capture_points_must_be_ready_before_stimulus(self):
        start_one = self.source[
            self.source.index("interop_capture_start_one() {"):
            self.source.index("\ninterop_capture_assert_active() {")
        ]
        start = self.source[
            self.source.index("interop_capture_start() {"):
            self.source.index("\ninterop_capture_stop() {")
        ]
        self.assertIn('grep -Fq "listening on ${interface}" "${log}"', start_one)
        self.assertIn('INTEROP_CAPTURE_MAIN_PIDS[-1]="${pid}"', start_one)
        for name in ("pod-net1-peer", "worker-underlay", "frr-underlay",
                     "frr-vxlan", "remote-workload-peer"):
            self.assertIn(f'interop_capture_start_one "${{prefix}}{name}" ', start)
        self.assertIn("interop_capture_assert_active pre-stimulus", start)
        verify = self.source[self.source.index("interop_verify() {"):]
        self.assertLess(verify.index("interop_capture_start"), verify.index("interop_ping_command forward"))

    def test_active_checks_cover_both_traffic_directions_and_drain(self):
        stop = self.source[
            self.source.index("interop_capture_stop() {"):
            self.source.index("\ninterop_capture_local_uuids() {")
        ]
        verify = self.source[self.source.index("interop_verify() {"):]
        for phase in ("after-forward-ping", "after-reverse-ping"):
            self.assertIn(f"interop_capture_assert_active {phase}", verify)
        self.assertLess(stop.index("drain-started"), stop.index("sleep 2"))
        self.assertLess(stop.index("sleep 2"), stop.index("drain-completed"))
        self.assertLess(stop.index("drain-completed"), stop.index("interop_capture_assert_active after-drain"))
        self.assertLess(stop.index("interop_capture_assert_active after-drain"), stop.index("systemctl stop"))

    def test_replacement_traffic_stimuli_are_recorded_before_capture_verification(self):
        verify_traffic = self.source[
            self.source.index("interop_verify_captured_traffic() {"):
            self.source.index("\ninterop_prepare_no_replacement_lifecycle() {")
        ]
        self.assertLess(
            verify_traffic.index('data["stimuli"] ='),
            verify_traffic.index("interop_capture_stop || return 1"),
        )
        self.assertIn('("pod-to-frr", result["forward"])', verify_traffic)
        self.assertIn('("frr-to-pod", result["reverse"])', verify_traffic)

    def test_final_network_cleanup_requires_valid_query_evidence(self):
        lifecycle = self.source[
            self.source.index("interop_verify_full_lifecycle() {"):
            self.source.index("\ninterop_verify() {")
        ]
        for artifact, expectation in (
            ("test-network-before-delete.txt", "required"),
            ("shared-bridge-before-network-delete.json", "json"),
            ("shared-vxlan-before-network-delete.json", "json"),
            ("logical-switches-after-network-delete.json", "json"),
            ("foreign-lsp-after-network-delete.txt", "required"),
            ("shared-bridge-after-network-delete.json", "json"),
            ("shared-vxlan-after-network-delete.json", "json"),
        ):
            with self.subTest(artifact=artifact):
                marker = f'interop_capture_diagnostic "{artifact}"'
                self.assertIn(marker, lifecycle)
                excerpt = lifecycle[lifecycle.index(marker):]
                self.assertIn(f" {expectation} \\\n", excerpt[:300])
        self.assertIn('interop_capture_diagnostic "test-network-delete.txt"', lifecycle)
        self.assertIn('interop_capture_diagnostic "br-int-after-network-delete.txt"', lifecycle)

    def test_graceful_bounded_shutdown_waits_and_records_exit_before_verification(self):
        start_one = self.source[
            self.source.index("interop_capture_start_one() {"):
            self.source.index("\ninterop_capture_assert_active() {")
        ]
        stop = self.source[
            self.source.index("interop_capture_stop() {"):
            self.source.index("\ninterop_capture_local_uuids() {")
        ]
        self.assertIn("--property=TimeoutStopSec=10s", start_one)
        self.assertIn("--property=KillSignal=SIGINT", start_one)
        self.assertNotIn("--collect", start_one)
        self.assertNotIn("-c 256", start_one)
        self.assertIn("cgroup.procs", stop)
        self.assertIn("--property=ExecMainStatus --value", stop)
        self.assertIn("--property=Result --value", stop)
        self.assertIn('capture-exit-status.txt', stop)
        self.assertLess(stop.index("systemctl stop"), stop.index("capture-exit-status.txt"))
        self.assertLess(stop.index("capture-exit-status.txt"), stop.index("cni-vxlan-pcap-verify.py"))
        self.assertLess(stop.index("while [[ -n \"${cgroup}\""), stop.index("interop_capture_decode_outputs"))
        decode = self.source[
            self.source.index("interop_capture_decode_outputs() {"):
            self.source.index("\ninterop_capture_start_one() {")
        ]
        self.assertIn("tcpdump -nn -vvv -e -r", decode)
        self.assertIn('if [[ ! -s "${pcap}" ]]', decode)

    def test_incomplete_main_capture_set_remains_strictly_incomplete(self):
        with tempfile.TemporaryDirectory() as directory:
            manifest = replay_manifest(directory)
            output = Path(directory) / "verification.json"
            result = run_verifier(manifest, output)
            report = json.loads(output.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["status"], "PARTIAL")
        missing_points = {
            name for name, capture in report["captures"].items()
            if any(packet[0] == "frr-to-pod" and packet[2] == 2 for packet in capture["missing"])
        }
        self.assertEqual(missing_points, {"pod-facing", "worker-underlay", "frr-underlay"})
        self.assertEqual(report["captures"]["frr-vxlan"]["missing"], [])
        self.assertEqual(report["captures"]["remote-workload"]["missing"], [])
        self.assertEqual(report["ovs"]["matchedFlowPacketsDelta"], 9)

    def test_capture_commands_use_private_staging_and_explicit_tcpdump_identity(self):
        create = self.source[
            self.source.index("interop_capture_create_staging_dir() {"):
            self.source.index("\ninterop_capture_discard_staging_dir() {")
        ]
        start_one = self.source[
            self.source.index("interop_capture_start_one() {"):
            self.source.index("\ninterop_capture_assert_active() {")
        ]
        preflight = self.source[
            self.source.index("interop_capture_preflight() {"):
            self.source.index("\ninterop_capture_publish_outputs() {")
        ]
        self.assertIn("mktemp -d /tmp/evpn4ovn-capture.XXXXXX", create)
        self.assertIn('chmod 0700 "${INTEROP_CAPTURE_STAGING_DIR}"', create)
        self.assertIn("stat -c '%u:%g:%a'", create)
        self.assertIn("--property=User=root", start_one)
        self.assertIn("--property=Group=root", start_one)
        self.assertIn("--property=LimitFSIZE=69656", start_one)
        self.assertIn("-Z tcpdump", start_one)
        self.assertIn('${INTEROP_CAPTURE_STAGING_DIR}/${name}.pcap', start_one)
        self.assertIn("-Z tcpdump", preflight)
        self.assertIn("-i lo", preflight)
        self.assertIn("ExecMainStatus --value", preflight)
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        self.assertLess(runner.index("interop_capture_preflight ||"),
                        runner.index("kind get clusters"))
        self.assertLess(runner.index("interop_capture_preflight ||"),
                        runner.index("e2e_lab_up cni"))

    def test_staging_directory_is_private_and_cleanup_preserves_unknown_files(self):
        with tempfile.TemporaryDirectory() as evidence:
            env = {**os.environ, "EVIDENCE_DIR": evidence}
            script = f'''
source "{HELPER}"
interop_capture_create_staging_dir
stage="$INTEROP_CAPTURE_STAGING_DIR"
test "$(stat -c '%u:%g:%a' "$stage")" = "$(id -u):$(id -g):700"
touch "$stage/foreign.keep"
if interop_capture_discard_staging_dir; then
  exit 20
fi
test -f "$stage/foreign.keep"
rm "$stage/foreign.keep"
interop_capture_discard_staging_dir
test ! -e "$stage"
test -z "$INTEROP_CAPTURE_STAGING_DIR"
'''
            result = subprocess.run(
                ["bash", "-ec", script], env=env, text=True, capture_output=True, check=False
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_staged_capture_publication_sets_runner_owned_private_file(self):
        if not privileged_capture_tests_enabled():
            self.skipTest(
                "set EVPN_RUN_PRIVILEGED_CAPTURE_TEST=1 and provide passwordless sudo "
                "with a tcpdump account"
            )
        with tempfile.TemporaryDirectory() as evidence:
            stage = tempfile.mkdtemp(prefix="evpn4ovn-capture-publish-test.", dir="/tmp")
            os.chmod(stage, 0o700)
            try:
                for name, _ in CAPTURE_FILES.values():
                    source = Path(stage) / name
                    source.write_bytes(b"offline capture publication fixture\n")
                    source.chmod(0o644)
                    subprocess.run(
                        ["sudo", "-n", "chown", "tcpdump:tcpdump", str(source)],
                        check=True, capture_output=True,
                    )
                env = {
                    **os.environ,
                    "EVIDENCE_DIR": evidence,
                    "CAPTURE_STAGE": stage,
                }
                script = f'''
source "{HELPER}"
INTEROP_CAPTURE_STAGING_DIR="$CAPTURE_STAGE"
interop_capture_publish_outputs
'''
                result = subprocess.run(
                    ["bash", "-ec", script], env=env, text=True, capture_output=True, check=False
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                for name, _ in CAPTURE_FILES.values():
                    published = Path(evidence) / name
                    self.assertEqual(
                        (published.stat().st_uid, published.stat().st_gid,
                         published.stat().st_mode & 0o777),
                        (os.getuid(), os.getgid(), 0o600),
                    )
                    self.assertEqual(published.read_bytes(), b"offline capture publication fixture\n")
            finally:
                for name, _ in CAPTURE_FILES.values():
                    source = Path(stage) / name
                    if source.exists():
                        source.unlink()
                os.rmdir(stage)

    def test_initial_and_replacement_capture_publication_keeps_all_pcap_and_decoded_artifacts(self):
        with tempfile.TemporaryDirectory() as evidence, tempfile.TemporaryDirectory() as stage, \
                tempfile.TemporaryDirectory() as stubs:
            stub_dir = Path(stubs)
            (stub_dir / "getent").write_text(
                f"#!/bin/sh\nprintf 'tcpdump:x:{os.getuid()}:{os.getgid()}:tcpdump:/nonexistent:/usr/sbin/nologin\\\\n'\n"
            )
            (stub_dir / "tcpdump").write_text(
                '#!/bin/sh\n'
                'for pcap do :; done\n'
                'test -s "$pcap" || exit 1\n'
                'printf "decoded %s\\\\n" "$pcap"\n'
            )
            for stub in stub_dir.iterdir():
                stub.chmod(0o755)
            env = {
                **os.environ,
                "EVIDENCE_DIR": evidence,
                "CAPTURE_STAGE": stage,
                "PATH": f"{stubs}:/usr/bin:/bin",
            }
            script = f'''
set -euo pipefail
KIND_CLUSTER=capture-publication-test
source "{HELPER}"
INTEROP_CAPTURE_STAGING_DIR="$CAPTURE_STAGE"
for phase in initial replacement; do
  INTEROP_CAPTURE_PHASE="$phase"
  prefix=""
  [[ "$phase" == initial ]] || prefix="$phase-"
  for name in pod-net1-peer worker-underlay frr-underlay frr-vxlan remote-workload-peer; do
    printf '%s %s capture fixture\\n' "$phase" "$name" >"$CAPTURE_STAGE/$prefix$name.pcap"
  done
  interop_capture_publish_outputs
  interop_capture_decode_outputs "$phase"
done
'''
            result = subprocess.run(
                ["bash", "-c", script],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            for phase in ("initial", "replacement"):
                prefix = "" if phase == "initial" else f"{phase}-"
                for name, _ in CAPTURE_FILES.values():
                    pcap = Path(evidence) / f"{prefix}{name}"
                    decoded = Path(evidence) / f"{prefix}{name.removesuffix('.pcap')}-decoded.txt"
                    self.assertEqual(
                        pcap.read_text(),
                        f"{phase} {name.removesuffix('.pcap')} capture fixture\n",
                    )
                    self.assertIn(str(pcap), decoded.read_text())
            self.assertEqual(
                len(list(Path(evidence).glob("*-decoded.txt"))),
                10,
            )
            self.assertEqual(
                len(list(Path(evidence).glob("replacement-*-decoded.txt"))),
                5,
            )

    def test_missing_or_empty_staged_captures_fail_publication_and_decoding(self):
        for missing in (True, False):
            with self.subTest(missing=missing), tempfile.TemporaryDirectory() as evidence, \
                    tempfile.TemporaryDirectory() as stage, tempfile.TemporaryDirectory() as stubs:
                stub_dir = Path(stubs)
                (stub_dir / "getent").write_text(
                    f"#!/bin/sh\nprintf 'tcpdump:x:{os.getuid()}:{os.getgid()}:tcpdump:/nonexistent:/usr/sbin/nologin\\\\n'\n"
                )
                (stub_dir / "tcpdump").write_text(
                    '#!/bin/sh\n'
                    'for pcap do :; done\n'
                    'test -s "$pcap" || exit 1\n'
                    'printf "decoded\\\\n"\n'
                )
                for stub in stub_dir.iterdir():
                    stub.chmod(0o755)
                env = {
                    **os.environ,
                    "EVIDENCE_DIR": evidence,
                    "CAPTURE_STAGE": stage,
                    "PATH": f"{stubs}:/usr/bin:/bin",
                }
                omitted = "replacement-remote-workload-peer.pcap"
                script = f'''
source "{HELPER}"
INTEROP_CAPTURE_PHASE=replacement
INTEROP_CAPTURE_STAGING_DIR="$CAPTURE_STAGE"
for name in pod-net1-peer worker-underlay frr-underlay frr-vxlan remote-workload-peer; do
  path="$CAPTURE_STAGE/replacement-$name.pcap"
  if [[ "$name" == remote-workload-peer ]]; then
    {'continue' if missing else ' : >"$path"'}
  else
    printf 'capture fixture\\\\n' >"$path"
  fi
done
if interop_capture_publish_outputs; then
  echo "publication accepted incomplete capture set" >&2
  exit 20
fi
if interop_capture_decode_outputs replacement; then
  echo "decoder accepted incomplete capture set" >&2
  exit 21
fi
'''
                result = subprocess.run(
                    ["bash", "-c", script],
                    env=env,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(omitted.removesuffix(".pcap"), result.stderr)

    def test_capture_preflight_launch_failure_fails_closed(self):
        with tempfile.TemporaryDirectory() as evidence, tempfile.TemporaryDirectory() as stub_dir:
            stub = Path(stub_dir) / "sudo"
            stub.write_text(
                '#!/bin/sh\n'
                'if [ "$2" = systemctl ] && [ "$3" = show ]; then echo not-found; exit 0; fi\n'
                'if [ "$2" = systemd-run ]; then exit 1; fi\n'
                'exit 97\n'
            )
            stub.chmod(0o755)
            env = {
                **os.environ,
                "EVIDENCE_DIR": evidence,
                "KIND_CLUSTER": "capture-preflight-failure",
                "PATH": f"{stub_dir}:/usr/bin:/bin",
            }
            script = f'''
source "{HELPER}"
if interop_capture_preflight; then
  exit 20
fi
test "$INTEROP_CAPTURE_PREFLIGHT_STATUS" = FAIL
test -z "$INTEROP_CAPTURE_STAGING_DIR"
'''
            result = subprocess.run(
                ["bash", "-ec", script], env=env, text=True, capture_output=True, check=False
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    @unittest.skipUnless(
        privileged_capture_tests_enabled(),
        "set EVPN_RUN_PRIVILEGED_CAPTURE_TEST=1 and provide passwordless sudo, systemd, "
        "ping and tcpdump",
    )
    def test_actual_bounded_tcpdump_preflight_uses_expected_identity_and_permissions(self):
        with tempfile.TemporaryDirectory() as evidence:
            env = {
                **os.environ,
                "EVIDENCE_DIR": evidence,
                "KIND_CLUSTER": f"offline-capture-{os.getpid()}",
            }
            result = subprocess.run(
                ["bash", "-ec", f'source "{HELPER}"; interop_capture_preflight'],
                env=env, text=True, capture_output=True, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads((Path(evidence) / "capture-preflight.json").read_text())
            lifecycle = (Path(evidence) / "capture-process-lifecycle.log").read_text()
            decoded = (Path(evidence) / "capture-preflight-decoded.txt").read_text()
        tcpdump = subprocess.run(
            ["getent", "passwd", "tcpdump"], text=True, capture_output=True, check=True
        ).stdout.split(":")
        self.assertEqual(report["status"], "PASS")
        self.assertEqual(report["serviceIdentity"], {"uid": 0, "gid": 0})
        self.assertEqual(report["tcpdumpIdentity"], {
            "effectiveUid": int(tcpdump[2]), "effectiveGid": int(tcpdump[3]),
        })
        self.assertEqual(report["staging"]["path"], "/tmp")
        self.assertEqual(report["staging"]["directoryMode"], "0700")
        self.assertEqual(report["output"]["owner"], f'{tcpdump[2]}:{tcpdump[3]}')
        self.assertEqual(report["output"]["mode"], "644")
        self.assertGreater(report["output"]["bytes"], 24)
        self.assertIn("ICMP echo request", decoded)
        staging_path = next(
            line.split("directory=", 1)[1].split()[0]
            for line in lifecycle.splitlines() if "staging-created" in line
        )
        self.assertFalse(Path(staging_path).exists())
        self.assertNotIn("apparmor", result.stderr.lower())

    @unittest.skipUnless(os.environ.get("EVPN_STABILIZATION_EVIDENCE_DIR"),
                         "set EVPN_STABILIZATION_EVIDENCE_DIR to replay retained stabilization PCAPs")
    def test_retained_stabilization_capture_set_passes_strict_attribution(self):
        reference = Path(os.environ["EVPN_STABILIZATION_EVIDENCE_DIR"])
        with tempfile.TemporaryDirectory() as directory:
            manifest = replay_manifest(directory, reference)
            output = Path(directory) / "verification.json"
            result = run_verifier(manifest, output)
            report = json.loads(output.read_text())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(report["status"], "PASS", report["errors"])
        self.assertEqual(set(report["captures"]), set(CAPTURE_FILES))
        self.assertTrue(all(not capture["missing"] for capture in report["captures"].values()))


if __name__ == "__main__":
    unittest.main()
