#!/usr/bin/env python3
"""Verify one exact Service delta while preserving every non-target Serve key."""

import json
import sys


def reject_funnel(value: object) -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if "funnel" in key.lower() and child not in (False, None, "", [], {}):
                raise AssertionError("Funnel is present")
            reject_funnel(child)
    elif isinstance(value, list):
        for child in value:
            reject_funnel(child)


def main() -> int:
    if len(sys.argv) != 6:
        return 2
    before_path, after_path, service, host, target = sys.argv[1:]
    with open(before_path, encoding="utf-8") as handle:
        before = json.load(handle)
    with open(after_path, encoding="utf-8") as handle:
        after = json.load(handle)
    assert isinstance(before, dict) and isinstance(after, dict)
    before_services = dict(before.get("Services") or {})
    after_services = dict(after.get("Services") or {})
    assert service not in before_services, "target Service already has local config"
    target_config = after_services.pop(service)
    assert after_services == before_services, "another Service changed"
    expected = {
        "TCP": {"80": {"HTTP": True}, "443": {"HTTPS": True}},
        "Web": {
            f"{host}:80": {"Handlers": {"/": {"Proxy": target}}},
            f"{host}:443": {"Handlers": {"/": {"Proxy": target}}},
        },
    }
    assert target_config == expected, "target Service config is not exact"
    before_node = {key: value for key, value in before.items() if key != "Services"}
    after_node = {key: value for key, value in after.items() if key != "Services"}
    assert after_node == before_node, "Node Serve state changed"
    reject_funnel(after)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (AssertionError, KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        print(f"invalid service config: {error}", file=sys.stderr)
        sys.exit(1)
