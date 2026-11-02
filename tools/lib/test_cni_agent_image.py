#!/usr/bin/env python3
"""Offline ownership and runner tests; every Docker command is stubbed."""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

LIB = Path(__file__).resolve().parent
ROOT = LIB.parents[1]
PIN = "sha256:cba1675ad8093310d116f5befb91949eba970b165626d3472a17c8e4e877ae82"
REF = "evpn4ovn/node-agent-test:cni-24b-d22e-20261027200001-r1"


class ImageTests(unittest.TestCase):
    def run_shell(self, body, tags=None, ids=None, platform="linux/amd64", runner=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            state = {"tags": tags if tags is not None else {REF: PIN},
                     "ids": ids if ids is not None else [PIN],
                     "platform": platform}
            (root / "state.json").write_text(json.dumps(state))
            stub = root / "docker"
            stub.write_text("""#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
p=Path(os.environ["MOCK_ROOT"])
s=json.loads((p/"state.json").read_text())
a=sys.argv[1:]
with (p/"calls").open("a") as f: f.write(json.dumps(a)+"\\n")
if a[:2]==["image","inspect"]:
    ref=a[2]
    if ref not in s["tags"]:
        print("Error response from daemon: No such image: "+ref,file=sys.stderr); sys.exit(1)
    id=s["tags"][ref]
    os_,arch=s["platform"].split("/")
    print(json.dumps([{"Id":id,"Os":os_,"Architecture":arch,
        "RepoTags":[r for r,i in s["tags"].items() if i==id],"RepoDigests":[]}]))
elif a[:2]==["image","ls"]: print("\\n".join(s["ids"]))
elif a[:2]==["image","rm"]:
    del s["tags"][a[2]]
elif a[:1]==["tag"]:
    assert a[2] not in s["tags"]
    s["tags"][a[2]]=a[1]
else:
    print("UNEXPECTED Docker command: "+repr(a),file=sys.stderr); sys.exit(99)
(p/"state.json").write_text(json.dumps(s))
""")
            stub.chmod(0o755)
            env = {**os.environ, "MOCK_ROOT": directory,
                   "PATH": directory + ":" + os.environ["PATH"]}
            prefix = "" if runner else f"source '{LIB}/cni-agent-image-test.sh'\nAGENT_IMAGE='{REF}'\n"
            result = subprocess.run(["bash", "-c", "set -euo pipefail\n" + prefix + body],
                                    env=env, text=True, capture_output=True)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()] \
                if (root / "calls").exists() else []
            return result, json.loads((root / "state.json").read_text()), calls

    def test_pinned_and_preexisting_tag_survive(self):
        result, state, calls = self.run_shell("test_image_pinned_preflight; test_image_cleanup")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["tags"], {REF: PIN})
        self.assertFalse(any(c[:2] == ["image", "rm"] for c in calls))

    def test_owned_temporary_tag_removed_idempotently(self):
        body = """
test_image_snapshot
test_image_require_new_tag temporary
docker tag "$PINNED_AGENT_ID" temporary
test_image_record_created temporary
[[ "${TEST_IMAGE_TAG_OWNER[temporary]}" == tag ]]
test_image_cleanup
test_image_cleanup
"""
        result, state, calls = self.run_shell(body)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["tags"], {REF: PIN})
        self.assertEqual(sum(c[:2] == ["image", "rm"] for c in calls), 1)

    def test_mismatch_blocks_removal(self):
        result, state, calls = self.run_shell("""
TEST_IMAGE_TAG_OWNER[temporary]=tag
TEST_IMAGE_TAG_ID[temporary]="$PINNED_AGENT_ID"
test_image_cleanup
""", tags={REF: PIN, "temporary": "sha256:other"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("target changed", result.stderr)
        self.assertIn("temporary", state["tags"])
        self.assertFalse(any(c[:2] == ["image", "rm"] for c in calls))

    def test_unknown_ownership_blocks_removal(self):
        result, state, calls = self.run_shell("""
TEST_IMAGE_TAG_OWNER[temporary]=pending
test_image_cleanup
""", tags={"temporary": PIN})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ownership review", result.stderr)
        self.assertIn("temporary", state["tags"])
        self.assertEqual(calls, [])

    def test_last_reference_to_preexisting_image_is_preserved(self):
        result, state, calls = self.run_shell("""
TEST_IMAGE_TAG_OWNER[temporary]=tag
TEST_IMAGE_TAG_ID[temporary]="$PINNED_AGENT_ID"
test_image_cleanup
""", tags={"temporary": PIN})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("last reference", result.stderr)
        self.assertIn("temporary", state["tags"])
        self.assertFalse(any(c[:2] == ["image", "rm"] for c in calls))

    def test_new_image_with_external_reference_preserves_external(self):
        result, state, _ = self.run_shell("""
TEST_IMAGE_TAG_OWNER[temporary]=image
TEST_IMAGE_TAG_ID[temporary]="$PINNED_AGENT_ID"
test_image_cleanup
""", tags={REF: PIN, "temporary": PIN})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["tags"], {REF: PIN})

    def test_missing_pinned_image_blocks_without_fallback(self):
        result, _, calls = self.run_shell("test_image_pinned_preflight", tags={})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [["image", "inspect", REF]])

    def test_wrong_id_and_platform_block(self):
        for tags, platform in (({REF: "sha256:other"}, "linux/amd64"),
                               ({REF: PIN}, "linux/arm64")):
            with self.subTest(tags=tags, platform=platform):
                result, _, calls = self.run_shell("test_image_pinned_preflight",
                                                  tags=tags, platform=platform)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("identity mismatch", result.stderr)
                self.assertEqual(calls, [["image", "inspect", REF]])

    def test_preexisting_tag_cannot_be_overwritten(self):
        result, state, calls = self.run_shell('test_image_require_new_tag "$AGENT_IMAGE"')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["tags"], {REF: PIN})
        self.assertEqual(calls, [["image", "inspect", REF]])

    def test_preexisting_base_target_mismatch_cannot_be_overwritten(self):
        result, state, calls = self.run_shell("""
BASE_ID="$PINNED_AGENT_ID"
test_image_ensure_base
""", tags={"evpn4ovn/node-agent-test:cni-24b-base": "sha256:other"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unexpected target", result.stderr)
        self.assertEqual(len(state["tags"]), 1)
        self.assertFalse(any(c[0] == "tag" for c in calls))

    def test_missing_owned_tag_cleanup_is_idempotent(self):
        result, state, calls = self.run_shell("""
TEST_IMAGE_TAG_OWNER[temporary]=tag
TEST_IMAGE_TAG_ID[temporary]="$PINNED_AGENT_ID"
test_image_cleanup
test_image_cleanup
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["tags"], {REF: PIN})
        self.assertEqual(calls, [["image", "inspect", "temporary"]])

    def test_daemon_error_is_not_treated_as_missing(self):
        result, _, _ = self.run_shell("""
docker() { echo 'permission denied' >&2; return 1; }
test_image_require_new_tag temporary
""")
        self.assertEqual(result.returncode, 2)
        self.assertIn("inspection failed", result.stderr)

    def test_created_image_requires_ownership_snapshot(self):
        result, _, calls = self.run_shell('test_image_record_created "$AGENT_IMAGE"')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot establish", result.stderr)
        self.assertEqual(calls, [])

    def test_new_build_and_cached_build_ownership_differ(self):
        for ids, expected in (([], "image"), ([PIN], "tag")):
            with self.subTest(ids=ids):
                result, _, _ = self.run_shell(f"""
test_image_snapshot
TEST_IMAGE_TAG_OWNER["$AGENT_IMAGE"]=pending
test_image_record_created "$AGENT_IMAGE"
[[ "${{TEST_IMAGE_TAG_OWNER[$AGENT_IMAGE]}}" == {expected} ]]
""", ids=ids)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_runner_initial_mode_never_builds_or_pulls(self):
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        # Execute the real pre-runtime runner prefix with all infrastructure stubbed.
        prefix = runner[:runner.index('CLUSTER_CREATE_ATTEMPTED=1')]
        prefix = prefix.replace("\ne2e_lab_acquire\n", "\ne2e_lab_acquire() { :; }\n")
        prefix = prefix.replace('ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"',
                                f"ROOT='{ROOT}'")
        prefix = prefix.replace(
            "interop_capture_preflight || {",
            "interop_capture_preflight() { INTEROP_CAPTURE_PREFLIGHT_STATUS=PASS; }\n"
            "interop_capture_preflight || {",
        )
        body = """
export EVPN_FRR_INTEROP_TEST=1 EVPN_FRR_INTEROP_INITIAL_ONLY=1
export KIND_CLUSTER=cni-24b-mocked
export EVIDENCE_DIR="$MOCK_ROOT/evidence"
kind() { :; }
go() { echo 'UNEXPECTED go build' >&2; return 99; }
curl() { : > "$EVIDENCE_DIR/multus.tar.gz"; }
sha256sum() { :; }
tar() { :; }
"""
        # OVN ID gate uses --format, so intercept only that exact read.
        body += """
docker() {
  if [[ "$*" == "image inspect evpn4ovn/ovn-ovs:kind-26.09 --format {{.Id}}" ]]; then
    echo sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505
  elif [[ "$1 $2" == "ps -a" || "$1 $2" == "network ls" ]]; then
    :
  elif [[ "$*" == "image inspect $AGENT_IMAGE --format {{.Id}}" ]]; then
    echo sha256:cba1675ad8093310d116f5befb91949eba970b165626d3472a17c8e4e877ae82
  else
    command docker "$@"
  fi
}
"""
        result, state, calls = self.run_shell(body + prefix + """
[[ "$EVPN_AGENT_IMAGE_MODE" == pinned ]]
[[ "${TEST_IMAGE_TAG_OWNER[$AGENT_IMAGE]}" == external ]]
""", runner=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state["tags"], {REF: PIN})
        self.assertEqual(calls, [["image", "inspect", REF]])

    def test_interop_mode_does_not_dispatch_noninterop_import_setup(self):
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        dispatch = 'if [[ "${EVPN_IMPORT_TEST:-0}" == 1 && "${EVPN_FRR_INTEROP_TEST}" != 1 ]]; then import_setup_config; fi'
        self.assertIn(dispatch, runner)
        for interop, import_test, expected in ((1, 1, ""), (0, 1, "called\n"), (1, 0, "")):
            with self.subTest(interop=interop, import_test=import_test):
                result = subprocess.run(
                    ["bash", "-c",
                     'import_setup_config() { echo called; }\n' + dispatch],
                    env={**os.environ,
                         "EVPN_FRR_INTEROP_TEST": str(interop),
                         "EVPN_IMPORT_TEST": str(import_test)},
                    capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, expected)

    def test_precluster_cleanup_failure_propagates(self):
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        cleanup = runner[runner.index("cleanup() {"):runner.index("\ntrap cleanup EXIT")]
        result, _, calls = self.run_shell("""
CREATED=0 EVPN_FRR_INTEROP_TEST=0
cleanup_cluster() { :; }
test_image_cleanup() { echo 'mock cleanup requires review' >&2; return 1; }
""" + cleanup + "\ntrap cleanup EXIT\nexit 0\n")
        self.assertEqual(result.returncode, 1)
        self.assertIn("requires review", result.stderr)
        self.assertEqual(calls, [])

    def test_interop_cleanup_requires_recorded_identity_and_label(self):
        baseline = {"Id": "owned-id", "Name": "/cni-24b-offline-frr",
                    "Config": {"Labels": {"evpn4ovn.test-run": "cni-24b-offline"}}}
        cases = [("owned", baseline, True)]
        for field, value in (("Id", "foreign-id"), ("Name", "/foreign"),
                             ("Config", {"Labels": {}})):
            metadata = {**baseline, field: value}
            cases.append((field, metadata, False))
        cases.append(("missing-record", baseline, False))
        for label, metadata, allowed in cases:
            with self.subTest(case=label):
                result, _, calls = self.run_shell(f"""
KIND_CLUSTER=cni-24b-offline
export PYTHONOPTIMIZE=1
source '{LIB}/cni-evpn-frr-interop-test.sh'
[[ {shlex.quote(label)} == missing-record ]] || echo owned-id > "$MOCK_ROOT/owned-id"
docker() {{
  if [[ "$1 $2" == 'container inspect' ]]; then
    if [[ -e "$MOCK_ROOT/removed" ]]; then echo "No such container: $3" >&2; return 1; fi
    printf '%s\\n' {shlex.quote(json.dumps([metadata]))}
  elif [[ "$1 $2" == 'rm -f' && "$3" == owned-id ]]; then
    echo "REMOVE:$3" >> "$MOCK_ROOT/removed"
  else echo 'UNEXPECTED' >&2; return 99; fi
}}
interop_remove_owned container "$FRR_CONTAINER" "$MOCK_ROOT/owned-id"
interop_remove_owned container "$FRR_CONTAINER" "$MOCK_ROOT/owned-id"
cat "$MOCK_ROOT/removed"
""")
                self.assertEqual(result.returncode == 0, allowed, result.stderr)
                self.assertEqual(result.stdout.count("REMOVE:owned-id"), int(allowed))
                self.assertEqual(calls, [])

    def test_interop_network_with_unrelated_members_is_preserved(self):
        metadata = {"Id": "network-id", "Name": "evpn33b3-cni-24b-offline",
                    "Labels": {"evpn4ovn.test-run": "cni-24b-offline"},
                    "Containers": {"foreign": {"Name": "unrelated"}}}
        result, _, _ = self.run_shell(f"""
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
echo network-id > "$MOCK_ROOT/id"
docker() {{
  [[ "$1 $2" == 'network inspect' ]] || {{ echo 'UNEXPECTED removal' >&2; return 99; }}
  printf '%s\\n' {shlex.quote(json.dumps([metadata]))}
}}
interop_remove_owned network "$INTEROP_NETWORK" "$MOCK_ROOT/id"
""")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ownership review", result.stderr)
        self.assertNotIn("UNEXPECTED", result.stderr)

    def test_interop_inspection_error_is_not_successful_cleanup(self):
        result, _, _ = self.run_shell(f"""
KIND_CLUSTER=cni-24b-offline
source '{LIB}/cni-evpn-frr-interop-test.sh'
docker() {{ echo 'permission denied' >&2; return 1; }}
interop_remove_owned container "$FRR_CONTAINER" "$MOCK_ROOT/id"
""")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("inspection failed", result.stderr)

    def test_underlay_setup_requires_common_lifecycle_readiness(self):
        result, _, calls = self.run_shell(f"""
KIND_CLUSTER=cni-24b-offline
EVIDENCE_DIR="$MOCK_ROOT"
ROOT={shlex.quote(str(ROOT))}
source '{LIB}/cni-evpn-frr-interop-test.sh'
python3() {{ echo 'mock Containerlab readiness failure' >&2; return 42; }}
docker() {{
  case "$1 $2" in
    'image inspect') echo "$FRR_IMAGE_ID";;
    *) echo UNEXPECTED >&2; return 99;;
  esac
}}
trap 'rc=$?; [[ "$INTEROP_CREATED" == 0 ]]; echo underlay-not-claimed; exit "$rc"' EXIT
interop_setup_underlay
""")
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertIn("underlay-not-claimed", result.stdout)
        self.assertIn("mock Containerlab readiness failure", result.stderr)
        self.assertNotIn("UNEXPECTED", result.stderr)
        self.assertEqual(calls, [])


class RunnerTests(unittest.TestCase):
    """Execute the actual startup/EXIT path with fail-closed infrastructure stubs."""

    def run_runner(self, name="cni-24b-offline", create_rc=37, residual=False,
                   extra="", before="", nodes_before=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            state = {"create_rc": create_rc, "residual": residual,
                     "nodes": nodes_before, "name": name, "deleted": False}
            (root / "state.json").write_text(json.dumps(state))
            stub = root / "stub"
            stub.write_text("""#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
p=Path(os.environ["MOCK_ROOT"]); s=json.loads((p/"state.json").read_text())
cmd=Path(sys.argv[0]).name; a=sys.argv[1:]
with (p/"calls").open("a") as f: f.write(json.dumps([cmd,*a])+"\\n")
def unexpected():
    print("UNEXPECTED infrastructure command: "+repr([cmd,*a]),file=sys.stderr)
    sys.exit(99)
if cmd=="docker":
    if a[:2]==["image","inspect"]:
        ref=a[2]
        id=("sha256:c593278d6b9fffcd51a7b355bd80efca291eda4bc7848c3dd838078ef5109505"
            if ref=="evpn4ovn/ovn-ovs:kind-26.09" else
            "sha256:cba1675ad8093310d116f5befb91949eba970b165626d3472a17c8e4e877ae82")
        if "--format" in a: print(id)
        else: print(json.dumps([{"Id":id,"Os":"linux","Architecture":"amd64"}]))
    elif a[:2]==["ps","-a"]:
        if "--filter" in a and s["nodes"]:
            for role,c in (("control-plane","a"),("worker","b")):
                print(c*64+" "+s["name"]+"-"+role+" "+s["name"])
        elif "--filter" not in a: print("external untouched-container external-image")
    elif a[:2]==["network","ls"]: print("external untouched-network bridge")
    else: unexpected()
elif cmd=="kind":
    if a==["get","clusters"]: print("kind0\\nkind-ovn2603")
    elif a[:2]==["create","cluster"]:
        s["name"]=a[a.index("--name")+1]
        s["nodes"]=s["residual"] or s["create_rc"]==0
        (p/"state.json").write_text(json.dumps(s))
        if s["create_rc"]: print("mock kind rejected creation (original error)",file=sys.stderr)
        sys.exit(s["create_rc"])
    elif a[:2]==["delete","cluster"]:
        assert s["nodes"] and a[2:4]==["--name",s["name"]]
        s["nodes"]=False; s["deleted"]=True
    else: unexpected()
elif cmd=="curl":
    Path(a[a.index("-o")+1]).write_bytes(b"mock release archive")
elif cmd in ("sha256sum","tar"): pass
else: unexpected()
(p/"state.json").write_text(json.dumps(s))
""")
            stub.chmod(0o755)
            for cmd in ("docker", "kind", "curl", "sha256sum", "tar", "go", "kubectl",
                        "sudo", "systemctl", "systemd-run", "tcpdump", "nsenter"):
                (root / cmd).symlink_to(stub)
            runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
            prefix = runner[:runner.index('WORKER="${KIND_CLUSTER}-worker"')]
            # Stub the shared lifecycle boundary, never execute a real Containerlab.
            prefix = prefix.replace("\ne2e_lab_acquire\n", """
e2e_lab_acquire() { :; }
e2e_lab_up() { kind create cluster --name "$KIND_CLUSTER"; }
e2e_lab_down() {
  local current
  current="$(cluster_nodes)" || return 1
  [[ -n "$current" ]] || return 0
  if [[ "$CREATED" != 1 || "$current" != "$CLUSTER_OWNED_NODES" ]]; then
    echo "mock Containerlab ownership review required" >&2; return 1
  fi
  kind delete cluster --name "$KIND_CLUSTER"
}
""")
            prefix = prefix.replace('ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"',
                                    f"ROOT='{ROOT}'")
            prefix = prefix.replace(
                "interop_capture_preflight || {",
                "interop_capture_preflight() { INTEROP_CAPTURE_PREFLIGHT_STATUS=PASS; }\n"
                "interop_capture_preflight || {",
            )
            env = {**os.environ, "MOCK_ROOT": directory,
                   "PATH": directory + ":" + os.environ["PATH"],
                   "EVIDENCE_DIR": str(root / "evidence"), "EVPN_FRR_INTEROP_TEST": "1",
                   "EVPN_FRR_INTEROP_INITIAL_ONLY": "1", "KEEP_CLUSTER": "0"}
            if name is None:
                env.pop("KIND_CLUSTER", None)
            else:
                env["KIND_CLUSTER"] = name
            result = subprocess.run(["bash", "-c", before + "\n" + prefix + "\n" + extra],
                                    text=True, capture_output=True, env=env)
            calls = [json.loads(x) for x in (root / "calls").read_text().splitlines()] \
                if (root / "calls").exists() else []
            self.assertNotIn("UNEXPECTED", result.stderr)
            self.assertNotIn("unbound variable", result.stderr)
            evidence = {p.name: p.read_text() for p in (root / "evidence").glob("*") if p.is_file()}
            status = json.loads(evidence["interop-status.json"])
            return result, status, calls, evidence, json.loads((root / "state.json").read_text())

    def test_invalid_supplied_names_fail_before_any_infrastructure_command(self):
        for name in ("cni-24b-d22f-20261027T230445-r1", "cni-24b-bad_name",
                     "unowned-name", "cni-24b-end-", "cni-24b-bad..label", "cni-24b-a.-b"):
            with self.subTest(name=name):
                result, status, calls, _, _ = self.run_runner(name)
                self.assertEqual(result.returncode, 1)
                self.assertIn("invalid disposable cluster name: " + name, result.stderr)
                self.assertEqual(calls, [])
                self.assertEqual(status["overall_status"], "BLOCKED")
                self.assertEqual(status["cluster_creation_status"], "NOT_STARTED")
                self.assertEqual(status["cluster_cleanup_status"], "SKIPPED")
                self.assertEqual(status["resource_cleanup_status"], "PASS")
                self.assertFalse(status["resources_created"])

    def test_length_limit_and_valid_derived_names(self):
        for length in (49, 50, 51, 128, 256):
            name = "cni-24b-" + "x" * (length - 8)
            with self.subTest(length=length):
                result, status, calls, _, _ = self.run_runner(name)
                creates = [c for c in calls if c[:3] == ["kind", "create", "cluster"]]
                self.assertEqual(len(creates), int(length <= 49))
                if length <= 49:
                    self.assertLessEqual(len(name + "-control-plane"), 63)
                    self.assertLessEqual(len("cni-d22d-" + name + "-remote-workload-peer.service"), 255)
                else:
                    self.assertIn("maximum 49", result.stderr)
                    self.assertEqual(status["cluster_creation_status"], "NOT_STARTED")

    def test_generated_names_are_lowercase_and_collision_resistant(self):
        runner = (ROOT / "tools/run-cni-mvp-2.4b-multus.sh").read_text()
        functions = runner[runner.index("generate_cluster_name() {"):runner.index('KIND_CLUSTER="${KIND_CLUSTER:-')]
        result = subprocess.run(["bash", "-c", "set -euo pipefail\n" + functions + """
date() { echo 20261027235959; }
for i in {1..32}; do KIND_CLUSTER="$(generate_cluster_name)"; validate_cluster_name; echo "$KIND_CLUSTER"; done
"""], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        names = result.stdout.splitlines()
        self.assertEqual(len(set(names)), 32)
        self.assertTrue(all(re.fullmatch(r"cni-24b-20261027235959-[0-9a-f]{12}", n) for n in names))

    def test_run_metadata_correlates_all_derived_identifiers(self):
        result, status, calls, evidence, _ = self.run_runner("cni-24b-correlated")
        self.assertEqual(result.returncode, 37)
        self.assertEqual(status["run_id"], "cni-24b-correlated")
        self.assertIn("run=cni-24b-correlated\nkind-cluster=cni-24b-correlated\n",
                      evidence["run-metadata.txt"])
        # Source-only check of names used by real resource creation; no runtime operations.
        script = f"""set -euo pipefail
KIND_CLUSTER=cni-24b-correlated
source '{LIB}/cni-evpn-frr-interop-test.sh'
[[ "$FRR_CONTAINER" == cni-24b-correlated-frr ]]
[[ "$INTEROP_NETWORK" == evpn33b3-cni-24b-correlated ]]
"""
        check = subprocess.run(["bash", "-c", script], text=True, capture_output=True)
        self.assertEqual(check.returncode, 0, check.stderr)
        self.assertTrue(any("label=io.x-k8s.kind.cluster=cni-24b-correlated" in c for c in calls))

    def test_default_generated_name_reaches_only_mocked_kind(self):
        result, status, calls, evidence, _ = self.run_runner(None)
        self.assertEqual(result.returncode, 37)
        self.assertRegex(status["run_id"], r"^cni-24b-[0-9]{14}-[a-f0-9]{12}$")
        self.assertIn("run=" + status["run_id"], evidence["run-metadata.txt"])
        self.assertEqual(sum(c[:3] == ["kind", "create", "cluster"] for c in calls), 1)

    def test_rejected_creation_preserves_original_code_without_deleting_planned_cluster(self):
        result, status, calls, _, state = self.run_runner()
        self.assertEqual(result.returncode, 37)
        self.assertIn("mock kind rejected creation (original error)", result.stderr)
        self.assertEqual(status["original_exit_code"], 37)
        self.assertFalse(status["resources_created"])
        self.assertEqual(status["exit_code"], 37)
        self.assertEqual(status["cleanup_exit_code"], 0)
        self.assertEqual(status["cluster_creation_status"], "FAIL")
        self.assertEqual(status["control_plane_status"], "NOT_RUN")
        self.assertEqual(status["packet_verification_status"], "NOT_RUN")
        self.assertFalse(state["deleted"])
        self.assertFalse(any(c[:3] == ["kind", "delete", "cluster"] for c in calls))

    def test_failed_creation_with_residual_unknown_nodes_preserves_them(self):
        result, status, calls, _, state = self.run_runner(residual=True)
        self.assertEqual(result.returncode, 37)
        self.assertTrue(state["nodes"])
        self.assertFalse(state["deleted"])
        self.assertIn("ownership review", result.stderr)
        self.assertEqual(status["resource_cleanup_status"], "FAIL")
        self.assertEqual(status["cleanup_exit_code"], 1)
        self.assertEqual(status["original_exit_code"], 37)
        self.assertIsNone(status["resources_created"])

    def test_preexisting_node_containers_are_not_claimed_or_deleted(self):
        result, status, calls, _, state = self.run_runner(nodes_before=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("pre-existing kind node", result.stderr)
        self.assertTrue(state["nodes"])
        self.assertFalse(state["deleted"])
        self.assertFalse(any(c[:3] in (["kind", "create", "cluster"],
                                      ["kind", "delete", "cluster"]) for c in calls))

    def test_successful_creation_cleanup_before_worker_assignment(self):
        result, status, _, _, state = self.run_runner(create_rc=0, extra="exit 23")
        self.assertEqual(result.returncode, 23)
        self.assertTrue(state["deleted"])
        self.assertFalse(state["nodes"])
        self.assertEqual(status["resource_cleanup_status"], "PASS")
        self.assertEqual(status["original_exit_code"], 23)
        self.assertTrue(status["resources_created"])

    def test_cluster_cleanup_is_idempotent(self):
        result, status, calls, _, _ = self.run_runner(create_rc=0, extra="""
cleanup_cluster
cleanup_cluster
exit 19
""")
        self.assertEqual(result.returncode, 19)
        self.assertEqual(sum(c[:3] == ["kind", "delete", "cluster"] for c in calls), 1)
        self.assertEqual(status["resource_cleanup_status"], "PASS")

    def test_changed_cluster_ownership_blocks_deletion(self):
        result, status, calls, _, state = self.run_runner(create_rc=0, extra="""
CLUSTER_OWNED_NODES=unknown
exit 29
""")
        self.assertEqual(result.returncode, 29)
        self.assertTrue(state["nodes"])
        self.assertFalse(state["deleted"])
        self.assertEqual(status["resource_cleanup_status"], "FAIL")

    def test_cleanup_error_does_not_replace_original_failure(self):
        result, status, _, _, _ = self.run_runner(create_rc=0, extra="""
kind() { echo 'mock deletion failure' >&2; return 41; }
exit 27
""")
        self.assertEqual(result.returncode, 27)
        self.assertIn("mock deletion failure", result.stderr)
        self.assertEqual(status["original_exit_code"], 27)
        self.assertEqual(status["resource_cleanup_status"], "FAIL")

    def test_missing_image_preflight_still_writes_status(self):
        result, status, calls, _, _ = self.run_runner(before="""
docker() { echo 'Error response from daemon: No such image: pinned' >&2; return 1; }
""", extra="")
        self.assertEqual(result.returncode, 1)
        self.assertIn("required pinned agent image unavailable", result.stderr)
        self.assertEqual(status["packet_verification_status"], "NOT_RUN")
        self.assertEqual(status["overall_status"], "BLOCKED")
        self.assertEqual(calls, [])
        self.assertFalse(any(c[:3] == ["docker", "image", "rm"] for c in calls))

    def test_empty_pinned_cni_extraction_preserves_failure_and_cleans_owned_cluster(self):
        result, status, calls, evidence, state = self.run_runner(
            create_rc=0,
            extra="""
ag() { return 0; }
stage_pinned_cni_binary "${EVIDENCE_DIR}/build/evpn-cni" \\
  /usr/local/bin/evpn-cni 15242639 \\
  39a499c490738a3c1d26a82623e7964bc790f30952eab3f6491ab6e8c9afad98 || exit $?
echo POD_CREATION_MUST_NOT_RUN >&2
""")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("POD_CREATION_MUST_NOT_RUN", result.stderr)
        self.assertIn("artifact is empty", result.stderr)
        self.assertTrue(state["deleted"])
        self.assertEqual(status["original_exit_code"], result.returncode)
        self.assertEqual(status["resource_cleanup_status"], "PASS")
        self.assertFalse(any(c[:2] == ["docker", "cp"] for c in calls))
        self.assertFalse(any("evpn-cni" in name for name in evidence))

    def test_worker_copy_failure_preserves_status_and_cleans_owned_cluster(self):
        result, status, calls, _, state = self.run_runner(
            create_rc=0,
            extra="""
docker() {
  case "$*" in
    'exec -i worker sh -s -- prepare '*) cat >/dev/null; echo /opt/cni/bin/.evpn-cni-stage.ABCD1234;;
    'cp '*) echo 'mock worker copy failed' >&2; return 37;;
    'exec -i worker sh -s -- cleanup '*) cat >/dev/null;;
    *) command docker "$@";;
  esac
}
stage_worker_cni_file worker /unused/host /opt/cni/bin/evpn-cni.real 15242639 \\
  39a499c490738a3c1d26a82623e7964bc790f30952eab3f6491ab6e8c9afad98 real
echo POD_CREATION_MUST_NOT_RUN >&2
""")
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertNotIn("POD_CREATION_MUST_NOT_RUN", result.stderr)
        self.assertTrue(state["deleted"])
        self.assertEqual(status["original_exit_code"], 37)
        self.assertEqual(status["resource_cleanup_status"], "PASS")
        self.assertFalse(any(c[:3] == ["docker", "image", "rm"] for c in calls))


if __name__ == "__main__":
    unittest.main()
