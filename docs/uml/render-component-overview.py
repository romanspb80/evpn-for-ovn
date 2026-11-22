#!/usr/bin/env python3
"""Render the component overview and apply its horizontal layout corrections."""

import json
import os
from pathlib import Path
import re
import subprocess
import xml.etree.ElementTree as ET


SVG = "{http://www.w3.org/2000/svg}"
PAIR = re.compile(r"(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?)")
DIRECTORY = Path(__file__).resolve().parent
SOURCE = DIRECTORY / "component-overview.puml"
OUTPUT = SOURCE.with_suffix(".svg")
SHIFTED = {"core", "netlink", "ovsdb", "gobgp", "provider"}
HORIZONTAL = {"linux->ovn_controller", "ovn_controller->ovn", "ovn->peer"}


def pairs(value):
    return [(float(x), float(y)) for x, y in PAIR.findall(value)]


def shift_group(group, offset):
    for element in group:
        for attribute in ("d", "points"):
            if attribute in element.attrib:
                element.set(attribute, PAIR.sub(
                    lambda m: f"{float(m[1]) + offset:g},{m[2]}",
                    element.get(attribute)))
        if "x" in element.attrib:
            element.set("x", f"{float(element.get('x')) + offset:g}")


def render():
    environment = os.environ.copy()
    environment["JAVA_TOOL_OPTIONS"] = (
        environment.get("JAVA_TOOL_OPTIONS", "") + " -Djava.awt.headless=true"
    ).strip()
    subprocess.run(["plantuml", "-tsvg", str(SOURCE)], env=environment, check=True)
    source = SOURCE.read_text()
    dot = source[source.index("digraph"):source.rindex("@enddot")]
    layout = json.loads(subprocess.check_output(["dot", "-Tjson"], input=dot.encode()))
    objects = {item["name"]: item for item in layout["objects"]}
    ET.register_namespace("", SVG[1:-1])
    tree = ET.parse(OUTPUT)
    groups = {}
    for group in tree.iter(SVG + "g"):
        title = group.find(SVG + "title")
        if title is not None:
            groups.setdefault(title.text, []).append(group)
    right = max(x for x, _ in pairs(groups["neutron"][0].find(SVG + "path").get("d")))
    control_right = max(x for x, _ in pairs(
        groups["cluster_control"][0].find(SVG + "path").get("d")))
    offset = right - control_right
    for name in SHIFTED | {"cluster_framework", "cluster_control"}:
        assert len(groups[name]) == 1, name
        shift_group(groups[name][0], offset)

    for edge in tree.iter(SVG + "g"):
        if edge.get("class") != "edge":
            continue
        title = edge.find(SVG + "title").text
        tail, head = title.split("->")
        tail_offset = offset if tail in SHIFTED else 0
        head_offset = offset if head in SHIFTED else 0
        path = edge.find(SVG + "path")
        coordinates = pairs(path.get("d"))
        first, last = coordinates[0], coordinates[-1]
        if tail_offset or head_offset:
            # Interpolate endpoint movement without altering any vertical coordinate.
            axis = 1 if first[1] != last[1] else 0
            span = last[axis] - first[axis]
            assert span != 0, title

            def move(match):
                point = (float(match[1]), float(match[2]))
                fraction = max(0, min(1, (point[axis] - first[axis]) / span))
                dx = tail_offset + fraction * (head_offset - tail_offset)
                return f"{point[0] + dx:g},{match[2]}"

            path.set("d", PAIR.sub(move, path.get("d")))
            for polygon in edge.findall(SVG + "polygon"):
                tip = pairs(polygon.get("points"))[1]
                at_tail = sum((tip[i] - first[i]) ** 2 for i in (0, 1))
                at_head = sum((tip[i] - last[i]) ** 2 for i in (0, 1))
                dx = tail_offset if at_tail < at_head else head_offset
                polygon.set("points", PAIR.sub(
                    lambda m: f"{float(m[1]) + dx:g},{m[2]}",
                    polygon.get("points")))
            for text in edge.findall(SVG + "text"):
                text.set("x", f"{float(text.get('x')) + (tail_offset + head_offset) / 2:g}")

        if title in HORIZONTAL:
            assert first[1] == last[1], title
            endpoints = [first[0], last[0]]
            for polygon in edge.findall(SVG + "polygon"):
                endpoints.append(pairs(polygon.get("points"))[1][0])
            midpoint = (min(endpoints) + max(endpoints)) / 2
            annotations = edge.findall(SVG + "text")
            assert len(annotations) == 2, title
            assert float(annotations[0].get("y")) < first[1]
            assert float(annotations[1].get("y")) > first[1]
            for text in annotations:
                assert text.get("text-anchor") == "middle"
                text.set("x", f"{midpoint:g}")

    legend_x = float(objects["legend"]["pos"].split(",")[0])
    graph_center = float(layout["bb"].split(",")[2]) / 2
    shift_group(groups["legend"][0], graph_center - legend_x)
    # Remove unused horizontal canvas while retaining Graphviz's vertical padding.
    coordinates = []
    for group in groups.values():
        for item in group:
            if item.get("class") == "graph":
                continue
            for element in item:
                for attribute in ("d", "points"):
                    coordinates.extend(pairs(element.get(attribute, "")))
    left = min(x for x, _ in coordinates)
    content_right = max(x for x, _ in coordinates)
    root = tree.getroot()
    graph = root.find(SVG + "g")
    translation = re.search(r"translate\(([-\d.]+) ([-\d.]+)\)", graph.get("transform"))
    padding = float(translation[1])
    width = content_right - left + 2 * padding
    viewport = root.get("viewBox").split()
    viewport[0] = f"{left:g}"
    viewport[2] = f"{width:g}"
    root.set("viewBox", " ".join(viewport))
    root.set("width", f"{width:g}pt")
    tree.write(OUTPUT, encoding="UTF-8", xml_declaration=True)
    print(f"Aligned control-plane right edge: {right:g} pt; cluster shift: {offset:g} pt")


if __name__ == "__main__":
    render()
