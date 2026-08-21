#!/usr/bin/env python3
"""Generate the golden QR vectors internal/qr is tested against.

WHY THIS EXISTS. A QR code is an artifact nobody on the build team can read. A
Go test that checks our encoder against our own understanding of ISO/IEC 18004
would pass for a bitmap no phone can scan — which is why the §10.4 QR was left
unbuilt in an earlier pass, with the reason recorded in session_admin.go.

So the expected output is not written by hand and not produced by the code under
test. It comes from implementations with no relationship to this repo:

  * python-qrcode (lincolnloop) produces the expected MODULE MATRIX, the chosen
    version and the chosen mask. Our encoder must reproduce all three bit for
    bit.

  * zxing-cpp — the C++ ZXing engine, ancestor of the decoders in a great many
    phone scanner apps — DECODES the reference symbol and must return the exact
    input string. That closes the loop that matters at the auditorium door: not
    "is this a well-formed matrix" but "does a scanner get the payload back".

  * segno is run as a THIRD opinion and its agreement is recorded per case, but
    it is not the authority. See below.

WHY THE FIXTURES ARE INDEXED BY MASK. Implementations genuinely disagree about
which of the eight data masks to choose, because the penalty rules are evaluated
on details the standard leaves open — whether the quiet zone counts as the light
margin for a finder-lookalike at the symbol edge, and whether the format
information is in place while the masks are being scored (python-qrcode blanks it
during evaluation; ZXing and Nayuki do not). None of that affects READABILITY:
the chosen mask is recorded in the format information, so a scanner reads our
answer rather than reproducing our reasoning.

So these fixtures pin every mask rather than one. For each case the reference is
generated eight times, once per forced mask, and each of those eight is decoded
by zxing-cpp before it is committed. Whatever mask our encoder picks, its output
must be byte-identical to a reference symbol that an independent decoder has
already read back correctly — which is the guarantee that matters, and it does
not require agreeing with anyone about mask selection.

WHAT THE THIRD OPINION FOUND, because it is the reason this file names three
implementations instead of one. The first version of this script used segno as
the matrix oracle, and our encoder disagreed with it on every case — always in
the padding, always by one codeword. segno's `write_padding_bits` is

    buff.extend([0] * (8 - (length % 8)))

with no outer `% 8`, so when the bit stream ALREADY ends on a codeword boundary
it appends a whole extra 0x00 codeword. ISO/IEC 18004 §7.4.10 — quoted verbatim
in segno's own docstring immediately above that line — says padding bits are
added only "if the bit stream length is such that it does not end at a codeword
boundary". In byte mode the stream is always byte-aligned after the terminator
(4 + 8 or 16 + 8n + 4 bits), so segno takes that branch every time.

It is a harmless deviation — the extra 0x00 lands in pad territory, both symbols
decode to the same payload, and zxing reads either — but it is a deviation, and
python-qrcode agrees with this package module-for-module (0 differing modules)
while disagreeing with segno by 146 on the same input. Two independent
implementations against one, with the spec text as the tiebreak.

The fixtures are committed, so CI needs neither Python nor a network. Re-running
this script requires both packages; it exits loudly rather than skipping.

Usage:
    python3 generate_vectors.py            > vectors.json
    python3 generate_vectors.py exhaustive > exhaustive.json
"""

import hashlib
import io
import json
import sys

try:
    import qrcode
    from qrcode.util import QRData, MODE_8BIT_BYTE
    import segno
    import zxingcpp
    from PIL import Image
except ImportError as exc:  # pragma: no cover - operator-facing
    sys.exit(
        f"missing an oracle: {exc}\n"
        "This script deliberately depends on INDEPENDENT implementations:\n"
        "    python3 -m venv .venv && .venv/bin/pip install qrcode segno zxing-cpp pillow\n"
        "The committed JSON is what the Go tests use; regenerate only when adding cases."
    )

ECC_LEVELS = {
    "L": qrcode.constants.ERROR_CORRECT_L,
    "M": qrcode.constants.ERROR_CORRECT_M,
    "Q": qrcode.constants.ERROR_CORRECT_Q,
    "H": qrcode.constants.ERROR_CORRECT_H,
}

# The corpus. Each case names WHY it is here, so a future reader can tell a
# deliberate boundary case from a random string.
CASES = [
    # The real thing: a per-show join URL exactly as internal/web builds it
    # (§7.2 "the local URL + session token"). This payload decides the symbol
    # version a booth actually prints, and therefore how big the card has to be.
    (
        "join_url_typical",
        "http://192.168.44.1:8080/accessibility?t=v1.YXVkaXRvcml1bS0zfHNob3ctOTkyfHNlc3MtQUJDRDEyMzR8MTc1NTcyMDAwMA.9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a",
        "M",
    ),
    # A longer auditorium/show id pushes into the next version: the encoder must
    # pick the version from capacity, not from a guess about typical length.
    (
        "join_url_long",
        "http://192.168.100.254:8080/accessibility?t=v1." + "Q" * 120 + "." + "a1b2c3d4" * 8,
        "M",
    ),
    # Short payloads: v1 has no alignment pattern and v2 has exactly one, which
    # is the first place a function-pattern bug shows.
    ("short_ascii", "CB", "M"),
    ("version1_boundary", "A" * 14, "M"),  # the last v1-M byte-mode payload
    ("version2_boundary", "A" * 15, "M"),  # one more byte forces v2
    # Every ECC level on one payload: the format-information bits and the block
    # structure differ for each, and a level mixed up is invisible to the eye.
    ("ecc_low", "http://192.168.44.1:8080/accessibility", "L"),
    ("ecc_medium", "http://192.168.44.1:8080/accessibility", "M"),
    ("ecc_quartile", "http://192.168.44.1:8080/accessibility", "Q"),
    ("ecc_high", "http://192.168.44.1:8080/accessibility", "H"),
    # Version >= 7 carries an 18-bit version information block in two places.
    # Nothing below v7 exercises it, and a missing one still looks like a QR.
    ("version7_plus", "B" * 200, "M"),
    ("large", "C" * 700, "M"),
    # Multi-block interleaving: above a threshold the codewords split into blocks
    # of two sizes and interleave. A wrong split produces a symbol that scans
    # clean and fails as soon as error correction has to do any work — the worst
    # failure mode available, because it passes every bench test.
    ("multiblock", "D" * 400, "Q"),
    # Non-ASCII: the join URL is ASCII today, but an auditorium name need not be,
    # and byte mode is UTF-8 by convention.
    ("utf8", "Auditorium 3 — Salle 3 — 上映", "M"),
    # The Wi-Fi onboarding form §9.5 mentions, so the encoder is known to handle
    # that escaping-heavy payload if the path is ever built.
    ("wifi_join", r"WIFI:T:WPA;S:CinemaPatron;P:a\;b\:c\\d;;", "M"),
]


def reference(data, ecc, version=None, mask=None):
    """python-qrcode's symbol: (rows, version, mask).

    With mask=None the oracle chooses; with a mask given, that mask is forced.
    """
    qr = qrcode.QRCode(
        version=version, error_correction=ECC_LEVELS[ecc], border=0, box_size=1, mask_pattern=mask
    )
    qr.add_data(QRData(data.encode("utf-8"), mode=MODE_8BIT_BYTE))
    qr.make(fit=version is None)
    rows = ["".join("1" if c else "0" for c in row) for row in qr.get_matrix()]
    # python-qrcode does not expose the chosen mask directly; read it back out of
    # the format information, which is where a scanner reads it too.
    return rows, qr.version, read_mask(rows)


def read_mask(rows):
    """Recover the mask number from the format information in the symbol.

    The 15-bit format field is (5 data bits << 10) | 10 BCH bits, XORed with
    0x5412. The five data bits are two of ECC level and three of mask, so the
    mask occupies FIELD bits 10-12 — not the low three bits, which belong to the
    BCH remainder. Reading the low bits instead yields a plausible number that is
    simply wrong, which is worth stating because it is what this function did
    first.

    In the copy of the format information beside the top-left finder, field bit i
    for i in 9..14 sits at (14 - i, 8).
    """
    field = 0
    for i in range(9, 15):
        if rows[8][14 - i] == "1":
            field |= 1 << i
    return ((field >> 10) & 0b111) ^ ((0x5412 >> 10) & 0b111)


def decode(rows, expect):
    """zxing-cpp must read the payload back out of the reference symbol."""
    n = len(rows)
    border, scale = 4, 6
    img = Image.new("1", ((n + 2 * border) * scale,) * 2, 1)
    px = img.load()
    for y, row in enumerate(rows):
        for x, ch in enumerate(row):
            if ch == "1":
                for dy in range(scale):
                    for dx in range(scale):
                        px[(x + border) * scale + dx, (y + border) * scale + dy] = 0
    res = zxingcpp.read_barcode(img)
    return res is not None and res.text == expect


def segno_agrees(data, ecc, rows):
    """Third opinion, recorded but not authoritative — see the module docstring."""
    try:
        sym = segno.make(data, error=ecc.lower(), micro=False, mode="byte")
    except Exception:
        return None
    other = ["".join("1" if m else "0" for m in row) for row in sym.matrix]
    return other == rows


def curated():
    out = []
    for name, data, ecc in CASES:
        chosen_rows, version, chosen_mask = reference(data, ecc)
        masks = []
        for m in range(8):
            rows, got_version, got_mask = reference(data, ecc, version=version, mask=m)
            if got_mask != m:
                sys.exit(f"{name}: asked the oracle for mask {m} and the symbol reports {got_mask}")
            if not decode(rows, data):
                sys.exit(f"{name}: zxing-cpp could not read mask {m} back — it is not ground truth")
            masks.append(
                {
                    "mask": m,
                    "sha256": hashlib.sha256("\n".join(rows).encode()).hexdigest(),
                    "decoded": True,
                    # Full matrices only for the small symbols, where a row-by-row
                    # diff in a failing test is readable rather than a wall.
                    "matrix": rows if len(rows) <= 33 else None,
                }
            )
        out.append(
            {
                "name": name,
                "data": data,
                "ecc": ecc,
                "version": version,
                "oracle_mask": chosen_mask,
                "size": len(chosen_rows),
                "masks": masks,
                "decoded_by": "zxing-cpp",
                "segno_agrees": segno_agrees(data, ecc, chosen_rows),
            }
        )
    return {
        "note": (
            "Generated by generate_vectors.py. Matrices from python-qrcode, EIGHT per "
            "case (one per forced data mask), each decoded by zxing-cpp before being "
            "committed. oracle_mask records which one python-qrcode would have chosen; "
            "it is informational, because implementations legitimately differ there "
            "(see the script docstring). segno_agrees is a third opinion; false means "
            "segno's known padding deviation, not a fault in the fixture. Do not "
            "hand-edit: a fixture edited to match our encoder is not evidence of "
            "anything."
        ),
        "cases": out,
    }


def exhaustive():
    """Every version x ECC level: capacity, mask, and a hash of the full matrix.

    The point is the TABLES. A transcription slip in the error-correction block
    counts breaks exactly one version/level pair and leaves the other 159
    perfect — a defect that survives casual testing forever. Hashes rather than
    matrices keep this to a few tens of kilobytes instead of megabytes.
    """
    out = []
    for version in range(1, 41):
        for ecc in ("L", "M", "Q", "H"):
            cap = capacity(version, ecc)
            payload = fill(cap)
            # One forced mask is enough here: the capacity and block tables this
            # set exists to check are mask-independent, and the curated set
            # already pins all eight.
            rows, got_version, mask = reference(payload, ecc, version=version, mask=0)
            if got_version != version:
                sys.exit(f"v{version}-{ecc}: oracle returned version {got_version}")
            if not decode(rows, payload):
                sys.exit(f"v{version}-{ecc}: zxing-cpp could not read the full-capacity symbol back")
            out.append(
                {
                    "version": version,
                    "ecc": ecc,
                    "byte_capacity": cap,
                    "mask": mask,
                    "size": len(rows),
                    "matrix_sha256": hashlib.sha256("\n".join(rows).encode()).hexdigest(),
                }
            )
    return {
        "note": (
            "Every version x ECC level at its exact byte-mode capacity. Matrices from "
            "python-qrcode, hashed; each one decoded by zxing-cpp before commit. The "
            "payload is deterministic (see fill() and the Go test's fill helper)."
        ),
        "cases": out,
    }


def capacity(version, ecc):
    """Largest byte-mode payload for this version/level, found by asking the oracle.

    Determined by bisection against python-qrcode rather than from a table, so
    the capacity in these fixtures is an independent fact rather than a copy of
    the table under test.
    """
    lo, hi = 0, 3000
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if fits(version, ecc, mid):
            lo = mid
        else:
            hi = mid - 1
    return lo


def fits(version, ecc, n):
    try:
        qr = qrcode.QRCode(version=version, error_correction=ECC_LEVELS[ecc], border=0, box_size=1)
        qr.add_data(QRData(fill(n).encode("utf-8"), mode=MODE_8BIT_BYTE))
        qr.make(fit=False)
        return True
    except Exception:
        return False


def fill(n):
    """A deterministic ASCII payload of exactly n bytes, mirrored in the Go test."""
    alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
    return "".join(alphabet[i % len(alphabet)] for i in range(n))


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "curated"
    doc = exhaustive() if mode == "exhaustive" else curated()
    json.dump(doc, sys.stdout, indent=1, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
