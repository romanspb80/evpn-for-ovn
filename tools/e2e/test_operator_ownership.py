"""Offline tests for live Operator ownership evidence validation."""

import copy
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
import uuid
from pathlib import Path

import operator_ownership as ownership


def fixture():
    uid, worker = "installation-uid", "worker"
    placement = {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
        "nodeSelectorTerms": [{"matchFields": [{"key": "metadata.name", "operator": "In",
                                                "values": [worker]}]}]}}}
    agent_config = {"image": "agent:image", "exportConfigPath": "/run/export.json",
                    "remoteConfigPath": "/run/remote.json",
                    "cniContinuityConfigPath": "/run/cni.json", "gobgpHostPath": "/run/gobgp"}
    core = {"kind": "EvpnCore", "metadata": {"uid": uid, "generation": 2},
            "spec": {"dataplaneNode": worker, "nodeAgent": agent_config},
            "status": {"ready": True, "phase": "Ready", "observedGeneration": 2,
                       "managedResources": []}}
    workloads = []
    for kind, name in (("Deployment", "ovn-ovs"), ("DaemonSet", "evpn-node-agent")):
        obj = {"apiVersion": "apps/v1", "kind": kind,
               "metadata": {"name": name, "namespace": "cni-test", "uid": name + "-uid",
                            "generation": 1, "labels": {ownership.OWNER: uid},
                            "annotations": {ownership.OWNER: uid}},
               "spec": {"template": {"spec": {"affinity": copy.deepcopy(placement)}}}}
        core["status"]["managedResources"].append({
            "apiVersion": "apps/v1", "kind": kind, "namespace": "cni-test", "name": name,
            "uid": name + "-uid", "state": "Observed"})
        workloads.append(obj)
    ds = workloads[1]
    ds["status"] = {"observedGeneration": 1, "desiredNumberScheduled": 1,
                    "updatedNumberScheduled": 1, "numberReady": 1, "numberAvailable": 1}
    ds["spec"]["template"]["spec"].update({
        "hostPID": True,
        "containers": [{"name": "agent", "image": "agent:image", "args": [
            "--evpn-export-config=/run/export.json", "--evpn-remote-config=/run/remote.json",
            "--cni-continuity-config=/run/cni.json"]}],
        "volumes": [{"name": "gobgp", "hostPath": {"path": "/run/gobgp"}}]})
    pod = {"metadata": {"ownerReferences": [{"kind": "DaemonSet", "uid": "evpn-node-agent-uid",
                                             "controller": True}]},
           "spec": {"nodeName": worker},
           "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    return {"items": [core, *workloads]}, {"spec": {"holderIdentity": uid}}, pod, worker


def external_fixture():
    uid, ds_uid = "installation-uid", "agent-uid"
    selector = {"evpn.e2e/owner": "run"}
    agent = {"enabled": True, "image": "agent:image", "nodeSelector": selector,
             "providerAttestation": True, "cniContinuityConfigPath": "/run/evpn-node-agent/cni.json"}
    core = {"metadata": {"uid": uid, "generation": 2, "namespace": "installation"},
            "spec": {"enabled": True, "dataplaneMode": "external-ovn", "appNamespace": "cni-test",
                     "nodeAgent": copy.deepcopy(agent)},
            "status": {"dataplaneMode": "external-ovn", "ready": True, "phase": "Ready",
                       "observedGeneration": 2, "managedResources": [{
                           "apiVersion": "apps/v1", "kind": "DaemonSet", "namespace": "cni-test",
                           "name": "evpn-node-agent", "uid": ds_uid, "state": "Observed"}]}}
    ds = {"apiVersion": "apps/v1", "kind": "DaemonSet",
          "metadata": {"name": "evpn-node-agent", "namespace": "cni-test", "uid": ds_uid,
                       "generation": 3, "labels": {ownership.OWNER: uid},
                       "annotations": {ownership.OWNER: uid}},
          "spec": {"template": {"spec": {
              "hostNetwork": True, "hostPID": True, "nodeSelector": copy.deepcopy(selector),
              "containers": [{"name": "agent", "image": agent["image"],
                              "args": ["--ovsdb-endpoint=unix:/var/run/openvswitch/db.sock",
                                       "--cni-continuity-config=" + agent["cniContinuityConfigPath"]]}]}}},
          "status": {"observedGeneration": 3, "desiredNumberScheduled": 2,
                     "updatedNumberScheduled": 2, "numberReady": 2, "numberAvailable": 2}}
    nodes, pods = [], []
    for name in ("worker-a", "worker-b"):
        nodes.append({"metadata": {"name": name, "uid": name + "-uid", "labels": copy.deepcopy(selector)}})
        pods.append({"metadata": {"uid": name + "-pod-uid", "namespace": "cni-test",
                                  "ownerReferences": [{"apiVersion": "apps/v1", "kind": "DaemonSet",
                                                       "name": "evpn-node-agent", "uid": ds_uid,
                                                       "controller": True}]},
                     "spec": {"nodeName": name},
                     "status": {"conditions": [{"type": "Ready", "status": "True"}]}})
    return core, {"spec": {"holderIdentity": uid}}, ds, pods, nodes, selector


def inventory_fixture():
    core, lease, ds, pods, nodes, selector = external_fixture()
    uid, cluster, cm_uid = core["metadata"]["uid"], "cluster-uid", "publication-uid"
    workers = [{"nodeName": n["metadata"]["name"], "nodeUID": n["metadata"]["uid"],
                "chassis": "chassis-" + n["metadata"]["name"],
                "networks": [{"id": "tenant-a", "logicalSwitch": "tenant-a", "vni": 1000}]}
               for n in nodes]
    core["spec"]["workerInventory"] = {"clusterUID": cluster, "revision": 1, "workers": workers}
    published = {"version": 1, "revision": 1, "clusterUID": cluster,
                 "installationUID": uid, "workers": workers}
    compact = lambda obj: json.dumps(obj, separators=(",", ":"), ensure_ascii=False)
    text = compact(published)
    digest = hashlib.sha256(text.encode()).hexdigest()
    data = {"inventory.json": text, "sha256": digest, "revision": "1", "installationUID": uid}
    applied = {"apiVersion": "v1", "kind": "ConfigMap",
               "metadata": {"name": "evpn-worker-inventory", "namespace": "cni-test",
                            "annotations": {ownership.OWNER: uid}}, "data": data}
    cm = {"apiVersion": "v1", "kind": "ConfigMap",
          "metadata": {"name": "evpn-worker-inventory", "namespace": "cni-test", "uid": cm_uid,
                       "labels": {"app.kubernetes.io/managed-by": "evpn-core-operator", ownership.OWNER: uid},
                       "annotations": {ownership.OWNER: uid, ownership.LAST_APPLIED: compact(applied)}},
          "data": data}
    status = core["status"]
    status.update(ready=False, phase="Reconciling",
                  conditions=[{"type": name, "status": value, "observedGeneration": 2}
                              for name, value in (("InventoryPublished", "True"), ("InventoryRolloutReady", "True"),
                                                  ("InventoryAccepted", "Unknown"), ("RuntimeReady", "Unknown"),
                                                  ("MigrationRequired", "False"))])
    status["managedResources"].append({"apiVersion": "v1", "kind": "ConfigMap", "namespace": "cni-test",
                                       "name": "evpn-worker-inventory", "uid": cm_uid, "state": "Observed"})
    ds["spec"]["template"]["spec"]["containers"][0]["args"].extend([
        "--worker-inventory-namespace=cni-test", "--worker-inventory-uid=" + cm_uid,
        "--worker-inventory-cluster-uid=" + cluster, "--worker-inventory-installation-uid=" + uid,
        "--worker-inventory-journal=/var/lib/evpn-node-agent/inventory"])
    journals = {}
    for worker in workers:
        state = {"version": 1, "clusterUID": cluster, "installationUID": uid, "nodeName": worker["nodeName"],
                 "nodeUID": worker["nodeUID"], "revision": 1, "digest": digest,
                 "workerDigest": hashlib.sha256(compact(worker).encode()).hexdigest()}
        domain = {"InstallationUID": uid, "NodeUID": worker["nodeUID"],
                  "BootID": str(uuid.uuid5(uuid.NAMESPACE_OID, "worker-inventory-fence-v1")),
                  "NetworkNamespace": "worker-inventory:" + cluster,
                  "ConfigDigest": hashlib.sha256(("worker-inventory-v1:" + cluster).encode()).hexdigest()}
        record = compact({"Version": 1, "Domain": domain, "Sequence": 1, "State": state, "Pending": None})
        journals[worker["nodeName"]] = '{"Record":' + record + ',"SHA256":"' + hashlib.sha256(record.encode()).hexdigest() + '"}'
    return core, lease, ds, pods, nodes, selector, cm, journals


def alter_record(journals, update):
    envelope = json.loads(journals["worker-a"])
    update(envelope["Record"])
    record = json.dumps(envelope["Record"], separators=(",", ":"), ensure_ascii=False)
    journals["worker-a"] = ('{"Record":' + record + ',"SHA256":"' +
                            hashlib.sha256(record.encode()).hexdigest() + '"}')


class OperatorOwnershipTests(unittest.TestCase):
    def test_valid_chain(self):
        ownership.verify(*fixture())

    def test_rejects_stale_or_foreign_evidence(self):
        def cases():
            yield "current generation", lambda o, l, p: o["items"][0]["status"].update(observedGeneration=1)
            yield "Lease", lambda o, l, p: l["spec"].update(holderIdentity="foreign")
            yield "ownership", lambda o, l, p: o["items"][2]["metadata"]["labels"].update({ownership.OWNER: "foreign"})
            yield "receipt", lambda o, l, p: o["items"][0]["status"]["managedResources"][1].update(uid="old")
            yield "rollout", lambda o, l, p: o["items"][2]["status"].update(numberReady=0)
            yield "image", lambda o, l, p: o["items"][2]["spec"]["template"]["spec"]["containers"][0].update(image="old")
            yield "remoteConfigPath", lambda o, l, p: o["items"][0]["spec"]["nodeAgent"].update(remoteConfigPath="/run/new")
            yield "host path", lambda o, l, p: o["items"][0]["spec"]["nodeAgent"].update(gobgpHostPath="/run/other")
            yield "not owned", lambda o, l, p: p["metadata"]["ownerReferences"][0].update(uid="old")
            yield "designated node", lambda o, l, p: p["spec"].update(nodeName="other")
        for expected, mutate in cases():
            with self.subTest(expected=expected):
                objects, lease, pod, worker = fixture()
                mutate(objects, lease, pod)
                with self.assertRaisesRegex(ValueError, expected):
                    ownership.verify(objects, lease, pod, worker)

    def test_cleanup_rejects_owned_resources_held_lease_or_provider_changes(self):
        owned = {"items": []}
        lease = {"spec": {"holderIdentity": ""}}
        provider = {"metadata": {"uid": "provider-uid", "generation": 1}, "spec": {"asn": 64512}}
        ownership.verify_cleanup(owned, lease, provider, copy.deepcopy(provider), "installation-uid")
        for expected, mutate in (
                ("resources remain", lambda o, l, p: o["items"].append({"kind": "DaemonSet"})),
                ("Lease", lambda o, l, p: l["spec"].update(holderIdentity="installation-uid")),
                ("provider", lambda o, l, p: p["spec"].update(asn=64513))):
            with self.subTest(expected=expected):
                current_owned, current_lease, after = copy.deepcopy((owned, lease, provider))
                mutate(current_owned, current_lease, after)
                with self.assertRaisesRegex(ValueError, expected):
                    ownership.verify_cleanup(current_owned, current_lease, provider, after, "installation-uid")

    def test_external_ovn_ownership_chain(self):
        ownership.verify_external_ovn(*external_fixture())

    def test_inventory_external_ovn_evidence(self):
        *args, cm, journals = inventory_fixture()
        args[0]["status"].pop("ready")
        ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals)

    def test_inventory_condition_reports_exact_mismatch(self):
        for change, classification in (({"status": "False"}, "mismatched"),
                                       ({"observedGeneration": 1}, "stale")):
            with self.subTest(classification=classification):
                *args, cm, journals = inventory_fixture()
                condition = args[0]["status"]["conditions"][1]
                condition.update(change, reason="NotReady", message="waiting for workers")
                with self.assertRaises(ValueError) as caught:
                    ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals)
                detail = json.loads(str(caught.exception).split(": ", 1)[1])
                self.assertEqual(detail["type"], "InventoryRolloutReady")
                self.assertEqual(detail["classification"], classification)
                self.assertEqual(detail["expectedStatus"], "True")
                self.assertEqual(detail["reason"], "NotReady")
                self.assertEqual(detail["message"], "waiting for workers")
                self.assertEqual(detail["expectedRevision"], 1)
                self.assertEqual(detail["publishedDigest"], cm["data"]["sha256"])

    def test_new_publication_acceptance_does_not_authorize_security_recovery(self):
        *args, cm, journals = inventory_fixture()
        args[2]["spec"]["template"]["spec"]["containers"][0]["args"].append(
            "--worker-inventory-recovery-file=/var/lib/evpn-node-agent/inventory/recovery-request.json")
        for node, raw in list(journals.items()):
            record = json.loads(raw)["Record"]
            record["State"].update(version=2, trust={
                "Publication": "cni-test/evpn-worker-inventory/publication-uid",
                "Phase": "FENCED", "Security": True, "Binding": None})
            encoded = json.dumps(record, separators=(",", ":"), ensure_ascii=False)
            journals[node] = '{"Record":' + encoded + ',"SHA256":"' + hashlib.sha256(encoded.encode()).hexdigest() + '"}'
        with self.assertRaisesRegex(ValueError, "authorization is not current"):
            ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals)
        ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals,
                                      require_fenced=True)
        alter_record(journals, lambda r: r["State"]["trust"].update(Security=False))
        with self.assertRaisesRegex(ValueError, "retain the security fence"):
            ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals,
                                          require_fenced=True)

    def test_inventory_evidence_cli(self):
        core, lease, ds, pods, nodes, selector, cm, journals = inventory_fixture()
        objects = (core, lease, ds, {"items": pods}, {"items": nodes}, selector, cm, journals)
        names = ("core.json", "lease.json", "daemonset.json", "pods.json", "nodes.json",
                 "selector.json", "inventory.json", "inventory-journals.json")
        with tempfile.TemporaryDirectory() as directory:
            for name, obj in zip(names, objects):
                (Path(directory) / name).write_text(json.dumps(obj))
            result = subprocess.run([sys.executable, ownership.__file__, directory, "--external-inventory"],
                                    text=True, capture_output=True, check=True)
            self.assertIn("runtime and forwarding not established", result.stdout)

    def test_inventory_external_ovn_rejects_bad_evidence(self):
        cases = [
            ("generation", lambda c, l, d, p, n, s, m, j: c["status"].update(observedGeneration=1)),
            ("conditions", lambda c, l, d, p, n, s, m, j: c["status"]["conditions"][0].update(observedGeneration=1)),
            ("arguments", lambda c, l, d, p, n, s, m, j: m["metadata"].update(uid="substituted")),
            ("apply journal", lambda c, l, d, p, n, s, m, j: m["data"].update(sha256="wrong")),
            ("Node identities", lambda c, l, d, p, n, s, m, j: n[0]["metadata"].update(uid="different")),
            ("checksum", lambda c, l, d, p, n, s, m, j: j.update({"worker-a": j["worker-a"].replace('"Pending":null', '"Pending":{}')})),
            ("pending intent", lambda c, l, d, p, n, s, m, j: alter_record(j, lambda r: r.update(Pending={}))),
            ("domain", lambda c, l, d, p, n, s, m, j: alter_record(j, lambda r: r["Domain"].update(NodeUID="wrong"))),
            ("fence", lambda c, l, d, p, n, s, m, j: alter_record(j, lambda r: r["State"].update(digest="wrong"))),
            ("fence", lambda c, l, d, p, n, s, m, j: alter_record(j, lambda r: r["State"].update(workerDigest="wrong"))),
            ("domain", lambda c, l, d, p, n, s, m, j: j.update({"worker-a": j["worker-b"]})),
            ("receipt", lambda c, l, d, p, n, s, m, j: c["status"]["managedResources"][1].update(uid="old")),
        ]
        for expected, mutate in cases:
            with self.subTest(expected=expected):
                evidence = inventory_fixture()
                mutate(*evidence)
                *args, cm, journals = evidence
                with self.assertRaisesRegex(ValueError, expected):
                    ownership.verify_external_ovn(*args, inventory_publication=cm, inventory_journals=journals)

    def test_external_ovn_rejects_incomplete_or_foreign_evidence(self):
        cases = [
            ("generation", lambda c, l, d, p, n, s: c["status"].update(observedGeneration=1)),
            ("generation", lambda c, l, d, p, n, s: c["metadata"].pop("generation")),
            ("mode", lambda c, l, d, p, n, s: c["spec"].update(dataplaneMode="managed-single-node")),
            ("Lease", lambda c, l, d, p, n, s: l["spec"].update(holderIdentity="foreign")),
            ("ownership", lambda c, l, d, p, n, s: d["metadata"]["labels"].update({ownership.OWNER: "foreign"})),
            ("receipt", lambda c, l, d, p, n, s: c["status"]["managedResources"][0].update(uid="old")),
            ("selector", lambda c, l, d, p, n, s: d["spec"]["template"]["spec"]["nodeSelector"].update(other="true")),
            ("continuity", lambda c, l, d, p, n, s: c["spec"]["nodeAgent"].update(providerAttestation=False)),
            ("node-local", lambda c, l, d, p, n, s: d["spec"]["template"]["spec"]["containers"][0]["args"].__setitem__(0, "--ovsdb-endpoint=tcp:other:6640")),
            ("rollout", lambda c, l, d, p, n, s: d["status"].update(numberReady=1)),
            ("rollout", lambda c, l, d, p, n, s: d["metadata"].pop("generation")),
            ("Pod count", lambda c, l, d, p, n, s: p.pop()),
            ("Pod ownership", lambda c, l, d, p, n, s: p[0]["metadata"]["ownerReferences"][0].update(uid="old")),
            ("placement", lambda c, l, d, p, n, s: p[1]["spec"].update(nodeName="worker-a")),
            ("UIDs", lambda c, l, d, p, n, s: n[1]["metadata"].update(uid=n[0]["metadata"]["uid"])),
            ("selector", lambda c, l, d, p, n, s: n[0]["metadata"]["labels"].clear()),
        ]
        for expected, mutate in cases:
            with self.subTest(expected=expected):
                evidence = external_fixture()
                mutate(*evidence)
                with self.assertRaisesRegex(ValueError, expected):
                    ownership.verify_external_ovn(*evidence)


if __name__ == "__main__":
    unittest.main()
