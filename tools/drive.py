#!/usr/bin/env python3
"""Drive a Bubble Tea program through a pseudo-terminal and print its frames.

    python3 drive.py [--env K=V]... [--cwd DIR] [--size CxR] [--wait TEXT]
                     [--raw FILE] [--attrs] BINARY 'SCRIPT'

SCRIPT is a Python list of (keys, seconds, label): the keys are written to
the pty, the program is given `seconds` to render, and the screen is printed
under `label`. Keys are raw bytes: "\\r" enter, "\\x1b" escape (send it and the
next key as two steps — "\\x1b?" in one write arrives as alt+?), "\\x03"
ctrl-c. Mouse: SGR sequences with 1-based coordinates — press "\\x1b[<0;X;YM",
drag "\\x1b[<32;X;YM", release "\\x1b[<0;X;Ym", wheel down "\\x1b[<65;X;YM".

Needs pyte (pip install pyte). Unix only. Example:

    .venv/bin/python tools/drive.py --env HOME=/tmp/h --env BFFS_HOME=/tmp/h/bffs \\
        --cwd /tmp/proj --size 120x30 --wait bffs ./bffs \\
        '[("",1.5,"at rest"),("3",0.5,"sessions"),("\\r",1.0,"open"),("\\x1b",0.5,"back")]'
"""
import argparse, codecs, fcntl, os, pty, select, struct, sys, termios, time

import pyte


class Screen(pyte.Screen):
    """pyte with the three sequences Bubble Tea's renderer uses that it lacks:
    CBT (ESC[Z), SU (ESC[S) and SD (ESC[T, which overlays are drawn with
    inside a scroll region). Without them a capture shows rule fragments at
    column 0, stale cells after paging and the first columns of a dismissed
    overlay lingering. The scrolls go through pyte's own index/reverse_index
    so margins hold."""

    def cursor_back_tab(self, count=None):
        for _ in range(count or 1):
            stops = sorted(s for s in self.tabstops if s < self.cursor.x)
            self.cursor.x = stops[-1] if stops else 0

    def _region(self):
        m = self.margins
        return (m.top, m.bottom) if m else (0, self.lines - 1)

    def scroll_up(self, count=None):
        top, bottom = self._region()
        y = self.cursor.y
        for _ in range(count or 1):
            self.cursor.y = bottom
            self.index()
        self.cursor.y = y

    def scroll_down(self, count=None):
        top, bottom = self._region()
        y = self.cursor.y
        for _ in range(count or 1):
            self.cursor.y = top
            self.reverse_index()
        self.cursor.y = y


pyte.Stream.csi["Z"] = "cursor_back_tab"
pyte.Stream.csi["S"] = "scroll_up"
pyte.Stream.csi["T"] = "scroll_down"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--env", action="append", default=[], help="KEY=VALUE for the child (repeatable)")
    ap.add_argument("--cwd", default=os.getcwd())
    ap.add_argument("--size", default="120x30", help="columns x rows")
    ap.add_argument("--wait", default="", help="text that marks the first frame; typing starts after it appears")
    ap.add_argument("--raw", help="save every byte the program wrote to this file (grep for \\x1b]8;; or \\x1b]52;)")
    ap.add_argument("--attrs", action="store_true", help="also print which cells carry reverse video or a background")
    ap.add_argument("binary")
    ap.add_argument("script")
    a = ap.parse_args()

    cols, rows = (int(x) for x in a.size.lower().split("x"))
    env = dict(os.environ)
    env["TERM"] = "xterm-256color"
    for kv in a.env:
        k, _, v = kv.partition("=")
        env[k] = v

    scr = Screen(cols, rows)
    st = pyte.Stream(scr)
    dec = codecs.getincrementaldecoder("utf-8")("replace")  # per-chunk decoding garbles a rune split across reads
    raw = []

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(a.cwd)
        os.execve(a.binary, [a.binary], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))

    def pump(seconds):
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.1)
            if not r:
                continue
            try:
                d = os.read(fd, 65536)
            except OSError:
                return
            if not d:
                return
            raw.append(d)
            st.feed(dec.decode(d))

    def wait_for(text, timeout=15):
        end = time.time() + timeout
        while time.time() < end:
            pump(0.2)
            if not text or any(text in l for l in scr.display):
                return
        print(f"!! never saw {text!r} in the first {timeout}s", file=sys.stderr)

    wait_for(a.wait)
    pump(1.0)

    for keys, seconds, label in eval(a.script):
        os.write(fd, keys.encode())
        pump(seconds)
        print("=" * cols)
        print(f"### {label} ({cols}x{rows})")
        for l in scr.display:
            print(l.rstrip())
        if a.attrs:
            for y in range(rows):
                row = scr.buffer[y]
                rev = [x for x in range(cols) if row[x].reverse]
                bg = [x for x in range(cols) if row[x].bg != "default"]
                if rev:
                    print(f"  row {y}: reverse {rev[0]}-{rev[-1]}: {''.join(row[x].data for x in range(rev[0], rev[-1] + 1))!r}")
                if bg and not rev:
                    print(f"  row {y}: background {bg[0]}-{bg[-1]}")

    os.write(fd, b"\x03")
    pump(0.4)
    if a.raw:
        with open(a.raw, "wb") as f:
            f.write(b"".join(raw))


if __name__ == "__main__":
    main()
