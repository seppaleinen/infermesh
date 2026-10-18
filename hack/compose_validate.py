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

# Validate every compose file in the repo. `docker-compose.build.yml` uses the
# same service shapes as `docker-compose.yml` (just different build stanzas),
# so the same structural checks apply.
PATHS = ["docker-compose.yml", "docker-compose.build.yml"]


def fail(msg):
    print(f"COMPOSE VALIDATION FAILED: {msg}", file=sys.stderr)
    sys.exit(1)


def validate(path):
    try:
        with open(path) as fh:
            raw = fh.read()
    except OSError as exc:
        fail(f"cannot read {path}: {exc}")

    if not raw.strip():
        fail(f"{path}: file is empty")

    if not re.search(r"(?m)^services:\s*$", raw):
        fail(f"{path}: missing top-level 'services:' key")

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
        fail(f"{path}: no service definitions found under services:")

    for name, block in services.items():
        # image: OR build: is required (a registry pull or a local build
        # stanza — the structural check just needs SOMETHING to resolve the
        # image). Build-only services (docker-compose.build.yml) omit image:.
        if not re.search(r"(?ms)^\s+image:\s*\S", block) and \
           not re.search(r"(?ms)^\s+build:\s*\n", block):
            fail(f"{path}: service '{name}' has no 'image:' or 'build:'")
        # ports entries must be list items ("- ...") indented under ports:
        ports = re.search(r"(?m)^\s+ports:\s*\n((?:[ \t]+- [^\n]*\n)+)", block)
        if ports:
            for line in ports.group(1).splitlines():
                if not re.match(r"[ \t]+- ", line):
                    fail(f"{path}: service '{name}': malformed ports entry: {line!r}")
        # depends_on entries must reference declared services.
        dep = re.search(r"(?m)^\s+depends_on:\s*\n((?:[ \t]+- [^\n]*\n)+)", block)
        if dep:
            for line in dep.group(1).splitlines():
                ref = re.sub(r"\s+-\s+", "", line).strip()
                if ref and ref not in services:
                    fail(f"{path}: service '{name}' depends_on unknown service '{ref}'")

    expected = {"router", "worker"}
    missing = expected - set(services)
    if missing:
        fail(f"{path}: missing expected services: {sorted(missing)}")

    print(f"{path}: YAML structural validation OK — services: {sorted(services)}")


def main():
    for path in PATHS:
        validate(path)


if __name__ == "__main__":
    main()