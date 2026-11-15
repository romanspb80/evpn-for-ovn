"""Verify the Operator-managed Type-2 fixture's live ownership chain."""

import hashlib
import json
from pathlib import Path
import sys
import uuid


OWNER = "evpn.romanspb80.github.io/installation-uid"
LAST_APPLIED = "evpn.romanspb80.github.io/last-applied"


def inventory_condition_comparisons(core, publication):
    generation = core.get("metadata", {}).get("generation")
    revision = core.get("spec", {}).get("workerInventory", {}).get("revision")
    data = (publication or {}).get("data", {})
    conditions = core.get("status", {}).get("conditions", [])
    comparisons = []
    for name, expected in (("InventoryPublished", "True"), ("InventoryRolloutReady", "True"),
                           ("InventoryAccepted", "Unknown"), ("RuntimeReady", "Unknown"),
                           ("MigrationRequired", "False")):
        matches = [c for c in conditions if c.get("type") == name]
        observed = matches[0] if len(matches) == 1 else {}
        classification = ("missing" if not matches else "duplicate" if len(matches) != 1 else
                          "stale" if observed.get("observedGeneration") != generation else
                          "mismatched" if observed.get("status") != expected else "match")
        comparisons.append({
            "type": name, "classification": classification,
            "expectedStatus": expected, "observedStatus": observed.get("status"),
            "expectedGeneration": generation, "observedGeneration": observed.get("observedGeneration"),
            "expectedRevision": revision, "publishedRevision": data.get("revision"),
            "publishedDigest": data.get("sha256"),
            "reason": observed.get("reason"), "message": observed.get("message"),
        })
    return comparisons


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify_inventory_evidence(core, publication, daemonset, nodes, journals):
    """Compare publication and committed local fences; not runtime or packet proof."""
    spec, meta = core["spec"], core["metadata"]
    inventory = spec["workerInventory"]
    uid, cluster = meta["uid"], inventory["clusterUID"]
    namespace = spec.get("appNamespace") or "evpn-system"
    cm_meta = publication["metadata"]
    labels, annotations = cm_meta.get("labels", {}), cm_meta.get("annotations", {})
    require(publication.get("kind") == "ConfigMap" and publication.get("apiVersion") == "v1" and
            cm_meta.get("name") == "evpn-worker-inventory" and cm_meta.get("namespace") == namespace and
            bool(cm_meta.get("uid")) and not cm_meta.get("deletionTimestamp") and
            labels.get("app.kubernetes.io/managed-by") == "evpn-core-operator" and
            labels.get(OWNER) == uid and annotations.get(OWNER) == uid,
            "inventory publication identity or ownership mismatch")
    applied = json.loads(annotations[LAST_APPLIED])
    require(applied.get("apiVersion") == "v1" and applied.get("kind") == "ConfigMap" and
            applied.get("metadata", {}).get("name") == cm_meta["name"] and
            applied.get("metadata", {}).get("namespace") == namespace and
            applied.get("metadata", {}).get("annotations", {}).get(OWNER) == uid and
            applied.get("data") == publication["data"], "inventory apply journal mismatch")
    data = publication["data"]
    text = data["inventory.json"]
    digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
    published = json.loads(text)
    require(published.get("version") == 1 and published.get("clusterUID") == cluster and
            published.get("installationUID") == uid and
            published.get("revision") == inventory["revision"] and
            data.get("revision") == str(inventory["revision"]) and
            data.get("installationUID") == uid and data.get("sha256") == digest,
            "inventory publication digest or scope mismatch")
    workers = published["workers"]
    def normalized(items):
        return sorted(({**worker, "networks": sorted(worker["networks"], key=lambda n: n["id"])}
                       for worker in items), key=lambda w: w["nodeName"])

    require(len(workers) == len(inventory["workers"]) == len(nodes) == len(journals) and
            normalized(workers) == normalized(inventory["workers"]),
            "inventory worker assignments differ")
    node_uids = {node["metadata"]["name"]: node["metadata"]["uid"] for node in nodes}
    require(len(node_uids) == len(nodes) and
            all(node_uids.get(w["nodeName"]) == w["nodeUID"] for w in workers),
            "published Node identities differ")
    containers = daemonset["spec"]["template"]["spec"]["containers"]
    require(len(containers) == 1, "inventory requires exactly one agent container")
    args = containers[0]["args"]
    expected = {"--worker-inventory-namespace=" + namespace,
                "--worker-inventory-uid=" + cm_meta["uid"],
                "--worker-inventory-cluster-uid=" + cluster,
                "--worker-inventory-installation-uid=" + uid,
                "--worker-inventory-journal=/var/lib/evpn-node-agent/inventory"}
    actual = [arg for arg in args if arg.startswith("--worker-inventory-")]
    require(len(actual) == len(expected) and set(actual) == expected, "inventory arguments differ")
    marker = '"workers":['
    require(text.count(marker) == 1, "inventory worker serialization mismatch")
    offset = text.index(marker) + len(marker)
    worker_hashes = {}
    for worker in workers:
        decoded, end = json.JSONDecoder().raw_decode(text, offset)
        require(decoded == worker, "inventory worker serialization mismatch")
        worker_hashes[worker["nodeName"]] = hashlib.sha256(text[offset:end].encode("utf-8")).hexdigest()
        offset = end + 1
    for worker in workers:
        name = worker["nodeName"]
        raw = journals[name]
        prefix = '{"Record":'
        require(raw.startswith(prefix), "inventory journal envelope malformed")
        record, end = json.JSONDecoder().raw_decode(raw, len(prefix))
        envelope = json.loads(raw)
        require(hashlib.sha256(raw[len(prefix):end].encode("utf-8")).hexdigest() == envelope.get("SHA256") and
                record.get("Pending") is None, "inventory journal checksum or pending intent")
        domain = {"InstallationUID": uid, "NodeUID": worker["nodeUID"],
                  "BootID": str(uuid.uuid5(uuid.NAMESPACE_OID, "worker-inventory-fence-v1")),
                  "NetworkNamespace": "worker-inventory:" + cluster,
                  "ConfigDigest": hashlib.sha256(("worker-inventory-v1:" + cluster).encode()).hexdigest()}
        require(record.get("Version") == 1 and record.get("Domain") == domain and
                isinstance(record.get("Sequence"), int) and record["Sequence"] > 0,
                "inventory journal domain or sequence mismatch")
        state = record["State"]
        require(state.get("version") == 1 and state.get("clusterUID") == cluster and
                state.get("installationUID") == uid and state.get("nodeName") == name and
                state.get("nodeUID") == worker["nodeUID"] and state.get("revision") == inventory["revision"] and
                state.get("digest") == digest and state.get("workerDigest") == worker_hashes[name],
                "inventory fence does not match publication")


def verify(objects, lease, agent_pod, worker):
    by_kind = {item["kind"]: item for item in objects["items"]}
    require(len(objects["items"]) == 3 and
            set(by_kind) == {"EvpnCore", "Deployment", "DaemonSet"},
            "expected one EvpnCore, OVN Deployment and agent DaemonSet")
    core, deployment, daemonset = (by_kind[kind] for kind in
                                   ("EvpnCore", "Deployment", "DaemonSet"))
    uid = core["metadata"]["uid"]
    spec, status = core["spec"], core["status"]
    require(bool(uid) and spec["dataplaneNode"] == worker, "installation identity or placement mismatch")
    require(status.get("ready") is True and status.get("phase") == "Ready" and
            status.get("observedGeneration") == core["metadata"]["generation"],
            "EvpnCore is not ready for its current generation")
    require(lease["spec"]["holderIdentity"] == uid, "installation Lease holder mismatch")
    require(deployment["metadata"]["name"] == "ovn-ovs" and
            daemonset["metadata"]["name"] == "evpn-node-agent", "unexpected workload names")
    for obj in (deployment, daemonset):
        meta = obj["metadata"]
        require(bool(meta.get("uid")) and meta.get("labels", {}).get(OWNER) == uid and
                meta.get("annotations", {}).get(OWNER) == uid, "workload ownership mismatch")
        refs = [ref for ref in status.get("managedResources", [])
                if (ref.get("apiVersion"), ref.get("kind"), ref.get("namespace"), ref.get("name")) ==
                (obj["apiVersion"], obj["kind"], meta["namespace"], meta["name"])]
        require(len(refs) == 1 and refs[0].get("uid") == meta["uid"] and
                refs[0].get("state") == "Observed" and not refs[0].get("conflict"),
                "workload inventory receipt mismatch")
        pod_spec = obj["spec"]["template"]["spec"]
        terms = pod_spec["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        require(terms == [{"matchFields": [{"key": "metadata.name", "operator": "In",
                                            "values": [worker]}]}], "workload placement mismatch")
    ds_status = daemonset["status"]
    require(ds_status.get("observedGeneration", 0) >= daemonset["metadata"]["generation"] and
            all(ds_status.get(field) == 1 for field in
                ("desiredNumberScheduled", "updatedNumberScheduled", "numberReady", "numberAvailable")) and
            ds_status.get("numberUnavailable", 0) == 0 and ds_status.get("numberMisscheduled", 0) == 0,
            "agent DaemonSet rollout is incomplete")
    pod_spec = daemonset["spec"]["template"]["spec"]
    require(pod_spec.get("hostPID") is True, "agent host PID access is missing")
    containers = [c for c in pod_spec["containers"] if c["name"] == "agent"]
    require(len(containers) == 1 and containers[0]["image"] == spec["nodeAgent"]["image"],
            "agent image does not match EvpnCore")
    args = containers[0]["args"]
    for field, flag in (("exportConfigPath", "--evpn-export-config="),
                        ("remoteConfigPath", "--evpn-remote-config="),
                        ("cniContinuityConfigPath", "--cni-continuity-config=")):
        require(bool(spec["nodeAgent"].get(field)) and
                args.count(flag + spec["nodeAgent"][field]) == 1, f"agent {field} mismatch")
    volumes = [v for v in pod_spec["volumes"] if v["name"] == "gobgp"]
    require(len(volumes) == 1 and volumes[0]["hostPath"]["path"] == spec["nodeAgent"]["gobgpHostPath"],
            "agent GoBGP socket host path mismatch")
    owners = agent_pod["metadata"].get("ownerReferences", [])
    require(any(o.get("kind") == "DaemonSet" and o.get("uid") == daemonset["metadata"]["uid"] and
                o.get("controller") is True for o in owners), "agent Pod is not owned by the DaemonSet")
    require(agent_pod["spec"]["nodeName"] == worker and any(
        c.get("type") == "Ready" and c.get("status") == "True"
        for c in agent_pod["status"].get("conditions", [])), "agent Pod is not ready on the designated node")


def verify_external_ovn(core, lease, daemonset, pods, nodes, expected_selector, *,
                        require_continuity=True, inventory_publication=None, inventory_journals=None):
    """Verify the Kubernetes ownership chain, not node-local recovery or forwarding."""
    require(len(nodes) >= 2 and bool(expected_selector), "two nodes and a nonempty selector are required")
    node_names = [node.get("metadata", {}).get("name") for node in nodes]
    node_uids = [node.get("metadata", {}).get("uid") for node in nodes]
    require(all(node_names) and len(set(node_names)) == len(nodes) and
            all(node_uids) and len(set(node_uids)) == len(nodes),
            "node names and UIDs must be distinct and nonempty")
    for node in nodes:
        labels = node["metadata"].get("labels", {})
        require(all(labels.get(key) == value for key, value in expected_selector.items()),
                "expected node does not match selector")
    meta = core["metadata"]
    uid = meta.get("uid")
    spec, status = core["spec"], core.get("status", {})
    require(bool(uid) and spec.get("enabled") is True and
            spec.get("dataplaneMode") == "external-ovn" and not spec.get("dataplaneNode") and
            status.get("dataplaneMode") == "external-ovn", "external-ovn installation identity or mode mismatch")
    require(isinstance(meta.get("generation"), int) and meta["generation"] > 0 and
            status.get("observedGeneration") == meta.get("generation"),
            "EvpnCore has not observed its current generation")
    inventory_mode = spec.get("workerInventory") is not None
    if inventory_mode:
        require(inventory_publication is not None and inventory_journals is not None,
                "inventory publication and local journals are required")
        for comparison in inventory_condition_comparisons(core, inventory_publication):
            require(comparison["classification"] == "match",
                    "inventory conditions: " + json.dumps(comparison, sort_keys=True))
        require(status.get("ready", False) is False and status.get("phase") == "Reconciling",
                "inventory status must not claim runtime acceptance")
        verify_inventory_evidence(core, inventory_publication, daemonset, nodes, inventory_journals)
    else:
        require(inventory_publication is None and inventory_journals is None and
                status.get("ready") is True and status.get("phase") == "Ready",
                "EvpnCore is not ready for its current generation")
    require(lease.get("spec", {}).get("holderIdentity") == uid, "installation Lease holder mismatch")
    dmeta = daemonset["metadata"]
    ds_uid = dmeta.get("uid")
    require(daemonset.get("apiVersion") == "apps/v1" and daemonset.get("kind") == "DaemonSet" and
            dmeta.get("name") == "evpn-node-agent" and
            dmeta.get("namespace") == spec.get("appNamespace", "evpn-system") and
            bool(ds_uid) and dmeta.get("annotations", {}).get(OWNER) == uid and
            dmeta.get("labels", {}).get(OWNER) == uid, "DaemonSet ownership mismatch")
    refs = status.get("managedResources", [])
    expected_refs = {("apps/v1", "DaemonSet", dmeta["namespace"], dmeta["name"], ds_uid, "Observed")}
    if inventory_mode:
        cm = inventory_publication["metadata"]
        expected_refs.add(("v1", "ConfigMap", cm["namespace"], cm["name"], cm["uid"], "Observed"))
    actual_refs = [(r.get("apiVersion"), r.get("kind"), r.get("namespace"), r.get("name"),
                    r.get("uid"), r.get("state")) for r in refs]
    require(len(actual_refs) == len(expected_refs) and set(actual_refs) == expected_refs and
            not any(r.get("conflict") for r in refs), "DaemonSet inventory receipt mismatch")
    agent = spec.get("nodeAgent", {})
    template = daemonset["spec"]["template"]["spec"]
    require(agent.get("enabled") is True and agent.get("nodeSelector") == expected_selector and
            template.get("nodeSelector") == expected_selector,
            "DaemonSet selector mismatch")
    require(template.get("hostNetwork") is True and template.get("hostPID") is True,
            "node-local host namespaces missing")
    containers = [c for c in template.get("containers", []) if c.get("name") == "agent"]
    require(len(containers) == 1 and containers[0].get("image") == agent.get("image") and
            bool(agent.get("image")), "agent image mismatch")
    args = containers[0].get("args", [])
    continuity = agent.get("cniContinuityConfigPath")
    if require_continuity:
        require(bool(continuity) and args.count("--cni-continuity-config=" + continuity) == 1 and
                agent.get("providerAttestation") is True,
                "trusted CNI continuity configuration missing")
    require(args.count("--ovsdb-endpoint=unix:/var/run/openvswitch/db.sock") == 1,
            "agent OVSDB endpoint is not node-local")
    ds_status = daemonset.get("status", {})
    count = len(nodes)
    require(isinstance(dmeta.get("generation"), int) and dmeta["generation"] > 0 and
            ds_status.get("observedGeneration", 0) >= dmeta["generation"] and
            all(ds_status.get(field) == count for field in
                ("desiredNumberScheduled", "updatedNumberScheduled", "numberReady", "numberAvailable")) and
            ds_status.get("numberUnavailable", 0) == 0 and ds_status.get("numberMisscheduled", 0) == 0,
            "agent DaemonSet rollout is incomplete")
    require(len(pods) == count, "unexpected agent Pod count")
    seen_nodes, seen_uids = set(), set()
    for pod in pods:
        pmeta = pod.get("metadata", {})
        pod_uid = pmeta.get("uid")
        node = pod.get("spec", {}).get("nodeName")
        owners = pmeta.get("ownerReferences", [])
        require(bool(pod_uid) and pod_uid not in seen_uids and
                pmeta.get("namespace") == dmeta["namespace"] and
                not pmeta.get("deletionTimestamp") and
                any(owner.get("apiVersion") == "apps/v1" and owner.get("kind") == "DaemonSet" and
                    owner.get("name") == dmeta["name"] and owner.get("uid") == ds_uid and
                    owner.get("controller") is True for owner in owners),
                "agent Pod ownership mismatch")
        require(node in node_names and node not in seen_nodes and any(
            condition.get("type") == "Ready" and condition.get("status") == "True"
            for condition in pod.get("status", {}).get("conditions", [])),
            "agent Pod placement or readiness mismatch")
        seen_nodes.add(node)
        seen_uids.add(pod_uid)
    require(seen_nodes == set(node_names), "not every expected node has an agent Pod")


def verify_cleanup(owned, lease, provider_before, provider_after, installation_uid):
    require(not owned["items"], "installation-owned Kubernetes resources remain")
    require(lease["spec"].get("holderIdentity", "") == "", "installation Lease was not released")
    before, after = provider_before["metadata"], provider_after["metadata"]
    require(bool(installation_uid) and before["uid"] == after["uid"] and
            before["generation"] == after["generation"] and
            provider_before["spec"] == provider_after["spec"], "independent provider configuration changed")


if __name__ == "__main__":
    directory = Path(sys.argv[1])
    if sys.argv[2] == "--external-inventory":
        core, lease, ds, pods, nodes, selector, cm, journals = (
            json.loads((directory / name).read_text()) for name in
            ("core.json", "lease.json", "daemonset.json", "pods.json", "nodes.json",
             "selector.json", "inventory.json", "inventory-journals.json"))
        verify_external_ovn(core, lease, ds, pods["items"], nodes["items"], selector,
                            inventory_publication=cm, inventory_journals=journals)
        print("inventory publication, rollout and local committed fences=PASS; runtime and forwarding not established")
    elif sys.argv[2] == "--cleanup":
        verify_cleanup(*(json.loads((directory / name).read_text()) for name in
                         ("owned-after-delete.json", "lease-after-delete.json",
                          "provider-config-before.json", "provider-config-after-delete.json")),
                       sys.argv[3])
        print("owned resource deletion, Lease release and provider configuration preservation=PASS")
    else:
        verify(*(json.loads((directory / name).read_text()) for name in
                 ("operator-ownership.json", "installation-lease.json", "agent-ownership-pod.json")),
               sys.argv[2])
        print("current EvpnCore, inventory, Lease, workload configuration and agent Pod ownership=PASS")
