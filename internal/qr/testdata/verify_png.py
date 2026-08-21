#!/usr/bin/env python3
"""Decode the PNGs the Go encoder produced, with an independent decoder.

The committed fixtures pin every module of every symbol against an independent
implementation. They cannot pin the last step: that the PNG this appliance
actually serves — at the scales it actually serves it, with its quiet zone and
its palette — is an image a scanner reads. That is what this checks.

It is the second half of `make qr-verify`; the first half is
internal/qr/corpus_test.go, which writes the files.

Usage:
    python3 verify_png.py <directory written by QR_PNG_OUT>
"""

import json
import os
import sys

try:
    import zxingcpp
    from PIL import Image
except ImportError as exc:  # pragma: no cover - operator-facing
    sys.exit(
        f"missing the decoder oracle: {exc}\n"
        "    python3 -m venv .venv && .venv/bin/pip install zxing-cpp pillow\n"
        "This target is deliberately outside CI, which has no Python."
    )


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    d = sys.argv[1]
    manifest = json.load(open(os.path.join(d, "manifest.json")))
    ok, bad = 0, []
    for name, expect in sorted(manifest.items()):
        res = zxingcpp.read_barcode(Image.open(os.path.join(d, name)))
        if res is not None and res.text == expect:
            ok += 1
        else:
            bad.append((name, None if res is None else res.text))
    for name, got in bad:
        print(f"FAIL {name}: {'no symbol found' if got is None else 'decoded ' + repr(got)[:60]}")
    print(f"decoded {ok}/{ok + len(bad)} PNGs produced by internal/qr")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
