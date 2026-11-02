#!/usr/bin/env python3
"""Offline regressions for replacement-Pod OVN binding acceptance."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools/lib"))
from cni_invocation_identity import (
    CniIdentityError,
    parse_cni_args_identity,
    parse_invocation_identity,
)


HELPER = ROOT / "tools/lib/cni-evpn-frr-interop-test.sh"
CLASSIFIER = ROOT / "tools/lib/cni_ovn_binding.py"
RUNNER = ROOT / "tools/run-cni-mvp-2.4b-multus.sh"


def binding_snapshot(*, logical_port="replacement-lsp", datapath="datapath-1",
                     chassis="chassis-1", up=True, port_type=""):
    row = [
        logical_port,
        ["uuid", datapath],
        port_type,
        ["set", []] if chassis is None else ["uuid", chassis],
        up,
    ]
    return {
        "headings": ["logical_port", "datapath", "type", "chassis", "up"],
        "data": [row],
    }


class CniInvocationIdentityTests(unittest.TestCase):
    def test_cni_args_preserves_equals_in_identity_values(self):
        identity = parse_cni_args_identity(
            "K8S_POD_INFRA_CONTAINER_ID=container=id;K8S_POD_UID=uid-a"
        )
        self.assertEqual(identity["K8S_POD_INFRA_CONTAINER_ID"], "container=id")

    def test_observed_multus_duplicate_identity_formats_are_accepted(self):
        variants = (
            "IgnoreUnknown=true;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "IgnoreUnknown=1;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a",
            "IgnoreUnknown=true;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "IgnoreUnknown=1;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a",
            "IgnoreUnknown=true;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "IgnoreUnknown=1",
            "IgnoreUnknown=true;K8S_POD_NAMESPACE=ns;K8S_POD_NAME=pod-a;"
            "K8S_POD_INFRA_CONTAINER_ID=container-a;K8S_POD_UID=uid-a;"
            "K8S_POD_UID=uid-a;IgnoreUnknown=1;K8S_POD_NAMESPACE=ns;"
            "K8S_POD_NAME=pod-a;K8S_POD_INFRA_CONTAINER_ID=container-a",
        )
        for value in variants:
            with self.subTest(value=value):
                identity = parse_invocation_identity({"CNI_ARGS": value})
                self.assertEqual(identity["K8S_POD_UID"], "uid-a")
                self.assertEqual(identity["K8S_POD_NAME"], "pod-a")
                self.assertEqual(identity["K8S_POD_NAMESPACE"], "ns")

    def test_cni_args_rejects_conflicting_duplicate_identity_key(self):
        with self.assertRaisesRegex(CniIdentityError, "conflicting values"):
            parse_cni_args_identity("K8S_POD_UID=uid-a;K8S_POD_UID=uid-b")

    def test_identical_standalone_and_cni_args_identity_is_accepted(self):
        identity = parse_invocation_identity({
            "CNI_ARGS": "K8S_POD_UID=uid-a",
            "K8S_POD_UID": "uid-a",
        })
        self.assertEqual(identity["K8S_POD_UID"], "uid-a")


def classify(document, *, logical_port="replacement-lsp",
             local_chassis="chassis-1", datapath="datapath-1"):
    with tempfile.TemporaryDirectory() as directory:
        timeline = Path(directory) / "binding-timeline.jsonl"
        result = subprocess.run(
            [
                sys.executable, str(CLASSIFIER),
                "--logical-port", logical_port,
                "--local-chassis", local_chassis,
                "--expected-datapath", datapath,
                "--timeline", str(timeline),
            ],
            input=json.dumps(document),
            text=True,
            capture_output=True,
            check=False,
        )
        return result, json.loads(timeline.read_text())


class ReplacementBindingTests(unittest.TestCase):
    def test_foreign_sentinel_uses_valid_mac_on_separate_non_dynamic_switch(self):
        source = HELPER.read_text()
        setup = source[
            source.index("interop_setup_config() {"):
            source.index("\ninterop_capture_discover() {")
        ]
        self.assertIn('FOREIGN_SWITCH="interop-foreign-${KIND_CLUSTER}"', source)
        self.assertIn("FOREIGN_LSP='interop-foreign-sentinel'", source)
        self.assertIn("other_config:dynamic-routing-vni=", setup)
        self.assertIn('-- ls-add "${FOREIGN_SWITCH}"', setup)
        self.assertIn('-- lsp-add "${FOREIGN_SWITCH}" "${FOREIGN_LSP}"', setup)
        self.assertIn(
            '-- lsp-set-addresses "${FOREIGN_LSP}" 02:00:00:00:fa:02',
            setup,
        )
        self.assertNotIn("lsp-add ls24b interop-bootstrap", setup)
        self.assertNotIn("lsp-set-addresses interop-bootstrap unknown", setup)
        self.assertIn('>"${EVIDENCE_DIR}/foreign-lsp-before.txt"', source)
        self.assertIn('>"${EVIDENCE_DIR}/foreign-lsp-after.txt"', source)
        self.assertIn('cmp "${EVIDENCE_DIR}/foreign-lsp-before.txt"', source)
        self.assertIn("unrelated FDB and neighbor preserved=PASS", source)

    def test_no_replacement_mode_is_exclusive_to_full_interop(self):
        runner = RUNNER.read_text()
        config = runner[:runner.index('if [[ "${EVPN_FRR_INTEROP_TEST}" == 1 ]]; then\n  EVPN_EXPORT_TEST=1')]
        config = config.replace(
            'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"',
            f"ROOT='{ROOT}'",
        )
        cases = (
            ({"EVPN_FRR_INTEROP_NO_REPLACEMENT": "1"}, 1, "requires FRR interop"),
            ({"EVPN_FRR_INTEROP_TEST": "1", "EVPN_FRR_INTEROP_NO_REPLACEMENT": "1",
              "EVPN_FRR_INTEROP_INITIAL_ONLY": "1"}, 1, "cannot be combined"),
            ({"EVPN_FRR_INTEROP_TEST": "1", "EVPN_FRR_INTEROP_NO_REPLACEMENT": "1",
              "EVPN_FRR_INTEROP_BINDING_DIAGNOSTIC": "1"}, 1, "cannot be combined"),
            ({"EVPN_FRR_INTEROP_TEST": "1", "EVPN_FRR_INTEROP_NO_REPLACEMENT": "1"}, 0, None),
        )
        for overrides, expected, message in cases:
            with self.subTest(overrides=overrides):
                env = {
                    **os.environ,
                    "KIND_CLUSTER": "cni-24b-offline",
                    **overrides,
                }
                result = subprocess.run(
                    ["bash", "-c", config + "\nprintf 'CONFIG-VALID\\n'\n"],
                    text=True,
                    capture_output=True,
                    check=False,
                    env=env,
                )
                self.assertEqual(result.returncode, expected, result.stderr)
                if message:
                    self.assertIn(message, result.stderr)
                else:
                    self.assertIn("CONFIG-VALID", result.stdout)

    def test_no_replacement_path_checks_both_del_lifecycles_without_recreating(self):
        source = HELPER.read_text()
        lifecycle = source[
            source.index("interop_verify_no_replacement_lifecycle() {"):
            source.index("\ninterop_binding_diagnostic() {")
        ]
        verify = source[
            source.index("interop_verify() {"):
        ]
        self.assertIn('if [[ "${EVPN_FRR_INTEROP_NO_REPLACEMENT:-0}" == 1 ]]', verify)
        self.assertIn("interop_verify_no_replacement_lifecycle", verify)
        self.assertIn(
            "interop_verify_no_replacement_lifecycle || return 1\n"
            "  elif [[ \"${EVPN_FRR_INTEROP_FULL_LIFECYCLE:-0}\" == 1 ]]; then\n"
            "    interop_verify_full_lifecycle || return 1\n"
            "  else\n    interop_verify_lifecycle || return 1\n  fi",
            verify,
        )
        self.assertIn('delete pod pod-b --wait=true --timeout=120s', lifecycle)
        self.assertIn('delete pod pod-a --wait=true --timeout=90s', lifecycle)
        self.assertEqual(lifecycle.count("interop_assert_successful_cni_del"), 2)
        self.assertGreaterEqual(lifecycle.count("interop_replay_cni_del"), 3)
        self.assertIn('"${LOCAL_MAC}" "${LOCAL_IP}" present pod-b-withdrawal', lifecycle)
        self.assertIn('pod-a-after-pod-b-delete-ping.txt', lifecycle)
        self.assertIn('remote-after-pod-b-delete-ping.txt', lifecycle)
        self.assertIn("interop_wait_local_uuid_withdrawal", lifecycle)
        self.assertIn('absent local-withdrawal', lifecycle)
        self.assertIn('absent remote-withdrawal', lifecycle)
        self.assertIn("interop_capture_northd_health after-pod-a-withdrawal", lifecycle)
        self.assertIn("interop_capture_northd_health after-remote-withdrawal", lifecycle)
        self.assertNotIn("create_pod", lifecycle)
        self.assertNotIn("interop_wait_replacement_binding", lifecycle)
        self.assertNotIn("interop_create_remote_workload", lifecycle)
        self.assertNotIn("neighbor shutdown", lifecycle)

    def test_northd_health_checkpoint_rejects_missing_process_and_crash_log(self):
        source = HELPER.read_text()
        health = source[
            source.index("interop_capture_northd_health() {"):
            source.index("\ninterop_verify_no_replacement_lifecycle() {")
        ]
        self.assertIn("verify-runtime-health", health)
        self.assertIn("--processes", health)
        self.assertIn("--northd-log", health)
        self.assertIn("--baseline-pids", health)
        checker = (ROOT / "tools/lib/cni_evpn_acceptance.py").read_text()
        self.assertIn("SIGSEGV|segmentation fault|signal 11|core dumped", checker)
        self.assertIn("northd log shrank", health)

    def test_successful_del_evidence_accepts_a_later_success_after_failure(self):
        source = HELPER.read_text()
        successful_del = source[
            source.index("interop_assert_successful_cni_del() {"):
            source.index("\ninterop_replay_cni_del() {")
        ]
        self.assertIn('if [ "$rc" = 0 ]; then', successful_del)
        self.assertIn('found="$d"', successful_del)
        self.assertIn('no successful runtime CNI DEL found', successful_del)

    def test_classifier_distinguishes_absent_unbound_wrong_datapath_and_bound(self):
        cases = (
            ({"headings": ["logical_port"], "data": []}, "ABSENT"),
            (binding_snapshot(chassis=None), "UNBOUND"),
            (binding_snapshot(datapath="other-datapath"), "WRONG_DATAPATH"),
            (binding_snapshot(chassis="other-chassis"), "WRONG_CHASSIS"),
            (binding_snapshot(up=False), "DOWN"),
            (binding_snapshot(port_type="patch"), "WRONG_TYPE"),
            (binding_snapshot(), "BOUND"),
        )
        for document, expected in cases:
            with self.subTest(expected=expected):
                result, record = classify(document)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), expected)
                self.assertEqual(record["state"], expected)

    def test_duplicate_matching_bindings_are_not_accepted(self):
        document = binding_snapshot()
        document["data"].append(document["data"][0])
        result, record = classify(document)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "DUPLICATE")
        self.assertEqual(len(record["bindings"]), 2)

    def test_replacement_binding_wait_precedes_route_assertion(self):
        source = HELPER.read_text()
        wait = source[
            source.index("interop_wait_replacement_binding() {"):
            source.index("\ninterop_wait_rib_route() {")
        ]
        lifecycle = source[
            source.index("interop_verify_lifecycle() {"):
            source.index("\ninterop_assert_initial_route_scope() {")
        ]
        sync_check = wait[
            wait.index("ovn-nbctl --wait=sb sync"):
            wait.index("binding-resolution-switches.json")
        ]
        self.assertIn("will still determine binding state", sync_check)
        self.assertIn("classification", sync_check)
        self.assertIn("replacement-binding-timeline.jsonl", wait)
        self.assertIn("deadline=$((SECONDS+90))", wait)
        self.assertIn("interop_wait_replacement_binding", lifecycle)
        self.assertLess(
            lifecycle.index("interop_wait_replacement_binding"),
            lifecycle.index(
                'interop_wait_rib_route "${LOCAL_MAC}" "${LOCAL_IP}" present local-readvertisement'
            ),
        )

    def test_diagnostics_capture_complete_ovn_ovs_and_controller_state(self):
        source = HELPER.read_text()
        diagnostics = source[
            source.index("interop_collect_replacement_binding_diagnostics() {"):
            source.index("\ninterop_wait_replacement_binding() {")
        ]
        for required in (
            "dynamic_addresses,type,options,external_ids",
            "datapath,type,chassis,up,mac,options",
            "external_ids,ofport,error",
            "list Port",
            "list Bridge",
            "list Chassis",
            "Chassis_Private",
            "logs",
            "export-status.json",
        ):
            self.assertIn(required, diagnostics)


class NoReplacementInitialEvidenceTests(unittest.TestCase):
    UIDS = {"pod-a": "uid-a", "pod-b": "uid-b"}
    ADDRESSES = {
        "pod-a": ("0a:58:f0:00:00:0a", "10.244.240.10", "lsp-a", "pb-a", "iface-a", "port-a"),
        "pod-b": ("0a:58:f0:00:00:0b", "10.244.240.11", "lsp-b", "pb-b", "iface-b", "port-b"),
    }

    def _ovsdb(self, headings, rows):
        return json.dumps({"headings": headings, "data": rows})

    def test_real_multus_cni_args_receipt_is_selected(self):
        uid = "a141d81c-5936-40f6-adfb-80f9a9ba9fba"
        helper = HELPER.read_text(encoding="utf-8")
        selector = helper[
            helper.index("interop_find_copied_cni_call() {"):
            helper.index("\ninterop_capture_endpoint_identities() {")
        ]
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory) / "evidence"
            call = evidence / "initial-cni" / "call.pod-a"
            call.mkdir(parents=True)
            (call / "env").write_text(
                "CNI_COMMAND=ADD\n"
                "CNI_ARGS=IgnoreUnknown=true;K8S_POD_NAMESPACE=cni-test;"
                f"K8S_POD_NAME=pod-a;K8S_POD_UID={uid};"
                "IgnoreUnknown=1;K8S_POD_NAMESPACE=cni-test;"
                f"K8S_POD_NAME=pod-a;K8S_POD_UID={uid}\n",
                encoding="utf-8",
            )
            (call / "rc").write_text("0\n", encoding="utf-8")
            result = subprocess.run(
                ["bash", "-c", f"""
set -euo pipefail
ROOT={shlex.quote(str(ROOT))}
EVIDENCE_DIR={shlex.quote(str(evidence))}
{selector}
interop_find_copied_cni_call {shlex.quote(uid)} ADD
"""],
                text=True,
                capture_output=True,
                check=False,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.strip().endswith("call.pod-a"))

    def _write_fixtures(self, root, *, missing=None, malformed=None,
                        mismatched=None, duplicate=None, missing_uid=None,
                        wrong_uid=None, duplicate_uid=None, conflict=None,
                        malformed_args=None, failed=None):
        calls = root / "calls"
        snapshots = root / "snapshots"
        calls.mkdir()
        snapshots.mkdir()
        lsp_rows, binding_rows, interface_rows, port_rows = [], [], [], []
        for pod, uid in self.UIDS.items():
            mac, ip, lsp, binding, interface, port = self.ADDRESSES[pod]
            if pod != missing:
                call = calls / f"call.{pod}"
                call.mkdir()
                receipt_mac = "0a:58:f0:ff:ff:ff" if pod == mismatched else mac
                args_uid = "wrong-uid" if pod == wrong_uid else uid
                args = (
                    "IgnoreUnknown=true;K8S_POD_NAMESPACE=cni-test;"
                    f"K8S_POD_NAME={pod}"
                )
                if pod != missing_uid:
                    args += f";K8S_POD_UID={args_uid}"
                args += (
                    ";IgnoreUnknown=1;K8S_POD_NAMESPACE=cni-test;"
                    f"K8S_POD_NAME={pod}"
                )
                if pod != missing_uid:
                    args += f";K8S_POD_UID={args_uid}"
                if pod == duplicate_uid:
                    args += ";K8S_POD_UID=conflicting-uid"
                if pod == malformed_args:
                    args += ";malformed-field"
                env = f"CNI_COMMAND=ADD\nCNI_ARGS={args}\n"
                if pod == conflict:
                    env += f"K8S_POD_UID=conflicting-{uid}\n"
                (call / "env").write_text(env)
                (call / "rc").write_text("1\n" if pod == failed else "0\n")
                (call / "config").write_text(json.dumps({
                    "macAddress": mac,
                    "ipAddress": f"{ip}/24",
                }))
                (call / "stdout").write_text(
                    "not-json\n" if pod == malformed else json.dumps({
                        "interfaces": [{"name": "net1", "mac": receipt_mac}],
                        "ips": [{"interface": 0, "address": f"{ip}/24"}],
                    })
                )
                if pod == duplicate:
                    duplicate_call = calls / f"call.{pod}-duplicate"
                    duplicate_call.mkdir()
                    for name in ("env", "rc", "config", "stdout"):
                        (duplicate_call / name).write_bytes((call / name).read_bytes())

            lsp_rows.append([f"uuid-{lsp}", lsp, {
                "evpn.framework/workload-uid": uid,
            }])
            binding_rows.append([f"uuid-{binding}", lsp])
            interface_rows.append([f"uuid-{interface}", interface, {
                "evpn.framework/workload-uid": uid,
                "iface-id": lsp,
            }])
            port_rows.append([f"uuid-{port}", port, [f"uuid-{interface}"]])

        files = {
            "acceptance-nb-lsps-before-delete.json": self._ovsdb(
                ["_uuid", "name", "external_ids"], lsp_rows
            ),
            "acceptance-sb-bindings-before-delete.json": self._ovsdb(
                ["_uuid", "logical_port"], binding_rows
            ),
            "acceptance-ovs-interfaces-before-delete.json": self._ovsdb(
                ["_uuid", "name", "external_ids"], interface_rows
            ),
            "acceptance-ovs-ports-before-delete.json": self._ovsdb(
                ["_uuid", "name", "interfaces"], port_rows
            ),
            "acceptance-ovs-bridges-before-delete.json": self._ovsdb(
                ["_uuid", "name", "ports"],
                [["uuid-br-int", "br-int", [f"uuid-{item[5]}" for item in self.ADDRESSES.values()]]],
            ),
        }
        for name, content in files.items():
            (snapshots / name).write_text(content)
        return calls, snapshots

    def test_failed_add_status_is_rejected_before_identity_parsing(self):
        with tempfile.TemporaryDirectory() as directory:
            calls, snapshots = self._write_fixtures(
                Path(directory), failed="pod-b", missing_uid="pod-b"
            )
            result = subprocess.run(
                [
                    sys.executable, str(ROOT / "tools/lib/cni_evpn_acceptance.py"),
                    "capture-identity",
                    "--call-dir", str(calls / "call.pod-b"),
                    "--uid", self.UIDS["pod-b"],
                    "--pod-namespace", "cni-test",
                    "--pod-name", "pod-b",
                    "--rd", "198.18.0.2:1000",
                    "--vni", "1000",
                    "--nb-lsps", str(snapshots / "acceptance-nb-lsps-before-delete.json"),
                    "--sb-bindings", str(snapshots / "acceptance-sb-bindings-before-delete.json"),
                    "--ovs-interfaces", str(
                        snapshots / "acceptance-ovs-interfaces-before-delete.json"
                    ),
                    "--ovs-ports", str(snapshots / "acceptance-ovs-ports-before-delete.json"),
                    "--output", str(Path(directory) / "identity.json"),
                ],
                text=True,
                capture_output=True,
                check=False,
            )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not from a successful CNI ADD", result.stderr)
        self.assertNotIn("K8S_POD_UID", result.stderr)

    def _write_retained_receipt_fixtures(self, root):
        # Synthetic successful ADD receipts and OVSDB ownership snapshots retain
        # duplicated Multus identity fields without importing historical evidence.
        calls, snapshots = self._write_fixtures(root)
        return calls, snapshots, self.UIDS

    def _run_dispatch(self, *, no_replacement=True, missing=None, malformed=None,
                      mismatched=None, duplicate=None, missing_uid=None,
                      wrong_uid=None, duplicate_uid=None, conflict=None,
                      malformed_args=None, failed=None, calls_dir=None,
                      snapshots_dir=None, uids=None):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        if calls_dir is None or snapshots_dir is None:
            calls_dir, snapshots_dir = self._write_fixtures(
                root, missing=missing, malformed=malformed,
                mismatched=mismatched, duplicate=duplicate,
                missing_uid=missing_uid, wrong_uid=wrong_uid,
                duplicate_uid=duplicate_uid, conflict=conflict,
                malformed_args=malformed_args, failed=failed,
            )
        selected_uids = uids or self.UIDS
        uid_declaration = (
            "declare -A UIDS=([pod-a]=" + shlex.quote(selected_uids["pod-a"])
            + " [pod-b]=" + shlex.quote(selected_uids["pod-b"]) + ")"
        )
        helper = HELPER.read_text(encoding="utf-8")
        functions = []
        for start, end in (
            ("interop_find_copied_cni_call() {", "\ninterop_capture_endpoint_identities() {"),
            ("interop_capture_endpoint_identities() {", "\ninterop_prepare_no_replacement_lifecycle() {"),
            ("interop_prepare_no_replacement_lifecycle() {", "\ninterop_wait_endpoint_route_transition() {"),
            ("interop_run_lifecycle() {", None),
        ):
            begin = helper.index(start)
            finish = helper.index(end, begin) if end else len(helper)
            functions.append(helper[begin:finish])
        script = """
set -euo pipefail
ROOT=@ROOT@
EVIDENCE_DIR=@EVIDENCE@
FIXTURES=@SNAPSHOTS@
CALLS=@CALLS@
KIND_CLUSTER=offline
OVN_POD=ovn-runtime
LOCAL_VTEP=198.18.0.2
VNI=1000
EVPN_FRR_INTEROP_INITIAL_ONLY=0
EVPN_FRR_INTEROP_NO_REPLACEMENT=@NO_REPLACEMENT@
copy_calls() {
  printf 'copy-receipts\\n'
  mkdir -p "$1"
  cp -R "$CALLS"/call.* "$1"/ 2>/dev/null || true
}
k() {
  local pod=
  for arg in "$@"; do
    case "$arg" in pod-a|pod-b) pod="$arg";; esac
  done
  printf '%s\\n' "${UIDS[$pod]}"
}
@UIDS@
interop_capture_diagnostic() {
  printf 'snapshot:%s\\n' "$1"
  cp "$FIXTURES/$1" "$EVIDENCE_DIR/$1"
}
interop_verify_no_replacement_lifecycle() {
  test -s "$EVIDENCE_DIR/endpoint-a-identity.json"
  test -s "$EVIDENCE_DIR/endpoint-b-identity.json"
  python3 - "$EVIDENCE_DIR/endpoint-a-identity.json" "$EVIDENCE_DIR/endpoint-b-identity.json" \
    "${UIDS[pod-a]}" "${UIDS[pod-b]}" <<'PY'
import json,sys
assert [json.load(open(path))["uid"] for path in sys.argv[1:3]] == sys.argv[3:5]
PY
  printf 'lifecycle-start\\n'
}
interop_verify_lifecycle() { printf 'replacement-lifecycle-start\\n'; }
@FUNCTIONS@
interop_run_lifecycle
"""
        with tempfile.TemporaryDirectory() as evidence_dir:
            script = (script
                .replace("@ROOT@", shlex.quote(str(ROOT)))
                .replace("@EVIDENCE@", shlex.quote(evidence_dir))
                .replace("@SNAPSHOTS@", shlex.quote(str(snapshots_dir)))
                .replace("@CALLS@", shlex.quote(str(calls_dir)))
                .replace("@NO_REPLACEMENT@", "1" if no_replacement else "0")
                .replace("@UIDS@", uid_declaration)
                .replace("@FUNCTIONS@", "\n".join(functions)))
            return subprocess.run(
                ["bash", "-c", script],
                text=True,
                capture_output=True,
                check=False,
            )

    def test_receipt_and_identity_collection_precede_lifecycle(self):
        result = self._run_dispatch()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        events = result.stdout.splitlines()
        self.assertLess(events.index("copy-receipts"),
                        events.index("snapshot:acceptance-nb-lsps-before-delete.json"))
        self.assertLess(
            events.index("snapshot:acceptance-ovs-bridges-before-delete.json"),
            events.index("lifecycle-start"),
        )
        self.assertEqual(events.count("lifecycle-start"), 1)
        self.assertNotIn("replacement-lifecycle-start", events)

    def test_retained_receipts_and_ownership_snapshots_reach_lifecycle_boundary(self):
        with tempfile.TemporaryDirectory() as directory:
            calls, snapshots, uids = self._write_retained_receipt_fixtures(
                Path(directory)
            )
            result = self._run_dispatch(
                calls_dir=calls, snapshots_dir=snapshots, uids=uids
            )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        events = result.stdout.splitlines()
        self.assertIn("copy-receipts", events)
        self.assertEqual(events.count("lifecycle-start"), 1)
        self.assertNotIn("replacement-lifecycle-start", events)

    def test_missing_receipt_blocks_lifecycle(self):
        result = self._run_dispatch(missing="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("expected one successful ADD call", result.stderr)

    def test_missing_uid_blocks_lifecycle(self):
        result = self._run_dispatch(missing_uid="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("missing K8S_POD_UID", result.stderr)

    def test_wrong_uid_blocks_lifecycle(self):
        result = self._run_dispatch(wrong_uid="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("expected one successful ADD call", result.stderr)

    def test_conflicting_duplicate_uid_key_blocks_lifecycle(self):
        result = self._run_dispatch(duplicate_uid="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("conflicting values for K8S_POD_UID", result.stderr)

    def test_conflicting_standalone_and_cni_args_uid_blocks_lifecycle(self):
        result = self._run_dispatch(conflict="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("conflicting standalone and CNI_ARGS", result.stderr)

    def test_malformed_cni_args_blocks_lifecycle(self):
        result = self._run_dispatch(malformed_args="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("CNI_ARGS field", result.stderr)

    def test_failed_add_receipt_is_not_selected(self):
        result = self._run_dispatch(failed="pod-b", missing_uid="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("expected one successful ADD call", result.stderr)
        self.assertNotIn("missing K8S_POD_UID", result.stderr)

    def test_malformed_receipt_blocks_lifecycle(self):
        result = self._run_dispatch(malformed="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("CNI ADD result is unreadable or malformed", result.stderr)

    def test_mismatched_cni_endpoint_identity_blocks_lifecycle(self):
        result = self._run_dispatch(mismatched="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("config and successful result disagree", result.stderr)

    def test_duplicate_successful_receipt_blocks_lifecycle(self):
        result = self._run_dispatch(duplicate="pod-b")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("lifecycle-start", result.stdout)
        self.assertIn("found 2", result.stderr)

    def test_replacement_mode_dispatch_is_unchanged(self):
        result = self._run_dispatch(no_replacement=False)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("replacement-lifecycle-start", result.stdout)
        self.assertNotIn("copy-receipts", result.stdout)
        self.assertNotIn("\nlifecycle-start\n", "\n" + result.stdout)


if __name__ == "__main__":
    unittest.main()
