#!/usr/bin/env python3
"""Writes assets/architecture-dark.svg and assets/architecture-light.svg.

Edit the content lists below, then run: python3 assets/build_diagram.py
Both themes come from one layout so they never drift apart.
"""
from pathlib import Path
from html import escape

THEMES = {
    "dark": dict(bg="#0a0a0a", card="#111111", line="#2e2e2e", fg="#ededed",
                 dim="#a1a1a1", faint="#666666", accent="#ededed"),
    "light": dict(bg="#ffffff", card="#fafafa", line="#d4d4d4", fg="#0a0a0a",
                  dim="#525252", faint="#8f8f8f", accent="#0a0a0a"),
}

DRIVERS = ["Claude Code", "Codex", "Devin", "you, in a terminal"]
SERVICES = [
    ("start", "plain task → plan", "→ run → report"),
    ("workflows", "parallel workers,", "gates, resumable runs"),
    ("loops", "bounded cycles that", "stop on a named state"),
    ("agent teams", "peers sharing a task", "board and a mailbox"),
    ("repo intelligence", "symbols, risk,", "co-change, knowledge"),
    ("sessions", "recall what past", "agents did and decided"),
]
KERNEL = ["SQLite state", "worktrees + patches", "verification gates", "budgets + leases", "run journal"]
PROVIDERS = ["Claude", "Codex", "Devin", "any CLI behind a small wrapper"]

W, H = 960, 600
SANS = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"


def text(x, y, s, size, fill, weight=400, anchor="middle", family=SANS, spacing=0):
    return (f'<text x="{x}" y="{y}" font-family="{family}" font-size="{size}" font-weight="{weight}" '
            f'fill="{fill}" text-anchor="{anchor}" letter-spacing="{spacing}">{escape(s)}</text>')


def rect(x, y, w, h, c, r=10, fill=None, dash=False):
    d = ' stroke-dasharray="4 4"' if dash else ""
    return (f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{fill or c["card"]}" '
            f'stroke="{c["line"]}" stroke-width="1"{d}/>')


def chip_row(items, y, c, family=SANS):
    """Centered row of outlined chips; width estimated from character count."""
    widths = [len(s) * 7.8 + 30 for s in items]
    gap = 10
    x = (W - sum(widths) - gap * (len(items) - 1)) / 2
    out = []
    for s, w in zip(items, widths):
        out.append(f'<rect x="{x:.1f}" y="{y}" width="{w:.1f}" height="30" rx="15" fill="none" stroke="{c["line"]}"/>')
        out.append(text(x + w / 2, y + 20, s, 13, c["dim"], family=family))
        x += w + gap
    return out


def arrow(y1, y2, c):
    x = W / 2
    return (f'<line x1="{x}" y1="{y1}" x2="{x}" y2="{y2 - 6}" stroke="{c["faint"]}" stroke-width="1.2"/>'
            f'<path d="M{x - 4} {y2 - 7} L{x} {y2} L{x + 4} {y2 - 7}" fill="none" stroke="{c["faint"]}" stroke-width="1.2"/>')


def label(y, s, c):
    return text(40, y, s.upper(), 11, c["faint"], 600, "start", spacing=1.2)


def build(c):
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img" '
         f'aria-label="Pallium architecture: any agent drives pallium route, which picks one of six services '
         f'built on one kernel; any provider does the work">',
         f'<rect width="{W}" height="{H}" rx="14" fill="{c["bg"]}"/>']

    o.append(label(46, "any agent drives it", c))
    o += chip_row(DRIVERS, 60, c)
    o.append(arrow(96, 124, c))

    # route
    o.append(rect(300, 124, 360, 62, c, 12))
    o.append(text(W / 2, 151, 'pallium route "<task>"', 16, c["fg"], 600, family=MONO))
    o.append(text(W / 2, 173, "picks the right service, inside the authority you grant", 12.5, c["dim"]))
    o.append(arrow(186, 214, c))

    # services
    o.append(label(208, "services", c))
    gap, x0, y0 = 12, 40, 216
    cw = (W - 2 * x0 - gap * (len(SERVICES) - 1)) / len(SERVICES)
    for i, (name, a, b) in enumerate(SERVICES):
        x = x0 + i * (cw + gap)
        o.append(rect(f"{x:.1f}", y0, f"{cw:.1f}", 96, c))
        o.append(text(f"{x + cw / 2:.1f}", y0 + 34, name, 14, c["fg"], 600))
        o.append(text(f"{x + cw / 2:.1f}", y0 + 58, a, 11.5, c["dim"]))
        o.append(text(f"{x + cw / 2:.1f}", y0 + 75, b, 11.5, c["dim"]))

    # kernel
    o.append(label(354, "kernel · one binary, one local database", c))
    o.append(rect(40, 366, W - 80, 92, c, 12))
    o.append(text(W / 2, 396, "durable state lives here, so work survives a crash, a restart, or the end of a chat",
                  13, c["fg"], 500))
    o += chip_row(KERNEL, 412, c)
    o.append(arrow(458, 498, c))

    # providers
    o.append(label(492, "any model does the work", c))
    o += chip_row(PROVIDERS, 504, c)
    o.append(text(W / 2, 572, "the agent steering Pallium and the agents doing the work can be different models",
                  12, c["faint"]))
    o.append("</svg>")
    return "\n".join(o)


here = Path(__file__).parent
for name, colors in THEMES.items():
    (here / f"architecture-{name}.svg").write_text(build(colors) + "\n")
    print(f"wrote assets/architecture-{name}.svg")
