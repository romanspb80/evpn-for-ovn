"""Live tests of the approved inventory authority/recovery boundary."""

import copy
import hashlib
import json
import time
import uuid

import containerlab as lab
import isolated_secondary_cni as fixture


REQUEST_PATH = "/var/lib/evpn-worker/inventory/recovery-request.json"


def endpoint_outage(test, endpoints, identities, installation, node, survivor):
    import operator_secondary_cni as contract

    old = copy.deepcopy(endpoints[node])
    report = {"status": "IN_PROGRESS", "oldEndpoint": old, "before": test.gap_snapshot(endpoints)}
    artifact = "H053-endpoint-outage-" + node + ".json"
    test.record(artifact, report)
    test.worker(node, "systemctl", "stop", "evpn-e2e-gobgpd.service")
    try:
        report["unavailable"] = test.await_gap_routes(endpoints, [survivor], [node])
        pod = test.get_json("-n", "evpn-system", "get", "pod", old["pod"])
        if (pod["metadata"]["uid"] != old["podUID"] or
                pod["metadata"].get("labels", {}).get("evpn.e2e/owner") != test.owner):
            raise fixture.IsolationError("outage endpoint deletion identity changed")
        test.k("-n", "evpn-system", "delete", "pod", old["pod"], "--wait=true", "--timeout=120s")
        report["deletedCNIJournal"], _ = test.wait_journal(node, old["podUID"], "deleted")
        report["deletedWhileUnavailable"] = test.gap_snapshot(endpoints)
        contract.assert_inventory_survivor(report["before"], report["deletedWhileUnavailable"], survivor)
        test.record(artifact, report)
    finally:
        test.worker(node, "systemctl", "start", "evpn-e2e-gobgpd.service")
    try:
        test.await_external_import(node, identities[node], installation, old["vtep"])
        report["recoveredWithoutEndpoint"] = test.await_gap_routes(
            endpoints, [survivor], [node], withdrawn_receipts=True)
        worker = report["recoveredWithoutEndpoint"]["workers"][node]
        speaker = worker["export"]["SpeakerProcess"]
        paths = json.loads(test.worker(node, "/opt/evpn/bin/type2rib", "-socket", "/run/gobgp/gobgp.sock",
                                       "-router-id", old["vtep"], "-pid", str(speaker["PID"]),
                                       "-start-ticks", speaker["StartTicks"]).stdout)
        if any(path.get("Local") and (path.get("NLRI") or {}).get("MAC") == old["mac"] for path in paths):
            raise fixture.IsolationError("deleted outage endpoint resurrected in actual speaker")
        report["absentLocalRIB"] = paths
        created = test.create_inventory_endpoints(test.inventory_rendered, identities, recreate_node=node)
        replacement = created[node]
        if replacement["podUID"] == old["podUID"] or replacement["lspUUID"] == old["lspUUID"]:
            raise fixture.IsolationError("legitimate recreation reused deleted endpoint identity")
        endpoints[node] = replacement
        path_uuid = test.await_local_export(node, replacement, identities[node], installation)
        test.verify_speaker_rib(node, replacement, identities[node], installation, path_uuid)
        report["newEndpoint"] = replacement
        report["recovered"] = test.await_gap_routes(endpoints, test.workers)
        contract.assert_inventory_survivor(report["before"], report["recovered"], survivor)
        report["status"] = "PASS"
        return report
    finally:
        test.record(artifact, report)


def candidate_rejected(core):
    generation = core["metadata"]["generation"]
    status = core.get("status", {})
    return (status.get("phase") == "Degraded" and status.get("observedGeneration") == generation
            and any(condition.get("type") == "Degraded" and condition.get("status") == "True"
                    and condition.get("reason") in ("ReconcileFailed", "OwnershipConflict")
                    and condition.get("observedGeneration") == generation
                    for condition in status.get("conditions", [])))

def assert_probe_payloads(packets):
    if not packets:
        raise fixture.IsolationError("recovery capture is empty")
    pairs = {}
    for packet in packets:
        identity = packet["icmpIdentifier"], packet["icmpSequence"]
        payload = bytes.fromhex(packet["payload"])
        marker = identity[0].to_bytes(2, "big")
        if len(payload) != 56 or payload[16:] != (marker * 28)[16:]:
            raise fixture.IsolationError("recovery capture payload marker mismatch")
        pairs.setdefault(identity, []).append(payload)
    if any(len(values) != 2 or values[0] != values[1] for values in pairs.values()):
        raise fixture.IsolationError("recovery capture request/reply payloads differ")


def verify_dataplane(test, endpoints, case):
    import operator_secondary_cni as contract

    prefix = case + "-recovered-"
    evidence = {"status": "IN_PROGRESS", "scope": "post-recovery net1/external captures and current OVN/OVS counters",
                "external": {}, "crossWorker": {}}
    captures = []
    def ping(arguments, identifier, external=False):
        command = ["ping", "-n", "-e", str(identifier), "-p", identifier.to_bytes(2, "big").hex(),
                   "-c", "3", "-W", "3", *arguments]
        result = (test.docker("exec", test.frr, "ip", "netns", "exec", "remote-workload", *command,
                              check=False, timeout=30) if external else
                  test.k("-n", "evpn-system", "exec", active["pod"], "-c", "client", "--", *command,
                         check=False, timeout=30))
        observed = {**contract.parse_inventory_ping_result(result.stdout, result.returncode),
                    "command": command, "stdout": result.stdout, "stderr": result.stderr}
        if observed["status"] != "PASS":
            raise fixture.IsolationError("post-recovery workload ping failed: " + json.dumps(observed))
        return observed

    try:
        capture_endpoints = {node: {**endpoint, "icmpIdentifiers": {
            "workerToExternal": 20000 + index, "externalToWorker": 30000 + index}}
            for index, (node, endpoint) in enumerate(endpoints.items())}
        captures = test.start_inventory_workload_captures(capture_endpoints, artifact_prefix=prefix)
        for index, (node, active) in enumerate(endpoints.items()):
            before = test.collect_ovs_dataplane(node, active, test.inventory_identities[node]["chassis"], persist=False)
            forward = ping(["-I", "net1", contract.REMOTE_IP], 20000 + index)
            reverse = ping(["-I", "frr-work0", active["ip"]], 30000 + index, external=True)
            after = test.collect_ovs_dataplane(node, active, test.inventory_identities[node]["chassis"], persist=False)
            if any(before[key] != after[key] for key in ("interface", "ofport", "ownerEvidence")):
                raise fixture.IsolationError("recovered OVS forwarding identity changed")
            delta = contract.inventory_ovs_counter_delta(
                before["tunnelCounters"], after["tunnelCounters"],
                before["flowCounters"][contract.REMOTE_MAC]["packets"], after["flowCounters"][contract.REMOTE_MAC]["packets"])
            evidence["external"][node] = {"forward": forward, "reverse": reverse, "ovsBefore": before,
                                          "ovsAfter": after, "ovsCounterDelta": delta}
        evidence["externalCaptures"] = test.stop_inventory_workload_captures(captures, capture_endpoints)
        captures = []
        for capture in evidence["externalCaptures"]["captures"]:
            assert_probe_payloads(fixture.decode_icmp_pcap(test.directory / capture["pcap"]))
        before = test.cross_worker_snapshot(endpoints)
        captures = test.start_inventory_workload_captures(endpoints, cross_worker=True, artifact_prefix=prefix)
        for index, (node, active) in enumerate(endpoints.items()):
            peer = endpoints[test.workers[1 - index]]
            evidence["crossWorker"][node] = ping(["-I", "net1", peer["ip"]], 41000 + index)
        after = test.cross_worker_snapshot(endpoints)
        evidence["geneveCounterDeltas"] = contract.inventory_geneve_counter_deltas(before, after)
        evidence["ovnBefore"], evidence["ovnAfter"] = before, after
        evidence["crossWorkerCaptures"] = test.stop_inventory_workload_captures(captures, endpoints, cross_worker_ids=True)
        captures = []
        expected = set()
        for index, node in enumerate(test.workers):
            source, destination = endpoints[node]["ip"], endpoints[test.workers[1 - index]]["ip"]
            for kind, src, dst in ((8, source, destination), (0, destination, source)):
                expected.update((src, dst, kind, 41000 + index, sequence) for sequence in (1, 2, 3))
        reference = None
        for capture in evidence["crossWorkerCaptures"]["captures"]:
            packets = fixture.decode_icmp_pcap(test.directory / capture["pcap"])
            actual = {(p["source"], p["destination"], p["icmpType"], p["icmpIdentifier"], p["icmpSequence"])
                      for p in packets}
            if len(packets) != 12 or actual != expected:
                raise fixture.IsolationError("post-recovery cross-worker packet identities differ")
            assert_probe_payloads(packets)
            payloads = {(*identity, p["payload"]) for p in packets
                        for identity in [(p["source"], p["destination"], p["icmpType"],
                                          p["icmpIdentifier"], p["icmpSequence"])]}
            if reference is not None and payloads != reference:
                raise fixture.IsolationError("cross-worker receiver captured different payloads")
            reference = payloads
        evidence["status"] = "PASS"
        return evidence
    finally:
        for capture in captures:
            process = capture["process"]
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except fixture.subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)
            if not capture["stream"].closed:
                capture["stream"].close()


def write_protected(test, node, path, content):
    result = fixture.subprocess.run(
        ["docker", "exec", "-i", node, "sh", "-ec",
         'set -eu; test -d "$(dirname "$1")"; test ! -L "$1"; '
         'test ! -e "$1.new"; umask 077; cat > "$1.new"; '
         'chmod 0600 "$1.new"; mv -T "$1.new" "$1"', "sh", path],
        input=content, text=True, capture_output=True, timeout=30,
        pass_fds=lab.lock_descriptors(),
    )
    if result.returncode:
        raise fixture.IsolationError("protected test input could not be published: " + result.stderr)


def recovery_request(snapshot, node):
    target = snapshot["workers"][node]["export"].get("RecoveryTarget")
    if not target:
        raise fixture.IsolationError("no scoped administrative recovery challenge")
    return {"Version": 1, "Action": "authorize-verified-recovery", "ID": str(uuid.uuid4()),
            "Reason": "isolated E2E host administrator verified restored publication and independent pins",
            "Binding": copy.deepcopy(target)}


def assert_foreign_preserved(before, after):
    import operator_secondary_cni as contract

    owned_macs = [worker["endpoint"]["mac"] for worker in before["workers"].values()]
    def foreign_paths(snapshot):
        return [path for path in contract.inventory_all_type2_paths(snapshot["frrRIB"])
                if not any(mac in path for mac in owned_macs)]
    if not foreign_paths(before) or foreign_paths(before) != foreign_paths(after):
        raise fixture.IsolationError("foreign BGP control route changed or was not observed")
    for node in before["workers"]:
        old, new = before["workers"][node], after["workers"][node]
        if (not all(contract.inventory_remote_kernel(old).values()) or
                contract.inventory_remote_kernel(old) != contract.inventory_remote_kernel(new)):
            raise fixture.IsolationError("foreign imported forwarding state changed")
        if old["import"].get("Receipts") != new["import"].get("Receipts"):
            raise fixture.IsolationError("foreign import ownership changed")


def await_fenced(test, endpoints, nodes):
    for _ in range(90):
        snapshot = test.gap_snapshot(endpoints)
        if all((snapshot["workers"][node]["export"].get("InventoryTrust") or {}).get("Phase") == "FENCED"
               and not (snapshot["workers"][node]["export"].get("Result") or {}).get("Applied")
               for node in nodes):
            snapshot = test.await_gap_routes(endpoints, [node for node in endpoints if node not in nodes], nodes)
            for node in nodes:
                worker = snapshot["workers"][node]
                inventory = worker["inventoryJournal"]["Record"]
                exported = worker["exportJournal"]["Record"]
                if (inventory.get("Pending") or exported.get("Pending") or
                        inventory["State"]["trust"]["Phase"] != "FENCED" or exported["State"].get("Receipts")):
                    raise fixture.IsolationError("fenced status is not backed by drained durable journals")
                speaker = worker["export"]["SpeakerProcess"]
                result = test.worker(node, "/opt/evpn/bin/type2rib", "-socket", "/run/gobgp/gobgp.sock",
                                     "-router-id", endpoints[node]["vtep"], "-pid", str(speaker["PID"]),
                                     "-start-ticks", speaker["StartTicks"])
                paths = json.loads(result.stdout)
                if any(path.get("Local") and (path.get("NLRI") or {}).get("MAC") == endpoints[node]["mac"]
                       and (path.get("NLRI") or {}).get("IP") == endpoints[node]["ip"] for path in paths):
                    raise fixture.IsolationError("owned route survived in actual local speaker RIB")
                worker["fencedLocalRIB"] = paths
            return snapshot
        time.sleep(1)
    raise fixture.IsolationError("durable security fence and owned withdrawal were not both observed")


def await_request_rejection(test, endpoints, node, fenced):
    for _ in range(60):
        snapshot = await_fenced(test, endpoints, fenced)
        if "scoped fresh administrative recovery authorization required" in snapshot["workers"][node]["export"].get("Error", ""):
            return snapshot
        time.sleep(1)
    raise fixture.IsolationError("recovery request rejection was not explicitly observed")


def run_startup_withdrawal(test, endpoints, identities, installation):
    """Keep actual speakers alive while replacement CLIs encounter denial."""
    import operator_secondary_cni as contract

    report = {"contract": "EVPN-REVIEW-007", "status": "IN_PROGRESS", "cases": {}}
    deployment = test.get_json("-n", "evpn-operator-system", "get", "deployment", "evpn-core-operator")
    ds = test.get_json("-n", "evpn-system", "get", "daemonset", "evpn-node-agent")
    agent = copy.deepcopy(ds["spec"]["template"]["spec"]["containers"][0])
    publication = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")

    def record():
        test.record("startup-withdrawal.json", report)

    def replace_agent(container):
        current = test.get_json("-n", "evpn-system", "get", "daemonset", "evpn-node-agent")
        if current["metadata"]["uid"] != ds["metadata"]["uid"]:
            raise fixture.IsolationError("startup test DaemonSet identity changed")
        test.k("-n", "evpn-system", "patch", "daemonset", "evpn-node-agent", "--type=json", "-p",
               json.dumps([{"op": "test", "path": "/metadata/resourceVersion",
                            "value": current["metadata"]["resourceVersion"]},
                           {"op": "replace", "path": "/spec/template/spec/containers/0", "value": container}]))
        test.k("-n", "evpn-system", "rollout", "status", "daemonset/evpn-node-agent", "--timeout=180s")

    def publish(data):
        current = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")
        if current["metadata"]["uid"] != publication["metadata"]["uid"]:
            raise fixture.IsolationError("startup test publication identity changed")
        test.k("-n", "evpn-system", "patch", "configmap", "evpn-worker-inventory", "--type=json", "-p",
               json.dumps([{"op": "test", "path": "/metadata/resourceVersion",
                            "value": current["metadata"]["resourceVersion"]},
                           {"op": "replace", "path": "/data", "value": data}]))

    def invoke(node, fatal=False):
        pods = test.get_json("-n", "evpn-system", "get", "pods", "-l", "app=evpn-node-agent")["items"]
        matches = [p for p in pods if p["spec"].get("nodeName") == node and
                   not p["metadata"].get("deletionTimestamp")]
        if len(matches) != 1:
            raise fixture.IsolationError("replacement CLI Pod identity ambiguous")
        args = [*agent["command"], *agent["args"]]
        if fatal:
            # An existing non-socket directory fails after both preflight
            # gates and CNI initialization, before either export loop starts.
            args.append("--socket=/run/evpn-node-agent")
        result = test.k("-n", "evpn-system", "exec", matches[0]["metadata"]["name"], "-c", "agent", "--",
                        *args, check=False, timeout=90)
        observation = {"returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr,
                       "podUID": matches[0]["metadata"]["uid"]}
        expected = "failed to start evpn-node-agent:" if fatal else "inventory startup blocked:"
        if result.returncode != 1 or expected not in result.stderr:
            raise fixture.IsolationError("replacement CLI did not reject startup: " + json.dumps(observation))
        return observation

    test.k("-n", "evpn-operator-system", "scale", "deployment/evpn-core-operator", "--replicas=0")
    test.k("-n", "evpn-operator-system", "rollout", "status", "deployment/evpn-core-operator", "--timeout=90s")
    try:
        for label in ("reachable", "socket-unavailable-then-reconnected", "post-preflight-fatal"):
            case = report["cases"][label] = {"status": "IN_PROGRESS", "before": test.gap_snapshot(endpoints)}
            record()
            sleeping = copy.deepcopy(agent)
            sleeping.update(command=["/bin/sleep"], args=["infinity"])
            sleeping.pop("readinessProbe", None)
            replace_agent(sleeping)
            retained = case["agentStoppedSpeakerSurvived"] = test.gap_snapshot(endpoints)
            contract.assert_inventory_no_mutation(case["before"], retained)
            if label != "post-preflight-fatal":
                invalid = copy.deepcopy(publication["data"])
                invalid["sha256"] = "0" * 64
                publish(invalid)
            case["attempts"] = {}
            for node in test.workers:
                socket = "/run/gobgp/gobgp.sock"
                if label == "socket-unavailable-then-reconnected":
                    test.worker(node, "mv", socket, socket + ".review007")
                    try:
                        failed = case["attempts"][node + "-unreachable"] = invoke(node)
                        if "owned startup withdrawal unresolved" not in failed["stderr"] or "unverified=true" not in failed["stderr"]:
                            raise fixture.IsolationError("unreachable cleanup claimed success")
                        failed["snapshot"] = test.gap_snapshot(endpoints)
                        journal = failed["snapshot"]["workers"][node]["inventoryJournal"]["Record"]["State"]["trust"]
                        if journal["Phase"] != "FENCED" or not contract.inventory_endpoint_paths(
                                failed["snapshot"]["frrRIB"], endpoints[node]):
                            raise fixture.IsolationError("unresolved cleanup did not retain both durable fence and live remote route")
                    finally:
                        test.worker(node, "mv", socket + ".review007", socket)
                attempt = case["attempts"][node] = invoke(node, fatal=label == "post-preflight-fatal")
                if "unverified=false error=<nil>" not in attempt["stderr"]:
                    raise fixture.IsolationError("reachable cleanup did not confirm exact withdrawal: " + attempt["stderr"])
            drained = case["drained"] = test.await_gap_routes(endpoints, [], test.workers)
            assert_foreign_preserved(case["before"], drained)
            for node in test.workers:
                old, new = case["before"]["workers"][node], drained["workers"][node]
                state = new["exportJournal"]["Record"]["State"]
                if state.get("Receipts") or not state.get("WithdrawalRequired"):
                    raise fixture.IsolationError("startup withdrawal lacks durable empty ownership barrier")
                if old["export"]["SpeakerProcess"] != new["export"]["SpeakerProcess"]:
                    raise fixture.IsolationError("startup test restarted the speaker")
            publish(publication["data"])
            replace_agent(agent)
            fenced = await_fenced(test, endpoints, test.workers)
            for node in test.workers:
                write_protected(test, node, REQUEST_PATH, json.dumps(recovery_request(fenced, node)))
                test.await_local_export(node, endpoints[node], identities[node], installation)
            case["recovered"] = test.await_gap_routes(endpoints, test.workers, [])
            case["status"] = "PASS"
            record()
        report["status"] = "PASS"
    except Exception as error:
        report.update(status="FAIL", error=str(error))
        raise
    finally:
        record()
        publish(publication["data"])
        replace_agent(agent)
        test.k("-n", "evpn-operator-system", "scale", "deployment/evpn-core-operator",
               "--replicas=" + str(deployment["spec"].get("replicas", 1)))
        test.k("-n", "evpn-operator-system", "rollout", "status", "deployment/evpn-core-operator", "--timeout=90s")
        # A newly scheduled Operator pod can finish IPv6 DAD after Ready.
        # Establish a stable baseline, without filtering any kernel route,
        # before the next negative-publication no-mutation assertions.
        previous, stable = test.gap_snapshot(endpoints), 0
        report["restorationStability"] = []
        for _ in range(30):
            time.sleep(1)
            current = test.gap_snapshot(endpoints)
            try:
                contract.assert_inventory_no_mutation(previous, current)
                stable += 1
            except contract.InventoryEvidenceError as error:
                stable = 0
                report["restorationStability"].append(str(error))
            if stable == 3:
                report["restorationStable"] = current
                record()
                break
            previous = current
        else:
            report.update(status="FAIL", error="startup fixture restoration did not settle")
            record()
            raise fixture.IsolationError(report["error"])


def run_authorization(test, endpoints, identities, installation):
    import operator_secondary_cni as contract

    report = {"id": "H054", "contract": "EVPN-REVIEW-004", "status": "IN_PROGRESS",
              "convergenceSeconds": 90, "cases": {}}
    publication = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")
    core = test.get_json("-n", "evpn-system", "get", "evpncore", "evpn-core")
    desired = copy.deepcopy(core["spec"]["workerInventory"])
    deployment = test.get_json("-n", "evpn-operator-system", "get", "deployment", "evpn-core-operator")
    replicas = deployment["spec"].get("replicas", 1)
    a, b = test.workers
    provider_path = "/etc/evpn/provider-attestation/provider.json"
    provider = test.worker(a, "cat", provider_path).stdout

    def record():
        test.record("inventory-gap-H054.json", report)

    def patch_core(candidate):
        return test.k("-n", "evpn-system", "patch", "evpncore", "evpn-core", "--type=merge", "-p",
                      json.dumps({"spec": {"workerInventory": candidate}}), check=False)

    def scale(count):
        current = test.get_json("-n", "evpn-operator-system", "get", "deployment", "evpn-core-operator")
        if current["metadata"]["uid"] != deployment["metadata"]["uid"]:
            raise fixture.IsolationError("Operator deployment identity changed")
        test.k("-n", "evpn-operator-system", "scale", "deployment/evpn-core-operator", "--replicas=" + str(count))
        test.k("-n", "evpn-operator-system", "rollout", "status", "deployment/evpn-core-operator", "--timeout=90s")

    def patch_publication(data):
        current = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")
        if current["metadata"]["uid"] != publication["metadata"]["uid"]:
            raise fixture.IsolationError("publication UID changed")
        test.k("-n", "evpn-system", "patch", "configmap", "evpn-worker-inventory", "--type=json", "-p",
               json.dumps([{"op": "test", "path": "/metadata/resourceVersion",
                            "value": current["metadata"]["resourceVersion"]},
                           {"op": "replace", "path": "/data", "value": data}]))

    def authorize(node, request, present):
        write_protected(test, node, REQUEST_PATH, json.dumps(request))
        path_uuid = test.await_local_export(node, endpoints[node], identities[node], installation)
        test.await_external_import(node, identities[node], installation, endpoints[node]["vtep"])
        snapshot = test.await_gap_routes(endpoints, present, [worker for worker in test.workers if worker not in present])
        trust = snapshot["workers"][node]["inventoryJournal"]["Record"]["State"]["trust"]
        exported = snapshot["workers"][node]["exportJournal"]["Record"]["State"]
        binding_sha = hashlib.sha256(json.dumps(trust["Binding"], separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
        if (trust["Phase"] != "RECOVERY_AUTHORIZED" or trust["RequestID"] != request["ID"] or
                trust["Generation"] != request["Binding"]["Generation"] or
                exported.get("InventoryReady") != binding_sha or exported.get("WithdrawalRequired")):
            raise fixture.IsolationError("re-advertisement lacks durable administrative authorization")
        test.verify_speaker_rib(node, endpoints[node], identities[node], installation, path_uuid)
        return snapshot

    def restart_agent(node, snapshot):
        pods = snapshot["workers"][node]["agentPods"]
        if len(pods) != 1:
            raise fixture.IsolationError("ambiguous agent restart")
        test.k("-n", "evpn-system", "delete", "pod", pods[0]["metadata"]["name"], "--wait=true", "--timeout=120s")
        test.k("-n", "evpn-system", "rollout", "status", "daemonset/evpn-node-agent", "--timeout=180s")

    def authorize_with_crash(node, request, present, phase):
        prefix = "/run/evpn-node-agent/trust-cut-" + str(uuid.uuid4())
        output, release, log = prefix + ".json", prefix + ".release", prefix + ".log"
        before = test.gap_snapshot(endpoints)
        previous_ready = before["workers"][node]["exportJournal"]["Record"]["State"].get("InventoryReady", "")
        test.docker("exec", "-d", node, "sh", "-ec", 'exec "$@" >"$0" 2>&1', log,
                    "/opt/evpn/bin/trustcut", "-confirm-isolated-worker", "-phase", phase,
                    "-request", request["ID"], "-previous-ready", previous_ready,
                    "-output", output, "-release", release)

        def wait_status(expected):
            for _ in range(90):
                result = test.worker(node, "cat", output, check=False)
                try:
                    observed = json.loads(result.stdout)
                except ValueError:
                    observed = {}
                if observed.get("status") == expected:
                    return observed
                time.sleep(0.5)
            diagnostic = test.worker(node, "cat", log, check=False).stdout
            raise fixture.IsolationError("actual recovery crash cut not observed: " + phase + ": " + diagnostic)

        case = report["cases"]["C-crash-" + phase] = {"status": "IN_PROGRESS", "request": request}
        record()
        try:
            case["armed"] = wait_status("READY")
            write_protected(test, node, REQUEST_PATH, json.dumps(request))
            case["injector"] = wait_status("CUT")
            cut = case["cut"] = test.gap_snapshot(endpoints)
            trust = cut["workers"][node]["inventoryJournal"]["Record"]["State"]["trust"]
            exported = cut["workers"][node]["exportJournal"]["Record"]["State"]
            expected_phase = "RECOVERY_AUTHORIZED" if phase == "final" else "RECOVERY_PENDING"
            if (trust["Phase"] != expected_phase or trust["RequestID"] != request["ID"] or
                    trust["Generation"] != request["Binding"]["Generation"] or exported.get("Receipts")):
                raise fixture.IsolationError("SIGKILL did not preserve the exact no-add recovery boundary")
            if contract.inventory_endpoint_paths(cut["frrRIB"], endpoints[node]):
                raise fixture.IsolationError("route advertised before the interrupted recovery completed")
            if phase in ("ready", "final"):
                digest = hashlib.sha256(json.dumps(trust["Binding"], separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
                if exported.get("InventoryReady") != digest:
                    raise fixture.IsolationError("READY crash cut lacks the exact cross-journal binding")
            assert_foreign_preserved(before, cut)
            record()
        finally:
            test.worker(node, "touch", release)
        case["released"] = wait_status("RELEASED")
        recovered = authorize(node, request, present)
        case.update(status="PASS", recovered=recovered,
                    injectorLog=test.worker(node, "cat", log, check=False).stdout)
        record()
        return recovered

    def corrupt_and_restore(label):
        before = test.gap_snapshot(endpoints)
        invalid = copy.deepcopy(publication["data"])
        invalid["sha256"] = "0" * 64
        case = report["cases"][label] = {"before": before, "invalidPublication": invalid, "status": "IN_PROGRESS"}
        record()
        patch_publication(invalid)
        case["during"] = await_fenced(test, endpoints, test.workers)
        case["events"] = test.get_json("-n", "evpn-system", "get", "events")
        assert_foreign_preserved(before, case["during"])
        patch_publication(publication["data"])
        case["restored"] = await_fenced(test, endpoints, test.workers)
        for node in test.workers:
            test.await_external_import(node, identities[node], installation, endpoints[node]["vtep"], require_export=False)
        case["restored"] = await_fenced(test, endpoints, test.workers)
        case["status"] = "PASS"
        record()
        return case["restored"]

    try:
        # A: invalid proposals enter through the real reconciliation boundary.
        candidates = {}
        for label in ("node-uid", "vtep", "stale-revision", "same-revision-digest", "network-policy"):
            candidate = copy.deepcopy(desired)
            if label == "node-uid":
                candidate["workers"][0]["nodeUID"] = str(uuid.uuid4())
            elif label == "vtep":
                candidate["workers"][0]["localVTEP"] = candidate["workers"][1]["localVTEP"]
            elif label == "stale-revision":
                candidate["revision"] = 0
            elif label == "same-revision-digest":
                candidate["workers"][0]["networks"][0]["logicalSwitch"] = "unapproved-switch"
            else:
                candidate["workers"][0]["networks"][0]["vni"] = 0
            candidates[label] = candidate
        for label, candidate in candidates.items():
            before = test.gap_snapshot(endpoints)
            if any(before["workers"][node]["export"].get("Error") for node in test.workers):
                raise fixture.IsolationError("candidate test requires healthy baseline")
            reservation = test.get_json("-n", "evpn-operator-system", "get", "configmap", "evpn-worker-reservations")
            case = report["cases"]["A-" + label] = {"status": "IN_PROGRESS", "candidate": candidate, "before": before}
            result = patch_core(candidate)
            case["admission"] = {"returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr}
            if not result.returncode:
                for _ in range(90):
                    observed = test.get_json("-n", "evpn-system", "get", "evpncore", "evpn-core")
                    case["lastOperatorObservation"] = observed
                    if candidate_rejected(observed):
                        case["rejection"] = observed
                        break
                    time.sleep(1)
                else:
                    raise fixture.IsolationError("candidate rejection was not independently observed")
            current = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")
            retained = test.get_json("-n", "evpn-operator-system", "get", "configmap", "evpn-worker-reservations")
            after = case["after"] = test.gap_snapshot(endpoints)
            if (current["metadata"]["uid"] != publication["metadata"]["uid"] or current["data"] != publication["data"] or
                    retained["metadata"]["uid"] != reservation["metadata"]["uid"] or retained["data"] != reservation["data"]):
                raise fixture.IsolationError("rejected candidate changed publication or reservation head")
            contract.assert_inventory_no_mutation(before, after)
            for node in test.workers:
                contract.assert_inventory_survivor(before, after, node)
                old = before["workers"][node]["inventoryJournal"]["Record"]["State"]
                new = after["workers"][node]["inventoryJournal"]["Record"]["State"]
                if old != new:
                    raise fixture.IsolationError("rejected candidate changed durable authority")
            if patch_core(desired).returncode:
                raise fixture.IsolationError("valid CR restoration failed")
            test.await_inventory_acceptance(identities, installation)
            case["status"] = "PASS"
            record()

        # B2: protected runtime identity loss is worker-local, not a proposal.
        scale(0)
        before = test.gap_snapshot(endpoints)
        local_case = report["cases"]["B-local-runtime-identity"] = {"status": "IN_PROGRESS", "before": before}
        record()
        invalid = json.loads(provider)
        invalid["standalone"]["nodeUID"] = str(uuid.uuid4())
        write_protected(test, a, provider_path, json.dumps(invalid))
        for _ in range(90):
            during = test.gap_snapshot(endpoints)
            if (during["workers"][a]["export"].get("InventoryTrust") or {}).get("Security"):
                break
            time.sleep(1)
        else:
            raise fixture.IsolationError("local identity incident did not persist")
        contract.assert_inventory_survivor(before, during, b)
        local_case["during"] = during
        write_protected(test, a, provider_path, provider)
        restored = await_fenced(test, endpoints, [a])
        contract.assert_inventory_survivor(before, restored, b)
        assert_foreign_preserved(before, restored)
        request = recovery_request(restored, a)
        local_case.update(restored=restored, request=request)
        record()
        recovered = authorize(a, request, test.workers)
        report["cases"]["B-local-runtime-identity"] = {
            "status": "PASS", "before": before, "during": during, "restored": restored,
            "request": request, "recovered": recovered,
            "survivorProbe": test.inventory_probe(endpoints[b], contract.REMOTE_IP, 43000),
        }
        record()

        restored = corrupt_and_restore("B-shared-publication-integrity")
        # B3: a legitimate higher revision cannot replace an admin grant.
        desired["revision"] += 1
        if patch_core(desired).returncode:
            raise fixture.IsolationError("valid higher revision rejected")
        scale(replicas)
        test.await_inventory_acceptance(identities, installation, require_fenced=True)
        publication = test.get_json("-n", "evpn-system", "get", "configmap", "evpn-worker-inventory")
        if int(publication["data"]["revision"]) != desired["revision"]:
            raise fixture.IsolationError("higher authoritative revision was not published")
        scale(0)
        restored = await_fenced(test, endpoints, test.workers)
        report["cases"]["B-higher-revision-still-fenced"] = {"status": "PASS", "observed": restored}
        record()
        requests = {node: recovery_request(restored, node) for node in test.workers}
        for field, value in (("NodeUID", str(uuid.uuid4())), ("InstallationUID", str(uuid.uuid4())),
                             ("Publication", "wrong/authority/uid"), ("Generation", 0),
                             ("Incident", str(uuid.uuid4())), ("Domain", "0" * 64),
                             ("Digest", "0" * 64), ("Revision", 1)):
            invalid_request = copy.deepcopy(requests[a])
            invalid_request["Binding"][field] = value
            write_protected(test, a, REQUEST_PATH, json.dumps(invalid_request))
            observed = await_request_rejection(test, endpoints, a, test.workers)
            if observed["workers"][a]["export"]["InventoryTrust"]["Generation"] != restored["workers"][a]["export"]["InventoryTrust"]["Generation"]:
                raise fixture.IsolationError("rejected request consumed a generation")
            report["cases"]["C-reject-" + field] = {"status": "PASS", "request": invalid_request, "observed": observed}
            record()
        restart_agent(a, restored)
        restored = await_fenced(test, endpoints, test.workers)
        report["cases"]["C-restart-fenced"] = {"status": "PASS", "observed": restored}
        for node in test.workers:
            recovered = authorize_with_crash(node, requests[node], [a] if node == a else test.workers,
                                            "prepared" if node == a else "ready")
            if node == a and recovered["workers"][b]["export"]["InventoryTrust"]["Phase"] != "FENCED":
                raise fixture.IsolationError("A's request released B")
            report["cases"]["C-authorized-" + node] = {"status": "PASS", "request": requests[node], "observed": recovered}
            record()
        before = test.gap_snapshot(endpoints)
        for node in test.workers:
            write_protected(test, node, REQUEST_PATH, json.dumps(requests[node]))
        restart_agent(a, before)
        test.await_local_export(a, endpoints[a], identities[a], installation)
        after = test.await_gap_routes(endpoints, test.workers)
        for node in test.workers:
            contract.assert_inventory_survivor(before, after, node)
        report["cases"]["C-idempotency-and-completed-restart"] = {"status": "PASS", "before": before, "after": after}
        record()
        restored = corrupt_and_restore("C-new-incident")
        for node in test.workers:
            observed = await_request_rejection(test, endpoints, node, test.workers)
            report["cases"]["C-replay-old-incident-" + node] = {"status": "PASS", "request": requests[node], "observed": observed}
            record()
        for node in test.workers:
            request = recovery_request(restored, node)
            if node == a:
                authorize_with_crash(node, request, [a], "final")
            else:
                authorize(node, request, test.workers)
        final = test.await_gap_routes(endpoints, test.workers)
        assert_foreign_preserved(report["cases"]["B-shared-publication-integrity"]["before"], final)
        report["dataplane"] = test.verify_inventory_recovered_dataplane(endpoints, "H054")
        report["status"] = "PASS"
    except Exception as error:
        report.update(status="FAIL", error=str(error), failureClass=getattr(error, "failure_class", "MISSING_OBSERVATION"))
        try:
            report["failureSnapshot"] = test.gap_snapshot(endpoints)
        except Exception as diagnostic_error:
            report["failureSnapshotError"] = str(diagnostic_error)
        raise
    finally:
        record()
        write_protected(test, a, provider_path, provider)
        patch_publication(publication["data"])
        if patch_core(desired).returncode:
            raise fixture.IsolationError("valid CR cleanup failed")
        scale(replicas)
