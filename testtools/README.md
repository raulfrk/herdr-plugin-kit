# Pane capture test helper

This directory is release-verification tooling, not Plugin Kit runtime API.
Install the exact Python packages in `requirements-pane-capture.txt` and use a
monospace TrueType font. Every capture records the selected font filename and
SHA-256 alongside its ANSI, plain-text, geometry, Herdr IDs, cell attributes,
and PNG hashes. The repository fixture uses DejaVu Sans Mono with SHA-256
`0f5db4f1749979d961019838b160bec74abdf7f9eca69553fe1aa856bbff49a4`.

Capture the same pane twice so terminal bytes and presentation evidence remain
separate:

```sh
herdr --session NAME pane read --source visible --format ansi PANE >pane.ansi
herdr --session NAME pane read --source visible --format text PANE >pane.txt
python -m testtools.pane_capture \
  --ansi pane.ansi --text pane.txt --columns 120 --rows 40 \
  --workspace WORKSPACE --tab TAB --pane PANE \
  --output pane.png --metadata pane.json
```

The renderer rejects mismatched ANSI/plain captures and oversized geometry.
Review the resulting PNG visually; hashes alone do not establish fidelity.
