#!/usr/bin/env python3
"""Validate config/samples against CRD schemas.

Extracts JSON schemas from config/crd/bases/*.yaml and validates
config/samples/**/*.yaml against them using jsonschema.

Exit codes:
  0 - all samples valid
  1 - validation errors found
  2 - script error (missing deps, etc.)
"""

import glob
import json
import sys
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator, ValidationError


def add_additional_properties(schema):
    """Recursively add additionalProperties: false to object schemas.
    
    Special case: metadata objects allow arbitrary additional properties
    (labels, annotations, etc.) since they're standard K8s fields.
    """
    if isinstance(schema, dict):
        if schema.get("type") == "object" and "additionalProperties" not in schema:
            # Skip nodes marked to hold opaque JSON — forcing
            # additionalProperties: false would reject legitimate
            # preserve-unknown-fields payloads (#1085, follow-up to #1021).
            if not schema.get("_is_metadata") and not schema.get("x-kubernetes-preserve-unknown-fields"):
                schema["additionalProperties"] = False
        for v in schema.values():
            add_additional_properties(v)
    elif isinstance(schema, list):
        for item in schema:
            add_additional_properties(item)
    return schema


def extract_schemas(crd_dir: str) -> dict:
    """Extract JSON schemas from CRD YAML files."""
    schemas = {}
    for crd_file in sorted(glob.glob(f"{crd_dir}/*.yaml")):
        with open(crd_file) as f:
            crd = yaml.safe_load(f)
        if crd.get("kind") != "CustomResourceDefinition":
            continue
        group = crd["spec"]["group"]
        for version in crd["spec"]["versions"]:
            if not version.get("schema", {}).get("openAPIV3Schema"):
                continue
            schema = version["schema"]["openAPIV3Schema"]
            # Mark metadata objects specially
            if "properties" in schema and "metadata" in schema["properties"]:
                meta = schema["properties"]["metadata"]
                if meta.get("type") == "object":
                    meta["_is_metadata"] = True
            schema = add_additional_properties(schema)
            # Clean up the marker
            if "properties" in schema and "metadata" in schema["properties"]:
                meta = schema["properties"]["metadata"]
                meta.pop("_is_metadata", None)
            kind = crd["spec"]["names"]["kind"]
            # Store by kind (last one wins if duplicate, shouldn't happen)
            schemas[kind] = schema
    return schemas


# Standard K8s kinds that should be skipped (not LLMKube CRDs)
STANDARD_KINDS = {
    "Secret", "Service", "ConfigMap", "Deployment", "StatefulSet", "DaemonSet",
    "Pod", "Namespace", "ServiceAccount", "Role", "RoleBinding", "ClusterRole",
    "ClusterRoleBinding", "PersistentVolumeClaim", "Ingress", "HorizontalPodAutoscaler",
    "NetworkPolicy", "PodDisruptionBudget", "PriorityClass", "StorageClass",
    "CustomResourceDefinition", "APIService", "ValidatingWebhookConfiguration",
    "MutatingWebhookConfiguration", "Certificate", "Issuer", "CertificateRequest",
    "Order", "Challenge"
}

# Third-party kinds that might appear in samples (skip validation)
THIRD_PARTY_KINDS = {
    "HTTPScaledObject", "KnativeService", "KnativeConfiguration",
    "KnativeRevision", "KnativeRoute", "KnativeDomainMapping"
}

def validate_sample(sample_path: str, schemas: dict) -> list:
    """Validate a single sample file against its CRD schema."""
    with open(sample_path) as f:
        # Handle multi-document YAML
        docs = list(yaml.safe_load_all(f))

    errors = []
    for doc in docs:
        if doc is None:
            continue
        kind = doc.get("kind")
        if not kind:
            # Skip non-resource YAML (kustomization, etc.)
            continue
        if kind in STANDARD_KINDS or kind in THIRD_PARTY_KINDS:
            # Skip standard K8s and third-party resources
            continue
        if kind not in schemas:
            errors.append(f"Unknown kind: {kind}")
            continue

        schema = schemas[kind]
        validator = Draft202012Validator(schema)
        for error in sorted(validator.iter_errors(doc), key=lambda e: list(e.path)):
            path = ".".join(str(p) for p in error.path) or "(root)"
            errors.append(f"{path}: {error.message}")
    return errors


def main():
    repo_root = Path(__file__).parent.parent
    crd_dir = str(repo_root / "config" / "crd" / "bases")
    samples_dir = str(repo_root / "config" / "samples")

    # Extract schemas
    schemas = extract_schemas(crd_dir)
    if not schemas:
        print("ERROR: No CRD schemas found in config/crd/bases/", file=sys.stderr)
        return 2

    print(f"Loaded {len(schemas)} CRD schema(s): {', '.join(sorted(schemas.keys()))}")

    # Find all sample files
    sample_files = sorted(glob.glob(f"{samples_dir}/**/*.yaml", recursive=True))
    if not sample_files:
        print("WARNING: No sample files found in config/samples/", file=sys.stderr)
        return 0

    print(f"Validating {len(sample_files)} sample file(s)...")

    all_errors = []
    for sample_path in sample_files:
        errors = validate_sample(sample_path, schemas)
        if errors:
            all_errors.append((sample_path, errors))
            print(f"\n❌ {sample_path}:")
            for err in errors:
                print(f"   - {err}")

    if all_errors:
        print(f"\n{'='*60}")
        print(f"FAILED: {len(all_errors)} file(s) with validation errors")
        total_errors = sum(len(errs) for _, errs in all_errors)
        print(f"Total: {total_errors} error(s)")
        return 1
    else:
        print(f"\n✅ All {len(sample_files)} sample(s) valid!")
        return 0


if __name__ == "__main__":
    sys.exit(main())
