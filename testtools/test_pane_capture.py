from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from pathlib import Path
import subprocess
import sys

import pytest
from PIL import Image

from testtools.pane_capture import PaneCapture, render_pane_capture


ANSI_FIXTURE = (
    "\x1b[2J\x1b[H\x1b[1;36mTitle\x1b[0m\r\n"
    "café λ\r\n"
    "\x1b[30;43;7mselect\x1b[0m"
    "\x1b[5;1Hfooter\x1b[5;16HZ"
)
TEXT_FIXTURE = "Title\ncafé λ\nselect\n\nfooter         Z"
ANSI_SHA256 = "b358e4def03d64e69c687e92383782b8b6c57133b131d6b21fe8328964e6ff30"
FONT_SHA256 = "0f5db4f1749979d961019838b160bec74abdf7f9eca69553fe1aa856bbff49a4"


def fixture_capture() -> PaneCapture:
    return PaneCapture(
        ansi=ANSI_FIXTURE,
        text=TEXT_FIXTURE,
        columns=16,
        rows=5,
        workspace_id="w16",
        tab_id="w16:t-fixture",
        pane_id="w16:p-fixture",
    )


def test_render_pane_capture_is_deterministic_and_visually_inspectable(tmp_path: Path) -> None:
    first = tmp_path / "first.png"
    second = tmp_path / "second.png"
    metadata = render_pane_capture(fixture_capture(), first)
    repeated = render_pane_capture(fixture_capture(), second)

    assert first.read_bytes() == second.read_bytes()
    assert metadata == repeated
    assert metadata["ansi_sha256"] == ANSI_SHA256
    assert metadata["text_sha256"] == hashlib.sha256(TEXT_FIXTURE.encode()).hexdigest()
    assert metadata["workspace_id"] == "w16"
    assert metadata["tab_id"] == "w16:t-fixture"
    assert metadata["pane_id"] == "w16:p-fixture"
    assert metadata["font"] == "DejaVuSansMono.ttf"
    assert metadata["font_sha256"] == FONT_SHA256

    cells = {(cell["row"], cell["column"]): cell for cell in metadata["cells"]}
    assert cells[0, 0] | {"text": "T", "foreground": "cyan", "bold": True} == cells[0, 0]
    assert cells[1, 3]["text"] == "é"
    assert cells[1, 5]["text"] == "λ"
    assert cells[2, 0] | {"text": "s", "foreground": "black", "background": "brown", "reverse": True} == cells[2, 0]
    assert cells[4, 0]["text"] == "f"
    assert cells[4, 15]["text"] == "Z"

    with Image.open(first) as image:
        assert image.size == (16 * metadata["cell_width"], 5 * metadata["cell_height"])
        assert image.getpixel((0, 2 * metadata["cell_height"])) == (0, 0, 0)


def test_capture_command_writes_png_and_metadata(tmp_path: Path) -> None:
    ansi = tmp_path / "pane.ansi"
    text = tmp_path / "pane.txt"
    output = tmp_path / "evidence" / "pane.png"
    metadata_path = tmp_path / "evidence" / "pane.json"
    ansi.write_text(ANSI_FIXTURE, encoding="utf-8")
    text.write_text(TEXT_FIXTURE, encoding="utf-8")

    subprocess.run(
        [
            sys.executable,
            "-m",
            "testtools.pane_capture",
            "--ansi",
            str(ansi),
            "--text",
            str(text),
            "--columns",
            "16",
            "--rows",
            "5",
            "--workspace",
            "w16",
            "--tab",
            "w16:t-fixture",
            "--pane",
            "w16:p-fixture",
            "--output",
            str(output),
            "--metadata",
            str(metadata_path),
        ],
        check=True,
        cwd=Path(__file__).parents[1],
        timeout=30,
    )

    metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
    assert output.is_file()
    assert metadata["png_sha256"] == hashlib.sha256(output.read_bytes()).hexdigest()
    assert metadata["ansi_sha256"] == ANSI_SHA256


def test_capture_accepts_maximum_supported_herdr_id(tmp_path: Path) -> None:
    capture = replace(fixture_capture(), pane_id="p" * 256)
    metadata = render_pane_capture(capture, tmp_path / "capture.png")
    assert metadata["pane_id"] == "p" * 256


@pytest.mark.parametrize(
    ("capture", "message"),
    [
        (PaneCapture("", "", 0, 5, "w", "t", "p"), "geometry"),
        (PaneCapture("", "different", 5, 5, "w", "t", "p"), "differ"),
        (PaneCapture("", "", 5, 5, "", "t", "p"), "workspace_id"),
        (PaneCapture("", "", 5, 5, "w" * 257, "t", "p"), "workspace_id"),
        (PaneCapture("", "", 5, 5, "w", "t\nother", "p"), "tab_id"),
        (PaneCapture("", "", 5, 5, "w", "t", "p\x00other"), "pane_id"),
    ],
)
def test_render_pane_capture_rejects_invalid_evidence(tmp_path: Path, capture: PaneCapture, message: str) -> None:
    with pytest.raises(ValueError, match=message):
        render_pane_capture(capture, tmp_path / "capture.png")
