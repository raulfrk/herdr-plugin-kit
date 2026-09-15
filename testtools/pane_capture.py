"""Render a bounded Herdr ANSI pane capture to a deterministic test PNG."""

from __future__ import annotations

import argparse
import hashlib
import json
import unicodedata
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pyte
from PIL import Image, ImageDraw, ImageFont


_PALETTE = {
    "black": (0, 0, 0),
    "red": (205, 49, 49),
    "green": (13, 188, 121),
    "brown": (229, 229, 16),
    "blue": (36, 114, 200),
    "magenta": (188, 63, 188),
    "cyan": (17, 168, 205),
    "white": (229, 229, 229),
    "brightblack": (102, 102, 102),
    "brightred": (241, 76, 76),
    "brightgreen": (35, 209, 139),
    "brightbrown": (245, 245, 67),
    "brightblue": (59, 142, 234),
    "brightmagenta": (214, 112, 214),
    "brightcyan": (41, 184, 219),
    "brightwhite": (255, 255, 255),
}
_DEFAULT_FOREGROUND = (229, 229, 229)
_DEFAULT_BACKGROUND = (16, 20, 24)
_MAX_HERDR_ID_BYTES = 256


@dataclass(frozen=True)
class PaneCapture:
    ansi: str
    text: str
    columns: int
    rows: int
    workspace_id: str
    tab_id: str
    pane_id: str


def _color(value: str, default: tuple[int, int, int]) -> tuple[int, int, int]:
    if value == "default":
        return default
    if value in _PALETTE:
        return _PALETTE[value]
    if len(value) == 6:
        try:
            return tuple(int(value[index : index + 2], 16) for index in (0, 2, 4))
        except ValueError:
            pass
    raise ValueError(f"unsupported pyte color {value!r}")


def _visible_text(screen: pyte.Screen) -> str:
    return "\n".join(line.rstrip() for line in screen.display).rstrip("\n")


def _read_text_exact(path: Path) -> str:
    with path.open("r", encoding="utf-8", newline="") as stream:
        return stream.read()


def _validate_herdr_id(name: str, value: str) -> None:
    if (
        not value
        or len(value.encode("utf-8")) > _MAX_HERDR_ID_BYTES
        or any(unicodedata.category(character) == "Cc" for character in value)
    ):
        raise ValueError(f"{name} must be 1-{_MAX_HERDR_ID_BYTES} UTF-8 bytes without control characters")


def render_pane_capture(
    capture: PaneCapture,
    output: Path,
    font_size: int = 14,
    font_name: str = "DejaVuSansMono.ttf",
) -> dict[str, Any]:
    if capture.columns <= 0 or capture.rows <= 0:
        raise ValueError("pane geometry must be positive")
    if capture.columns * capture.rows > 200_000:
        raise ValueError("pane geometry exceeds the test capture bound")
    for name, value in {
        "workspace_id": capture.workspace_id,
        "tab_id": capture.tab_id,
        "pane_id": capture.pane_id,
    }.items():
        _validate_herdr_id(name, value)

    screen = pyte.Screen(capture.columns, capture.rows)
    pyte.Stream(screen).feed(capture.ansi)
    rendered_text = _visible_text(screen)
    if rendered_text != capture.text.rstrip("\n"):
        raise ValueError("ANSI and plain-text pane captures differ")

    font = ImageFont.truetype(font_name, font_size)
    font_path = Path(font.path)
    bounds = font.getbbox("M")
    cell_width = max(1, bounds[2] - bounds[0])
    cell_height = max(1, bounds[3] - bounds[1] + 4)
    image = Image.new("RGB", (capture.columns * cell_width, capture.rows * cell_height), _DEFAULT_BACKGROUND)
    draw = ImageDraw.Draw(image)
    cells: list[dict[str, Any]] = []

    for row in range(capture.rows):
        for column in range(capture.columns):
            char = screen.buffer[row][column]
            foreground = _color(char.fg, _DEFAULT_FOREGROUND)
            background = _color(char.bg, _DEFAULT_BACKGROUND)
            if char.reverse:
                foreground, background = background, foreground
            left = column * cell_width
            top = row * cell_height
            draw.rectangle((left, top, left + cell_width - 1, top + cell_height - 1), fill=background)
            if char.data != " ":
                draw.text((left, top), char.data, font=font, fill=foreground)
                if char.bold:
                    draw.text((left + 1, top), char.data, font=font, fill=foreground)
                if char.underscore:
                    draw.line((left, top + cell_height - 2, left + cell_width - 1, top + cell_height - 2), fill=foreground)
                cells.append(
                    {
                        "row": row,
                        "column": column,
                        "text": char.data,
                        "foreground": char.fg,
                        "background": char.bg,
                        "bold": char.bold,
                        "italics": char.italics,
                        "underscore": char.underscore,
                        "reverse": char.reverse,
                    }
                )

    output.parent.mkdir(parents=True, exist_ok=True)
    image.save(output, format="PNG", compress_level=9, optimize=False)
    png = output.read_bytes()
    return {
        "workspace_id": capture.workspace_id,
        "tab_id": capture.tab_id,
        "pane_id": capture.pane_id,
        "columns": capture.columns,
        "rows": capture.rows,
        "cell_width": cell_width,
        "cell_height": cell_height,
        "font": font_path.name,
        "font_sha256": hashlib.sha256(font_path.read_bytes()).hexdigest(),
        "ansi_sha256": hashlib.sha256(capture.ansi.encode("utf-8")).hexdigest(),
        "text_sha256": hashlib.sha256(capture.text.encode("utf-8")).hexdigest(),
        "png_sha256": hashlib.sha256(png).hexdigest(),
        "cells": cells,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ansi", type=Path, required=True)
    parser.add_argument("--text", type=Path, required=True)
    parser.add_argument("--columns", type=int, required=True)
    parser.add_argument("--rows", type=int, required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--tab", required=True)
    parser.add_argument("--pane", required=True)
    parser.add_argument("--font", default="DejaVuSansMono.ttf")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--metadata", type=Path, required=True)
    args = parser.parse_args()
    capture = PaneCapture(
        ansi=_read_text_exact(args.ansi),
        text=_read_text_exact(args.text),
        columns=args.columns,
        rows=args.rows,
        workspace_id=args.workspace,
        tab_id=args.tab,
        pane_id=args.pane,
    )
    metadata = render_pane_capture(capture, args.output, font_name=args.font)
    args.metadata.parent.mkdir(parents=True, exist_ok=True)
    args.metadata.write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
