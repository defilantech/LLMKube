#!/usr/bin/env python3
"""Regression tests for scripts/validate-samples.py helpers.

Kept alongside the CRD test fixtures so the validate-samples gate has a
deterministic unit surface — the script itself is integration-only (it
touches the filesystem).
"""

import importlib.util
import sys
from pathlib import Path

# Load the script as a module without installing it.
_script = Path(__file__).resolve().parent.parent.parent / "scripts" / "validate-samples.py"
_spec = importlib.util.spec_from_file_location("validate_samples", _script)
_validate_samples = importlib.util.module_from_spec(_spec)
sys.modules["validate_samples"] = _validate_samples
_spec.loader.exec_module(_validate_samples)
add_additional_properties = _validate_samples.add_additional_properties


def test_add_additional_properties_skips_preserve_unknown_fields():
    """A node with x-kubernetes-preserve-unknown-fields must NOT get
    additionalProperties: false forced on it (#1085).
    """
    schema = {
        "type": "object",
        "properties": {
            "opaque": {
                "type": "object",
                "x-kubernetes-preserve-unknown-fields": True,
            },
            "strict": {
                "type": "object",
                "properties": {"name": {"type": "string"}},
            },
        },
    }
    add_additional_properties(schema)

    assert "additionalProperties" not in schema["properties"]["opaque"], (
        "preserve-unknown-fields node must keep its additionalProperties unset"
    )
    assert schema["properties"]["strict"].get("additionalProperties") is False, (
        "plain object node must get additionalProperties: false"
    )


def test_add_additional_properties_still_forces_on_plain_objects():
    """Plain object nodes still get additionalProperties: false forced."""
    schema = {"type": "object", "properties": {"foo": {"type": "string"}}}
    add_additional_properties(schema)
    assert schema.get("additionalProperties") is False


def test_add_additional_properties_does_not_override_existing():
    """If a node already declares additionalProperties, leave it alone."""
    schema = {
        "type": "object",
        "additionalProperties": True,
        "properties": {"x": {"type": "integer"}},
    }
    add_additional_properties(schema)
    assert schema["additionalProperties"] is True


if __name__ == "__main__":
    test_add_additional_properties_skips_preserve_unknown_fields()
    test_add_additional_properties_still_forces_on_plain_objects()
    test_add_additional_properties_does_not_override_existing()
    print("OK")
