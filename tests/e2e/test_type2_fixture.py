import importlib.util
import ipaddress
import json
import subprocess
import tempfile
import tomllib
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
FIXTURE_SCRIPT = ROOT / "tools/e2e/evpn-type2-fixture.py"
SPEC = importlib.util.spec_from_file_location("evpn_type2_fixture", FIXTURE_SCRIPT)
FIXTURE = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(FIXTURE)


class FixtureHelpersTest(unittest.TestCase):
    def setUp(self):
        state = patch.object(FIXTURE.lab, "load", return_value={"name": "evpn-fixture-test"})
        state.start()
        self.addCleanup(state.stop)

    endpoint = {"mac": "02:00:00:00:00:01", "ip": "198.18.1.101"}
    vni = 1000
    rd = "198.18.1.3:1000"
    rt = "64512:1000"
    next_hop = "198.18.1.4"
    frr_peer = "198.18.1.3"

    def gobgp_route(self, **overrides):
        evpn = {
            "rd": self.rd,
            "macAddress": self.endpoint["mac"],
            "ipAddress": self.endpoint["ip"],
            "labels": [self.vni],
        }
        evpn.update(overrides.pop("nlri", {}))
        attrs = overrides.pop(
            "attrs",
            [
            {"type": 3, "value": {"address": self.next_hop}},
            {
                "type": 16,
                "value": [{"type": 0, "subType": 2, "value": self.rt}],
            },
            ],
        )
        path = {"nlri": {"evpn": evpn}, "attrs": attrs}
        peer_address = overrides.pop("peer_address", None)
        if peer_address is not None:
            path["peer-address"] = peer_address
        return json.dumps({"paths": [path]})

    def frr_route(self, **overrides):
        route_key = (
            f"[2]:[0]:[48]:[{self.endpoint['mac']}]:[32]:[{self.endpoint['ip']}]"
        )
        path = {
            "VNI": "1000",
            "vni": "1000",
            "aspath": {"string": "Local", "segments": [], "length": 0},
            "origin": "incomplete",
            "locPrf": 100,
            "valid": True,
            "version": 5,
            "bestpath": {"overall": True, "selectionReason": "First path received"},
            "extendedCommunity": {"string": f"RT:{self.rt} ET:8"},
            "nexthops": [{"ip": self.frr_peer, "afi": "ipv4", "accessible": True, "used": True}],
            "peer": {
                "peerId": self.frr_peer,
                "routerId": self.frr_peer,
                "hostname": "gobgp",
                "type": "internal",
            },
        }
        document = {
            "rd": self.rd,
            "routeType": 2,
            "ethTag": 0,
            "macLen": 48,
            "mac": self.endpoint["mac"],
            "ipLen": 32,
            "ip": self.endpoint["ip"],
            route_key: [[path]],
            "numPaths": 1,
        }
        for key, value in overrides.items():
            if key in {"rd", "routeType", "mac", "ip", "numPaths"}:
                document[key] = value
            elif value is None:
                path.pop(key, None)
            else:
                path[key] = value
        return json.dumps(document)

    def frr_table(self, present=True):
        route_key = (
            f"[2]:[0]:[48]:[{self.endpoint['mac']}]:[32]:[{self.endpoint['ip']}]"
        )
        routes = {}
        if present:
            routes[self.rd] = {
                "rd": self.rd,
                route_key: {
                    "prefix": route_key,
                    "prefixLen": 352,
                    "paths": [[{"routeType": 2}]],
                },
            }
        return json.dumps({**routes, "numPrefix": int(present), "numPaths": int(present)})

    def frr_observation(self, output=None, **options):
        return FIXTURE.route_observation(
            self.frr_route() if output is None else output,
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            options.pop("next_hop", self.frr_peer),
            "FRR",
            options.pop("originator", "gobgp"),
            options.pop("expected_peer", self.frr_peer),
            options.pop("expected_router_id", self.frr_peer),
        )

    def test_generated_subnet_is_isolated_benchmark_space(self):
        subnet = FIXTURE.generated_subnet("12345678-1234-5678-1234-567812345678")
        self.assertTrue(subnet.subnet_of(ipaddress.ip_network("198.18.0.0/15")))
        self.assertEqual(subnet.prefixlen, 24)

    def test_frr_107_per_rd_route_count_is_metadata_not_a_route(self):
        table = {"numPrefix": 0, "numPaths": 0,
                 self.rd: {"rd": self.rd, "numRoutes": 0}}
        self.assertTrue(FIXTURE._valid_frr_evpn_schema(table))
        for invalid in ("0", -1, True, {}):
            table[self.rd]["numRoutes"] = invalid
            self.assertFalse(FIXTURE._valid_frr_evpn_schema(table))

    def test_frr_107_exact_prefix_paths_preserve_all_route_attribute_checks(self):
        original = json.loads(self.frr_route())
        key = next(key for key in original if key.startswith("[2]:"))
        modern = {**original, "prefix": key, "paths": original[key],
                  "pathCount": original["numPaths"]}
        del modern[key]
        self.assertTrue(FIXTURE._valid_frr_exact_route_schema(modern))
        found, valid, detail = self.frr_observation(json.dumps(modern))
        self.assertEqual((found, valid), (True, True), detail)
        modern["paths"][0][0]["vni"] = "999"
        self.assertFalse(self.frr_observation(json.dumps(modern))[1])
        modern["pathCount"] = 2
        self.assertFalse(FIXTURE._valid_frr_exact_route_schema(modern))

    def test_route_observation_validates_gobgp_json_full_identity(self):
        found, valid, detail = FIXTURE.route_observation(
            self.gobgp_route(),
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
        )
        self.assertEqual((found, valid), (True, True), detail)

    def test_route_observation_enforces_explicit_gobgp_origin_and_frr_peer(self):
        local_origin = self.gobgp_route(
            attrs=[
                {"type": 3, "value": {"address": "0.0.0.0"}},
                {
                    "type": 16,
                    "value": [{"type": 0, "subType": 2, "value": self.rt}],
                },
            ]
        )
        found, valid, detail = FIXTURE.route_observation(
            local_origin,
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            "0.0.0.0",
            "GoBGP",
            "gobgp",
        )
        self.assertEqual((found, valid), (True, True), detail)

        received_from_frr = self.gobgp_route(peer_address=self.frr_peer)
        found, valid, detail = FIXTURE.route_observation(
            received_from_frr,
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
            "frr",
            self.frr_peer,
        )
        self.assertEqual((found, valid), (True, True), detail)

        found, valid, detail = FIXTURE.route_observation(
            self.gobgp_route(peer_address="198.18.1.9"),
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
            "frr",
            self.frr_peer,
        )
        self.assertTrue(found, detail)
        self.assertFalse(valid, detail)
        self.assertIn("expected FRR peer", detail)

    def test_captured_frr_exact_route_json_verifies_remote_gobgp_path(self):
        output = self.frr_route()
        document = json.loads(output)
        self.assertTrue(FIXTURE._valid_frr_exact_route_schema(document))
        found, valid, detail = self.frr_observation(output)
        self.assertEqual((found, valid), (True, True), detail)
        self.assertIn(f"peer={self.frr_peer}", detail)

    def test_preserved_frr_diagnostic_fixture_verifies_exact_received_route(self):
        fixture_path = (
            ROOT
            / "tests/e2e/type2-fixture/fixtures/frr-gobgp-exact-route.json"
        )
        document = json.loads(fixture_path.read_text())
        self.assertTrue(FIXTURE._valid_frr_exact_route_schema(document))
        endpoint = {"mac": document["mac"], "ip": document["ip"]}
        found, valid, detail = FIXTURE._frr_exact_route_observation(
            document,
            endpoint,
            1000,
            "198.18.74.3:1000",
            "64512:1000",
            "198.18.74.3",
            "gobgp",
            "198.18.74.3",
            "198.18.74.3",
        )
        self.assertEqual((found, valid), (True, True), detail)

    def test_preserved_frr_local_route_fixture_identifies_local_origin(self):
        fixture_path = (
            ROOT
            / "tests/e2e/type2-fixture/fixtures/frr-local-exact-route.json"
        )
        document = json.loads(fixture_path.read_text())
        self.assertTrue(FIXTURE._valid_frr_exact_route_schema(document))
        endpoint = {"mac": document["mac"], "ip": document["ip"]}
        found, valid, detail = FIXTURE._frr_exact_route_observation(
            document,
            endpoint,
            1000,
            "198.19.206.3:1000",
            "64512:1000",
            "198.19.206.4",
            "frr",
            "0.0.0.0",
            "198.19.206.2",
        )
        self.assertEqual((found, valid), (True, True), detail)

    def test_frr_exact_route_accepts_evidence_backed_lowercase_vni(self):
        output = self.frr_route(VNI=None, vni="1000")
        found, valid, detail = self.frr_observation(output)
        self.assertEqual((found, valid), (True, True), detail)
        output = self.frr_route(VNI="1000", vni="2000")
        found, valid, detail = self.frr_observation(output)
        self.assertTrue(found, detail)
        self.assertFalse(valid, detail)
        self.assertIn("VNI", detail)

    def test_exact_frr_query_uses_validated_rd_mac_and_ip(self):
        manifest = {
            "compose_project": "evpn-type2-test",
            "parameters": {"rd": self.rd},
        }
        expected = FIXTURE.JsonCommandOutput({}, "{}", "")
        with patch.object(
            FIXTURE, "run_json_command", return_value=expected
        ) as run_json:
            result = FIXTURE.frr_exact_route_output(
                Path("/tmp/run"),
                manifest,
                self.endpoint,
            )
        self.assertIs(result, expected)
        command = run_json.call_args.args[0]
        self.assertEqual(
            command[-2:],
            [
                "-c",
                "show bgp l2vpn evpn route rd "
                f"{self.rd} mac {self.endpoint['mac']} ip {self.endpoint['ip']} json",
            ],
        )
        with self.assertRaisesRegex(FIXTURE.FixtureError, "MAC"):
            FIXTURE.frr_exact_route_output(
                Path("/tmp/run"),
                manifest,
                {"mac": "invalid", "ip": self.endpoint["ip"]},
            )

    def test_frr_exact_route_rejects_incorrect_rd_rt_next_hop_and_peer(self):
        cases = (
            ("identity", self.frr_route(rd="198.18.1.9:1000")),
            (
                "RT",
                self.frr_route(
                    extendedCommunity={"string": "ET:8 RT:64512:2000"}
                ),
            ),
            (
                "next hop",
                self.frr_route(nexthops=[{"ip": "198.18.1.9"}]),
            ),
            (
                "peer provenance",
                self.frr_route(
                    peer={"peerId": "198.18.1.9", "routerId": "198.18.1.9"}
                ),
            ),
        )
        for expected, output in cases:
            with self.subTest(expected=expected):
                found, valid, detail = self.frr_observation(output)
                if expected == "identity":
                    self.assertIsNone(found, detail)
                else:
                    self.assertTrue(found, detail)
                self.assertFalse(valid, detail)
                self.assertIn(expected, detail)

    def test_frr_exact_route_rejects_missing_attributes(self):
        cases = (
            ("RT", self.frr_route(extendedCommunity={})),
            ("next hop", self.frr_route(nexthops=[])),
            ("VNI", self.frr_route(VNI=None, vni=None)),
        )
        for expected, output in cases:
            with self.subTest(expected=expected):
                found, valid, detail = self.frr_observation(output)
                self.assertTrue(found, detail)
                self.assertFalse(valid, detail)
                self.assertIn(expected, detail)

    def test_frr_exact_route_rejects_local_path_for_remote_assertion(self):
        found, valid, detail = self.frr_observation(
            self.frr_route(
                local=True,
                peer={"peerId": "0.0.0.0", "routerId": "198.18.1.2"},
            )
        )
        self.assertTrue(found, detail)
        self.assertFalse(valid, detail)
        self.assertIn("remote GoBGP peer provenance", detail)

    def test_frr_exact_route_rejects_inconsistent_path_count(self):
        found, valid, detail = self.frr_observation(
            self.frr_route(numPaths=0)
        )
        self.assertIsNone(found, detail)
        self.assertFalse(valid, detail)
        self.assertIn("inconsistent path data", detail)

    def test_route_direction_and_speaker_specific_next_hops_are_explicit(self):
        manifest = {
            "parameters": {
                "gobgp_address": "198.18.1.3",
                "frr_address": "198.18.1.2",
                "remote_vtep": "198.18.1.4",
                "gobgp_router_id": "198.18.1.3",
                "frr_router_id": "198.18.1.2",
            }
        }
        primary = {"name": "primary", "originator": "gobgp", "receiver": "frr"}
        secondary = {"name": "secondary", "originator": "frr", "receiver": "gobgp"}
        self.assertEqual(
            FIXTURE.route_expectations(manifest, primary, "GoBGP"),
            ("gobgp", "0.0.0.0", None, "198.18.1.3"),
        )
        self.assertEqual(
            FIXTURE.route_expectations(manifest, primary, "FRR"),
            ("gobgp", "198.18.1.3", "198.18.1.3", "198.18.1.3"),
        )
        local_endpoint = {
            "name": "local",
            "originator": "gobgp",
            "receiver": "frr",
            "local_vtep": "198.18.1.5",
        }
        self.assertEqual(
            FIXTURE.route_expectations(manifest, local_endpoint, "GoBGP"),
            ("gobgp", "198.18.1.5", None, "198.18.1.3"),
        )
        self.assertEqual(
            FIXTURE.route_expectations(manifest, local_endpoint, "FRR"),
            ("gobgp", "198.18.1.5", "198.18.1.3", "198.18.1.3"),
        )
        self.assertEqual(
            FIXTURE.route_expectations(manifest, secondary, "GoBGP"),
            ("frr", "198.18.1.4", "198.18.1.2", "198.18.1.2"),
        )
        self.assertEqual(
            FIXTURE.route_expectations(manifest, secondary, "FRR"),
            ("frr", "198.18.1.4", None, "198.18.1.2"),
        )

    def test_primary_withdrawal_requires_secondary_preservation_and_readd(self):
        withdrawn_primary = (False, False, ["primary absent"], "")
        present_secondary = (True, True, ["secondary valid"], "frr exact json")
        self.assertTrue(
            FIXTURE.route_state_confirmed(
                withdrawn_primary, present=False, sibling=present_secondary
            )
        )
        self.assertFalse(
            FIXTURE.route_state_confirmed(
                withdrawn_primary,
                present=False,
                sibling=(True, False, ["secondary attributes changed"], ""),
            )
        )
        readvertised_primary = (True, True, ["primary valid"], "frr exact json")
        self.assertTrue(
            FIXTURE.route_state_confirmed(
                readvertised_primary, present=True, sibling=present_secondary
            )
        )

    def test_wait_route_state_checks_primary_withdrawal_and_readvertisement(self):
        primary = {"name": "primary"}
        secondary = {"name": "secondary"}
        manifest = {"parameters": {}}
        outputs = (
            FIXTURE.JsonCommandOutput({}, "{}", ""),
            FIXTURE.JsonCommandOutput({}, "{}", ""),
        )
        observations = {
            "primary": (False, False, ["primary absent"], ""),
            "secondary": (True, True, ["secondary unchanged"], "secondary exact JSON"),
        }
        with (
            patch.object(FIXTURE, "route_outputs", return_value=outputs),
            patch.object(
                FIXTURE,
                "observe_route_pair",
                side_effect=lambda _directory, _manifest, endpoint, *_outputs: observations[
                    endpoint["name"]
                ],
            ) as observe,
        ):
            withdrawn = FIXTURE.wait_route_state(
                Path("/tmp/run"),
                manifest,
                primary,
                present=False,
                sibling=secondary,
            )
            self.assertEqual(withdrawn[0], outputs[0])
            self.assertIn("secondary exact JSON", withdrawn[1])
            self.assertEqual(
                [call.args[2]["name"] for call in observe.call_args_list],
                ["primary", "secondary"],
            )

            observations["primary"] = (
                True,
                True,
                ["primary restored"],
                "primary exact JSON",
            )
            observe.reset_mock()
            readvertised = FIXTURE.wait_route_state(
                Path("/tmp/run"),
                manifest,
                primary,
                present=True,
                sibling=secondary,
            )
            self.assertEqual(readvertised[0], outputs[0])
            self.assertIn("primary exact JSON", readvertised[1])
            self.assertIn("secondary exact JSON", readvertised[1])
            self.assertEqual(
                [call.args[2]["name"] for call in observe.call_args_list],
                ["primary", "secondary"],
            )

    def test_gobgp_primary_add_and_delete_reuse_the_same_route_identity(self):
        manifest = {
            "parameters": {
                "vni": 1000,
                "rd": self.rd,
                "rt": self.rt,
            }
        }
        endpoint = {
            "name": "primary",
            "mac": self.endpoint["mac"],
            "ip": self.endpoint["ip"],
        }
        with patch.object(FIXTURE, "container_exec", return_value="") as execute:
            FIXTURE.gobgp_route_command(Path("/tmp/run"), manifest, endpoint, "add")
            FIXTURE.gobgp_route_command(Path("/tmp/run"), manifest, endpoint, "del")
            FIXTURE.gobgp_route_command(Path("/tmp/run"), manifest, endpoint, "add")
        add_args = execute.call_args_list[0].args[3:]
        del_args = execute.call_args_list[1].args[3:]
        readvertise_args = execute.call_args_list[2].args[3:]
        self.assertEqual(add_args[:3], ("gobgp", "global", "rib"))
        self.assertEqual(del_args[:3], ("gobgp", "global", "rib"))
        self.assertEqual(readvertise_args, add_args)
        self.assertEqual(add_args[4:], del_args[4:])
        self.assertEqual((add_args[3], del_args[3]), ("add", "del"))

    def test_frr_exact_route_rejects_incorrect_type2_identity(self):
        for override in (
            {"rd": "198.18.1.9:1000"},
            {"routeType": 5},
            {"mac": "02:00:00:00:00:09"},
            {"ip": "198.18.1.109"},
        ):
            with self.subTest(override=override):
                found, valid, detail = self.frr_observation(
                    self.frr_route(**override)
                )
                self.assertIsNone(found, detail)
                self.assertFalse(valid, detail)
                self.assertIn("route", detail.lower())

    def test_frr_route_is_absent_from_captured_json_schema(self):
        found, valid, detail = FIXTURE.route_observation(
            self.frr_table(present=False),
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "FRR",
        )
        self.assertEqual((found, valid), (False, False))
        self.assertIn("absent", detail)

    def test_json_command_output_uses_stdout_and_preserves_stderr(self):
        stdout = self.frr_route()
        warning = "vtysh warning on stderr"
        parsed = FIXTURE.parse_json_command_output(
            stdout,
            warning,
            0,
            "FRR",
            FIXTURE._valid_frr_exact_route_schema,
        )
        self.assertEqual(parsed.document, json.loads(stdout))
        self.assertEqual(parsed.stdout, stdout)
        self.assertEqual(parsed.stderr, warning)

    def test_run_json_command_captures_stderr_separately(self):
        completed = FIXTURE.subprocess.CompletedProcess(
            ["vtysh"], 0, self.frr_route(), "vtysh warning"
        )
        with patch.object(FIXTURE.subprocess, "run", return_value=completed) as run:
            result = FIXTURE.run_json_command(
                ["vtysh"], "FRR", FIXTURE._valid_frr_exact_route_schema
            )
        self.assertEqual(result.stderr, "vtysh warning")
        self.assertEqual(result.document, json.loads(result.stdout))
        self.assertEqual(run.call_args.kwargs["stderr"], FIXTURE.subprocess.PIPE)

    def test_json_command_output_rejects_failed_empty_malformed_and_wrong_schema(self):
        cases = (
            (self.frr_route(), "", 1, "failed with exit code 1"),
            ("", "", 0, "empty stdout"),
            ("not-json", "", 0, "malformed stdout"),
            ("[]", "", 0, "unexpected schema"),
        )
        for stdout, stderr, status, message in cases:
            with self.subTest(message=message):
                with self.assertRaisesRegex(FIXTURE.FixtureError, message):
                    FIXTURE.parse_json_command_output(
                        stdout,
                        stderr,
                        status,
                        "FRR",
                        FIXTURE._valid_frr_exact_route_schema,
                    )

    def test_json_command_error_preserves_stderr_diagnostic(self):
        with self.assertRaises(FIXTURE.FixtureError) as raised:
            FIXTURE.parse_json_command_output(
                "not-json",
                "vtysh warning",
                0,
                "FRR",
                FIXTURE._valid_frr_exact_route_schema,
            )
        self.assertIn("vtysh warning", str(raised.exception))

    def test_frr_json_route_rejects_unexpected_schema(self):
        found, valid, detail = FIXTURE.route_observation(
            json.dumps({"routes": []}),
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "FRR",
        )
        self.assertEqual((found, valid), (None, False))
        self.assertIn("unexpected EVPN JSON schema", detail)

    def test_route_observation_rejects_each_incorrect_attribute(self):
        cases = (
            ("RD", self.gobgp_route(nlri={"rd": "198.18.1.9:1000"})),
            ("VNI", self.gobgp_route(nlri={"labels": [2000]})),
            (
                "RT",
                self.gobgp_route(
                    attrs=[
                        {
                            "type": 16,
                            "value": [{"type": 0, "subType": 2, "value": "64512:2000"}],
                        }
                    ]
                ),
            ),
            (
                "next hop",
                json.dumps(
                    {
                        "paths": [
                            {
                                "nlri": {
                                    "evpn": {
                                        "rd": self.rd,
                                        "macAddress": self.endpoint["mac"],
                                        "ipAddress": self.endpoint["ip"],
                                        "labels": [self.vni],
                                    }
                                },
                                "attrs": [
                                    {"type": 3, "value": {"address": "198.18.1.9"}},
                                    {
                                        "type": 16,
                                        "value": [{"type": 0, "subType": 2, "value": self.rt}],
                                    },
                                ],
                            }
                        ]
                    }
                ),
            ),
        )
        for expected, output in cases:
            with self.subTest(expected=expected):
                found, valid, detail = FIXTURE.route_observation(
                    output,
                    self.endpoint,
                    self.vni,
                    self.rd,
                    self.rt,
                    self.next_hop,
                    "GoBGP",
                )
                self.assertTrue(found, detail)
                self.assertFalse(valid, detail)
                self.assertIn(expected, detail)

    def test_route_observation_requires_mac_and_ip_on_the_same_path(self):
        wrong_ip = self.gobgp_route(
            nlri={"ipAddress": "198.18.1.102"}
        )
        wrong_mac = self.gobgp_route(
            nlri={"macAddress": "02:00:00:00:00:02"}
        )
        for output in (wrong_ip, wrong_mac):
            with self.subTest(output=output):
                found, valid, detail = FIXTURE.route_observation(
                    output,
                    self.endpoint,
                    self.vni,
                    self.rd,
                    self.rt,
                    self.next_hop,
                    "GoBGP",
                )
                self.assertEqual((found, valid), (False, False), detail)

        one_path_mac = json.loads(
            self.gobgp_route(nlri={"ipAddress": "198.18.1.102"})
        )["paths"][0]
        one_path_ip = json.loads(
            self.gobgp_route(nlri={"macAddress": "02:00:00:00:00:02"})
        )["paths"][0]
        mixed = json.dumps({"paths": [one_path_mac, one_path_ip]})
        found, valid, detail = FIXTURE.route_observation(
            mixed,
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
        )
        self.assertEqual((found, valid), (None, False), detail)
        self.assertIn("no recognized route/path object", detail)

    def test_route_observation_reports_missing_and_malformed_routes(self):
        found, valid, detail = FIXTURE.route_observation(
            "{}", self.endpoint, self.vni, self.rd, self.rt, self.next_hop, "GoBGP"
        )
        self.assertEqual((found, valid), (False, False))
        self.assertIn("absent", detail)
        found, valid, detail = FIXTURE.route_observation(
            "{not-json",
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
        )
        self.assertEqual((found, valid), (None, False))
        self.assertIn("malformed JSON", detail)
        unsupported = json.dumps(
            {
                "partial": [
                    {"mac": self.endpoint["mac"]},
                    {"ip": self.endpoint["ip"]},
                ]
            }
        )
        found, valid, detail = FIXTURE.route_observation(
            unsupported,
            self.endpoint,
            self.vni,
            self.rd,
            self.rt,
            self.next_hop,
            "GoBGP",
        )
        self.assertEqual((found, valid), (None, False))
        self.assertIn("no recognized route/path object", detail)

    def test_peer_observation_accepts_gobgp_abbreviated_summary_state(self):
        output = (
            "Peer            AS  Up/Down  State   | #Received  #Accepted\n"
            "198.18.1.2      64512 00:16:19 Establ  | 3          3\n"
        )
        established, detail = FIXTURE.peer_observation(
            output, "198.18.1.2", "GoBGP"
        )
        self.assertTrue(established, detail)

    def test_peer_observation_accepts_gobgp_and_frr_detailed_states(self):
        cases = (
            (
                "BGP neighbor is 198.18.1.2, remote AS 64512\n"
                "  BGP state = SESSION_STATE_ESTABLISHED, up for 00:00:47\n",
                "GoBGP",
            ),
            (
                "BGP neighbor is 198.18.1.3, remote AS 64512\n"
                "  BGP state = Established, up for 00:00:49\n",
                "FRR",
            ),
            (
                "BGP neighbor is 198.18.1.3, remote AS 64512\n"
                "  BGP state = Established, up for 00:00:49\n",
                "GoBGP",
            ),
        )
        for output, speaker in cases:
            with self.subTest(speaker=speaker, output=output):
                peer = "198.18.1.2" if "1.2," in output else "198.18.1.3"
                established, detail = FIXTURE.peer_observation(
                    output, peer, speaker
                )
                self.assertTrue(established, detail)

    def test_peer_observation_rejects_nonestablished_states(self):
        for state in ("Active", "Idle", "Connect", "OpenSent", "OpenConfirm"):
            output = (
                "BGP neighbor is 198.18.1.2, remote AS 64512\n"
                f"  BGP state = {state}\n"
            )
            with self.subTest(state=state):
                established, detail = FIXTURE.peer_observation(
                    output, "198.18.1.2", "GoBGP"
                )
                self.assertFalse(established, detail)

    def test_peer_observation_rejects_wrong_missing_and_malformed_output(self):
        wrong_peer = (
            "BGP neighbor is 198.18.1.9, remote AS 64512\n"
            "  BGP state = Established\n"
        )
        with self.assertRaisesRegex(FIXTURE.FixtureError, "wrong BGP peer"):
            FIXTURE.peer_observation(wrong_peer, "198.18.1.2", "GoBGP")
        for output in ("", "BGP state = Established\n", "peer unavailable"):
            with self.subTest(output=output):
                with self.assertRaises(FIXTURE.FixtureError):
                    FIXTURE.peer_observation(output, "198.18.1.2", "GoBGP")

    def test_peer_observation_rejects_failed_command(self):
        with self.assertRaisesRegex(FIXTURE.FixtureError, "exit code 1"):
            FIXTURE.peer_observation(
                "BGP neighbor is 198.18.1.2", "198.18.1.2", "GoBGP", 1
            )

    def test_evpn_family_observation_requires_negotiated_capability(self):
        cases = (
            ("", "negotiation evidence unavailable"),
            (
                "BGP neighbor is 198.18.1.2, remote AS 64512\n"
                "  multiprotocol:\n"
                "    l2vpn-evpn: advertised\n",
                "not confirmed as advertised and received",
            ),
        )
        for output, message in cases:
            with self.subTest(output=output):
                if not output:
                    output = "BGP neighbor is 198.18.1.2, remote AS 64512\n"
                negotiated, detail = FIXTURE.evpn_family_observation(
                    output, "198.18.1.2", "GoBGP"
                )
                self.assertFalse(negotiated, detail)
                self.assertIn(message, detail)

    def test_evpn_family_observation_accepts_captured_gobgp_and_frr_formats(self):
        cases = (
            (
                "BGP neighbor is 198.18.1.2, remote AS 64512\n"
                "  Neighbor capabilities:\n"
                "    multiprotocol:\n"
                "        l2vpn-evpn:\tadvertised and received\n",
                "GoBGP",
                "198.18.1.2",
            ),
            (
                "BGP neighbor is 198.18.1.3, remote AS 64512\n"
                "  Neighbor capabilities:\n"
                "    Address Family L2VPN EVPN: advertised and received\n",
                "FRR",
                "198.18.1.3",
            ),
        )
        for output, speaker, peer in cases:
            with self.subTest(speaker=speaker):
                negotiated, detail = FIXTURE.evpn_family_observation(
                    output, peer, speaker
                )
                self.assertTrue(negotiated, detail)

    def test_evpn_family_observation_rejects_failed_command(self):
        with self.assertRaisesRegex(FIXTURE.FixtureError, "exit code 127"):
            FIXTURE.evpn_family_observation(
                "", "198.18.1.2", "GoBGP", 127
            )

    def test_containerlab_uses_standalone_speaker_and_run_owned_frr_daemons(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            FIXTURE.lab.init(path, "type2", "test-fixture", "test-fixture", fixture={
                "run_id": "test", "project": "test", "subnet": "198.18.1.0/24",
                "images": {"gobgp": "gobgp@sha256:" + "a" * 64,
                           "frr": "frr@sha256:" + "b" * 64},
                "gobgp_address": "198.18.1.3", "frr_address": "198.18.1.2",
            })
            topology = json.loads((path / "lab.clab.yaml").read_text())
        nodes = list(topology["topology"]["nodes"].values())
        speaker, frr = nodes
        self.assertEqual(speaker["entrypoint"], "/usr/local/bin/gobgpd")
        for flag in ("--config-file", "--api-hosts 0.0.0.0:50051", "--api-insecure-allow-remote"):
            self.assertIn(flag, speaker["cmd"])
        self.assertEqual(frr["cap-add"], ["NET_ADMIN", "SYS_ADMIN"])
        self.assertFalse(frr["privileged"])
        self.assertNotIn("security-opts", frr)
        self.assertTrue(any(bind.endswith(":/etc/frr/daemons:ro") for bind in frr["binds"]))
        self.assertTrue(any(bind.endswith(":/etc/frr/vtysh.conf:ro") for bind in frr["binds"]))
        args = FIXTURE.parser().parse_args(
            [
                "start",
                "--gobgp-image",
                "registry.example/gobgp@sha256:" + "a" * 64,
                "--frr-image",
                "registry.example/frr@sha256:" + "b" * 64,
            ]
        )
        self.assertEqual((args.gobgp_asn, args.frr_asn), (64512, 64512))
        with self.assertRaises(SystemExit):
            FIXTURE.parser().parse_args(
                [
                    "start",
                    "--authorize-disposable",
                    "--gobgp-image",
                    "registry.example/gobgp@sha256:" + "a" * 64,
                    "--frr-image",
                    "registry.example/frr@sha256:" + "b" * 64,
                ]
            )

    def test_image_must_be_digest_pinned(self):
        self.assertEqual(
            FIXTURE.digest_image("registry.example/image@sha256:" + "a" * 64, "test"),
            "registry.example/image@sha256:" + "a" * 64,
        )
        with self.assertRaises(FIXTURE.FixtureError):
            FIXTURE.digest_image("registry.example/image:latest", "test")

    def test_default_macs_are_local_unicast_and_distinct(self):
        run_id = "12345678-1234-5678-1234-567812345678"
        macs = [FIXTURE.default_mac(run_id, index) for index in (1, 2, 3)]
        self.assertEqual(len(set(macs)), 3)
        for mac in macs:
            self.assertEqual(int(mac.split(":")[0], 16) & 3, 2)

    def test_named_mac_roles_are_deterministic_and_distinct(self):
        run_id = "12345678-1234-5678-1234-567812345678"
        roles = (
            "primary",
            "secondary",
            "bridge",
            "netlink-primary",
            "netlink-local-endpoint",
            "netlink-bridge",
        )
        macs = [FIXTURE.mac_for_role(run_id, role) for role in roles]
        self.assertEqual(macs, [FIXTURE.mac_for_role(run_id, role) for role in roles])
        self.assertEqual(len(set(macs)), len(roles))
        for mac in macs:
            self.assertEqual(int(mac.split(":")[0], 16) & 3, 2)

    def test_mac_identity_validation_rejects_duplicate_endpoint_mac(self):
        endpoints = [
            {"name": "primary", "mac": "02:00:00:00:00:01"},
            {"name": "secondary", "mac": "02:00:00:00:00:02"},
        ]
        local_endpoint = {"mac": "02:00:00:00:00:03"}
        with self.assertRaisesRegex(FIXTURE.FixtureError, "duplicate MAC identity"):
            FIXTURE.validate_mac_identities(
                "02:00:00:00:00:04",
                endpoints + [{"name": "duplicate", "mac": endpoints[0]["mac"]}],
                local_endpoint,
                "02:00:00:00:00:05",
            )

    def test_mac_identity_validation_rejects_multicast_mac(self):
        with self.assertRaisesRegex(
            FIXTURE.FixtureError, "unicast and locally administered"
        ):
            FIXTURE.validate_mac_identities(
                "02:00:00:00:00:04",
                [{"name": "primary", "mac": "01:00:00:00:00:02"}],
                {"mac": "02:00:00:00:00:03"},
                "02:00:00:00:00:05",
            )

    def test_generated_gobgp_config_is_valid_toml(self):
        values = {
            "gobgp_asn": 64512,
            "frr_asn": 64512,
            "gobgp_address": "198.18.1.3",
            "gobgp_router_id": "198.18.1.3",
            "frr_router_id": "198.18.1.2",
            "frr_address": "198.18.1.2",
            "remote_vtep": "198.18.1.4",
            "rd": "198.18.1.3:1000",
            "rt": "64512:1000",
            "vni": 1000,
        }
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            FIXTURE.write_configs(directory, values)
            config = tomllib.loads((directory / "gobgp.toml").read_text())
            daemons = (directory / "daemons").read_text()
            frr_config = (directory / "frr.conf").read_text()
            vtysh_config = (directory / "vtysh.conf").read_text()
        self.assertEqual(config["global"]["config"]["as"], 64512)
        self.assertEqual(
            config["neighbors"][0]["config"]["neighbor-address"],
            "198.18.1.2",
        )
        self.assertEqual(
            config["neighbors"][0]["afi-safis"][0]["config"]["afi-safi-name"],
            "l2vpn-evpn",
        )
        self.assertIn("rd 198.18.1.3:1000", frr_config)
        self.assertIn("route-target import 64512:1000", frr_config)
        self.assertIn("route-target export 64512:1000", frr_config)
        self.assertIn("bgpd=yes", daemons)
        self.assertIn("zebra=yes", daemons)
        self.assertIn("vtysh_enable=yes", daemons)
        self.assertEqual(
            vtysh_config,
            "service integrated-vtysh-config\nhostname evpn-type2-frr\n",
        )


class FixtureTopologyTest(unittest.TestCase):
    def setUp(self):
        state = patch.object(FIXTURE.lab, "load", return_value={"name": "evpn-fixture-test"})
        state.start()
        self.addCleanup(state.stop)

    run_id = "12345678-1234-5678-1234-567812345678"
    namespace = "evpn-ep-123456781234"
    parameters = {
        "remote_vtep": "198.18.1.4",
        "vni": 1000,
        "udp_port": 4789,
        "bridge_mac": "02:00:00:00:00:03",
        "endpoint_subnet": "198.18.2.0/24",
        "endpoint_gateway": "198.18.2.1",
        "endpoint_namespace": namespace,
        "endpoints": [
            {
                "name": "primary",
                "mac": "02:00:00:00:00:01",
                "ip": "198.18.1.101",
                "originator": "gobgp",
                "receiver": "frr",
                "state": "not-advertised",
            },
            {
                "name": "secondary",
                "mac": "02:00:00:00:00:02",
                "ip": "198.18.2.2",
                "originator": "frr",
                "receiver": "gobgp",
                "state": "not-advertised",
            },
            {
                "name": "netlink-primary",
                "mac": "02:00:00:00:00:04",
                "ip": "198.18.2.3",
                "originator": "frr",
                "receiver": "gobgp",
                "state": "not-advertised",
            },
        ],
    }

    def test_endpoint_subnet_is_distinct_and_avoids_existing_docker_networks(self):
        fixture_subnet = ipaddress.ip_network("198.18.1.0/24")
        with patch.object(
            FIXTURE,
            "network_overlaps_existing",
            side_effect=[["conflict"], []],
        ) as overlaps:
            endpoint_subnet = FIXTURE.generated_endpoint_subnet(
                self.run_id, fixture_subnet
            )
        self.assertNotEqual(endpoint_subnet, fixture_subnet)
        self.assertEqual(overlaps.call_count, 2)

    def test_endpoint_namespace_name_is_run_owned_and_deterministic(self):
        first = FIXTURE.endpoint_namespace_name(self.run_id)
        self.assertEqual(first, self.namespace)
        self.assertEqual(first, FIXTURE.endpoint_namespace_name(self.run_id))
        self.assertNotEqual(first, FIXTURE.endpoint_namespace_name(
            "87654321-4321-8765-4321-876543218765"
        ))

    def test_endpoint_namespace_is_process_owned_without_netns_mount(self):
        command = FIXTURE.endpoint_namespace_start_command(self.parameters)
        command_text = " ".join(command)
        self.assertIn("unshare --net --fork", command_text)
        self.assertIn("/tmp/evpn-ep-123456781234.pid", command_text)
        self.assertNotIn("ip netns add", command_text)
        self.assertNotIn("mount", command_text)

    def test_frr_topology_creates_svi_vni_bridge_and_process_owned_endpoint(self):
        commands = FIXTURE.frr_topology_commands(self.parameters, 456)
        self.assertIn(
            [
                "ip",
                "link",
                "add",
                "br-e2e",
                "type",
                "bridge",
                "vlan_filtering",
                "0",
            ],
            commands,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "ep2", "netns", "456"],
            commands,
        )
        self.assertIn(
            ["ip", "address", "add", "198.18.2.1/24", "dev", "br-e2e"],
            commands,
        )
        self.assertIn(
            [
                "nsenter",
                "--target",
                "456",
                "--net",
                "ip",
                "address",
                "add",
                "198.18.2.2/24",
                "dev",
                "ep2",
            ],
            commands,
        )
        self.assertFalse(any("netns" in command and "add" in command for command in commands))
        self.assertTrue(any(command[:3] == ["nsenter", "--target", "456"] for command in commands))
        self.assertIn(
            ["ip", "link", "set", "dev", "sw2", "master", "br-e2e"],
            commands,
        )
        self.assertIn(
            ["ip", "link", "add", "ep3", "type", "veth", "peer", "name", "sw3"],
            commands,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "ep3", "netns", "456"],
            commands,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "sw3", "master", "br-e2e"],
            commands,
        )
        self.assertIn(
            [
                "nsenter", "--target", "456", "--net", "ip", "address",
                "add", "198.18.2.3/24", "dev", "ep3",
            ],
            commands,
        )
        self.assertIn(
            [
                "ip",
                "link",
                "add",
                "vx-e2e",
                "type",
                "vxlan",
                "id",
                "1000",
                "local",
                "198.18.1.4",
                "dstport",
                "4789",
                "nolearning",
            ],
            commands,
        )
        self.assertFalse(
            any("neigh" in command or "fdb" in command for command in commands)
        )

    def test_secondary_advertisement_triggers_endpoint_generated_arp(self):
        commands = FIXTURE.secondary_advertisement_commands(self.parameters, 456)
        self.assertEqual(
            commands,
            [
                [
                    "nsenter",
                    "--target",
                    "456",
                    "--net",
                    "ping",
                    "-n",
                    "-I",
                    "ep2",
                    "-c",
                    "1",
                    "-W",
                    "2",
                    "198.18.2.1",
                ]
            ],
        )

    def test_netlink_primary_advertisement_and_withdrawal_use_distinct_endpoint(self):
        endpoint = next(
            ep for ep in self.parameters["endpoints"]
            if ep["name"] == "netlink-primary"
        )
        advertise = FIXTURE.frr_endpoint_advertisement_commands(
            self.parameters, endpoint, 456
        )
        self.assertEqual(
            advertise[-1],
            [
                "nsenter", "--target", "456", "--net", "ping", "-n",
                "-I", "ep3", "-c", "1", "-W", "2", "198.18.2.1",
            ],
        )
        self.assertIn(
            ["nsenter", "--target", "456", "--net", "ip", "link", "set",
             "dev", "ep3", "up"],
            FIXTURE.frr_topology_commands(self.parameters, 456),
        )
        withdrawal = FIXTURE.frr_endpoint_withdrawal_commands(
            self.parameters, endpoint, 456
        )
        self.assertIn(
            ["nsenter", "--target", "456", "--net", "ip", "address", "del",
             "198.18.2.3/24", "dev", "ep3"],
            withdrawal,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "sw3", "nomaster"],
            withdrawal,
        )
        readvertisement = FIXTURE.frr_endpoint_advertisement_commands(
            self.parameters, endpoint, 456, reactivate=True
        )
        self.assertIn(
            ["nsenter", "--target", "456", "--net", "ip", "address", "add",
             "198.18.2.3/24", "dev", "ep3"],
            readvertisement,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "sw3", "master", "br-e2e"],
            readvertisement,
        )

    def test_bounded_arp_captures_cover_endpoint_and_bridge_without_vxlan(self):
        manifest = {"compose_project": "evpn-fixture-test"}
        commands = FIXTURE.arp_capture_commands(
            Path("/tmp/run"), manifest, 456
        )
        self.assertEqual(set(commands), {
            "endpoint-ep2",
            "bridge-sw2",
            "bridge-br-e2e",
        })
        self.assertIn("nsenter", commands["endpoint-ep2"])
        for name, interface in (
            ("endpoint-ep2", "ep2"),
            ("bridge-sw2", "sw2"),
            ("bridge-br-e2e", "br-e2e"),
        ):
            self.assertIn("timeout", commands[name])
            self.assertIn("20", commands[name])
            self.assertIn("tcpdump", commands[name])
            self.assertEqual(commands[name][commands[name].index("-i") + 1], interface)
            self.assertEqual(commands[name][-1], "arp")
        self.assertFalse(any("vx-e2e" in command for command in commands.values()))
        netlink_primary = next(
            ep for ep in self.parameters["endpoints"]
            if ep["name"] == "netlink-primary"
        )
        endpoint_commands = FIXTURE.arp_capture_commands(
            Path("/tmp/run"),
            {"compose_project": "evpn-fixture-test", "parameters": self.parameters},
            456,
            netlink_primary,
        )
        self.assertEqual(
            set(endpoint_commands),
            {
                "netlink-primary-endpoint-ep3",
                "netlink-primary-bridge-sw3",
                "netlink-primary-bridge-br-e2e",
            },
        )
        self.assertEqual(
            endpoint_commands["netlink-primary-endpoint-ep3"][
                endpoint_commands["netlink-primary-endpoint-ep3"].index("-i") + 1
            ],
            "ep3",
        )

    def test_secondary_withdrawal_and_readvertisement_remove_and_restore_endpoint(self):
        withdrawal = FIXTURE.secondary_withdrawal_commands(self.parameters, 456)
        self.assertIn(
            [
                "nsenter",
                "--target",
                "456",
                "--net",
                "ip",
                "address",
                "del",
                "198.18.2.2/24",
                "dev",
                "ep2",
            ],
            withdrawal,
        )
        self.assertIn(
            ["ip", "neigh", "del", "198.18.2.2", "dev", "br-e2e"],
            withdrawal,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "sw2", "nomaster"],
            withdrawal,
        )
        readvertisement = FIXTURE.secondary_advertisement_commands(
            self.parameters, 456, reactivate=True
        )
        self.assertIn(
            [
                "nsenter",
                "--target",
                "456",
                "--net",
                "ip",
                "address",
                "add",
                "198.18.2.2/24",
                "dev",
                "ep2",
            ],
            readvertisement,
        )
        self.assertIn(
            ["ip", "link", "set", "dev", "sw2", "master", "br-e2e"],
            readvertisement,
        )
        self.assertEqual(readvertisement[-1][-1], "198.18.2.1")
        self.assertFalse(
            any("replace" in command for command in withdrawal + readvertisement)
        )

    def test_cleanup_removes_only_run_namespace_and_fixture_interfaces(self):
        manifest = {"parameters": {"endpoint_namespace": self.namespace}}
        with patch.object(
            FIXTURE,
            "container_exec",
            side_effect=[
                "",
                "3: br-e2e: <UP>\n5: sw2@if6: <UP>\n7: sw3@if8: <UP>\n",
                "",
                "",
                "",
            ],
        ) as execute:
            FIXTURE.cleanup_frr_topology(Path("/tmp/run"), manifest)
        self.assertEqual(
            [call.args[3:] for call in execute.call_args_list],
            [
                ("sh", "-ec", unittest.mock.ANY),
                ("ip", "-o", "link", "show"),
                ("ip", "link", "del", "dev", "sw2"),
                ("ip", "link", "del", "dev", "sw3"),
                ("ip", "link", "del", "dev", "br-e2e"),
            ],
        )

    def test_frr_topology_setup_rolls_back_after_command_failure(self):
        manifest = {
            "compose_project": "evpn-fixture-test",
            "parameters": self.parameters,
        }
        with (
            patch.object(
                FIXTURE,
                "frr_topology_commands",
                return_value=[["ip", "first"], ["ip", "second"]],
            ),
            patch.object(
                FIXTURE,
                "endpoint_namespace_start_command",
                return_value=["sh", "-c", "start"],
            ),
            patch.object(
                FIXTURE,
                "run_command",
                return_value=subprocess.CompletedProcess(
                    args=[], returncode=0, stdout="123\n", stderr=""
                ),
            ),
            patch.object(
                FIXTURE,
                "container_exec",
                side_effect=[
                    "",
                    "net:[4026533000]\nnet:[4026531840]",
                    "",
                    FIXTURE.FixtureError("second command failed"),
                ],
            ),
            patch.object(FIXTURE, "cleanup_frr_topology") as cleanup,
        ):
            with self.assertRaisesRegex(FIXTURE.FixtureError, "second command failed"):
                FIXTURE.setup_frr_topology(Path("/tmp/run"), manifest)
        cleanup.assert_called_once_with(Path("/tmp/run"), manifest)

    def test_cleanup_failure_is_reported_with_setup_failure(self):
        manifest = {
            "compose_project": "evpn-fixture-test",
            "parameters": self.parameters,
        }
        with (
            patch.object(
                FIXTURE,
                "frr_topology_commands",
                return_value=[["ip", "first"]],
            ),
            patch.object(
                FIXTURE,
                "endpoint_namespace_start_command",
                return_value=["sh", "-c", "start"],
            ),
            patch.object(
                FIXTURE,
                "run_command",
                return_value=subprocess.CompletedProcess(
                    args=[], returncode=0, stdout="123\n", stderr=""
                ),
            ),
            patch.object(
                FIXTURE,
                "container_exec",
                side_effect=[
                    "",
                    "net:[4026533000]\nnet:[4026531840]",
                    FIXTURE.FixtureError("setup failed"),
                ],
            ),
            patch.object(
                FIXTURE,
                "cleanup_frr_topology",
                side_effect=FIXTURE.FixtureError("rollback failed"),
            ),
        ):
            with self.assertRaisesRegex(
                FIXTURE.FixtureError,
                "setup failed.*topology rollback failed: rollback failed",
            ):
                FIXTURE.setup_frr_topology(Path("/tmp/run"), manifest)


if __name__ == "__main__":
    unittest.main()
