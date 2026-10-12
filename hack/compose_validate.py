#!/usr/bin/env python3
"""Structural validation of docker-compose.yml using only the stdlib.

Used by `make pkg-compose` when neither `docker compose` nor `docker-compose`
is installed on the host. Does a shallow structural check — not a full YAML
parse — so it catches shape errors (missing services, malformed ports,
bad depends_on) but not every schema nuance that `docker compose config`
would.
"""
import re
import sys

PATH = "docker-compose.yml"


def fail(msg):
    print(f"COMPOSE VALIDATION FAILED: {msg}", file=sys.stderr)
    sys.exit(1)


def main():
    try:
        with open(PATH) as fh:
            raw = fh.read()
    except OSError as exc:
        fail(f"cannot read {PATH}: {exc}")

    if not raw.strip():
        fail("file is empty")

    if not re.search(r"(?m)^services:\s*$", raw):
        fail("missing top-level 'services:' key")

    # Split into service blocks: each block starts at a 2-space key line and
    # runs until the next 2-space key line or EOF.
    services = {}
    for m in re.finditer(r"(?m)^  ([A-Za-z0-9_.-]+):\s*\n", raw):
        name = m.group(1)
        if name in ("services", "version"):
            continue
        start = m.start()
        nxt = re.search(r"(?m)^  [A-Za-z0-9_.-]+:\s*\n", raw[m.end():])
        end = m.end() + nxt.start() if nxt else len(raw)
        services[name] = raw[start:end]

    if not services:
        fail("no service definitions found under services:")

    for name, block in services.items():
        if not re.search(r"(?ms)^\s+image:\s*\S", block):
            fail(f"service '{name}' has no 'image:'")
        # ports entries must be list items ("- ...") indented under ports:
        ports = re.search(r"(?m)^\s+ports:\s*\n((?:[ \t]+- [^\n]*\n)+)", block)
        if ports:
            for line in ports.group(1).splitlines():
                if not re.match(r"[ \t]+- ", line):
                    fail(f"service '{name}': malformed ports entry: {line!r}")
        # depends_on entries must reference declared services.
        dep = re.search(r"(?m)^\s+depends_on:\s*\n((?:[ \t]+- [^\n]*\n)+)", block)
        if dep:
            for line in dep.group(1).splitlines():
                ref = re.sub(r"\s+-\s+", "", line).strip()
                if ref and ref not in services:
                    fail(f"service '{name}' depends_on unknown service '{ref}'")

    expected = {"router", "worker"}
    missing = expected - set(services)
    if missing:
        fail(f"missing expected services: {sorted(missing)}")

    print(f"YAML structural validation OK — services: {sorted(services)}")


if __name__ == "__main__":
    main()