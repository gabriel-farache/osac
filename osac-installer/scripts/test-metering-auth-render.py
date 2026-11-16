#!/usr/bin/env python3
"""Check metering event-log wiring in every metering profile."""

from __future__ import annotations

import subprocess

from _render import PROFILES, render, select_one, template_command


def check_profile(profile: str) -> None:
    docs = render(profile)
    metering = select_one(
        docs,
        lambda doc: "# Source: osac/charts/metering/templates/deployment.yaml\n" in doc,
        f"metering Deployment for {profile}",
    )
    rules = select_one(
        docs,
        lambda doc: "kind: ConfigMap\n" in doc
        and "  name: osac-metering-rules-0001\n" in doc,
        f"metering rules ConfigMap for {profile}",
    )
    policy = select_one(
        docs,
        lambda doc: "kind: ValidatingAdmissionPolicy\n" in doc
        and "  name: osac-metering-protect-rules\n" in doc,
        f"rules admission policy for {profile}",
    )
    binding = select_one(
        docs,
        lambda doc: "kind: ValidatingAdmissionPolicyBinding\n" in doc
        and "  name: osac-metering-protect-rules\n" in doc,
        f"rules admission policy binding for {profile}",
    )

    # Only the producer receives the dedicated attribution token; ordinary
    # pod/adapter tokens keep their existing API audience and permissions.
    assert "METERING_DEPLOYMENT_ID" in metering, profile
    assert "HEARTBEAT_INTERVAL" in metering, profile
    assert "KAFKA_BROKERS" in metering, profile
    assert "FULFILLMENT_ADDRESS" in metering, profile
    assert 'value: "fulfillment-internal-api:8001"' in metering, profile
    assert "FULFILLMENT_CA_FILE" in metering, profile
    assert "FULFILLMENT_TOKEN_FILE" in metering, profile
    assert "audience: osac-metering-attribution" in metering, profile
    assert "expirationSeconds: 600" in metering, profile
    assert "serviceAccountName: osac-metering" in metering, profile
    assert "mountPath: /etc/metering/attribution" in metering, profile
    init = metering.split("initContainers:", 1)[1].split("\n      containers:", 1)[0]
    assert "attribution-token" not in init, profile
    assert "fulfillment-credentials" not in metering, profile
    grpc = select_one(
        docs,
        lambda doc: "kind: Deployment\n" in doc and "  name: fulfillment-grpc-server\n" in doc,
        f"fulfillment gRPC Deployment for {profile}",
    )
    assert "--metering-service-account=system:serviceaccount:default:osac-metering" in grpc, profile
    for doc in docs:
        if "kind: Deployment\n" in doc and "adapter-deployment.yaml\n" in doc:
            assert "attribution-token" not in doc, profile

    # The default meters ship as revision 0001 and are protected.
    assert 'effective_at: "1970-01-01T00:00:00Z"' in rules, profile
    assert "meters.yaml:" in rules, profile
    assert "resource: ComputeInstance" in rules, profile
    assert "immutable: true" in rules, profile
    assert "osac-metering-rules-" in policy, profile
    assert "impossible" not in policy, profile
    assert "validationActions:" in binding, profile
    assert 'validationActions: ["Deny"]' in binding, profile

    # Billing history survives a release uninstall: the ConfigMap, the policy,
    # and the binding are all kept. Removing them is always manual.
    keep = '"helm.sh/resource-policy": keep'
    assert keep in rules, f"{profile}: rules ConfigMap must be kept on uninstall"
    assert keep in policy, f"{profile}: rules policy must be kept on uninstall"
    assert keep in binding, f"{profile}: rules binding must be kept on uninstall"


def check_adapter_isolation() -> None:
    command = [
        *template_command("full-ci"),
        "--set", "metering.echoAdapter.enabled=true",
        "--set", "metering.m360Adapter.enabled=true",
        "--set", "metering.m360Adapter.m360.apiUrl=https://provider.example.test",
        "--set", "metering.m360Adapter.m360.apiKeySecret=existing-provider-key",
    ]
    docs = subprocess.run(command, check=True, capture_output=True, text=True, timeout=300).stdout.split("\n---\n")
    for adapter in ("echo", "m360"):
        doc = select_one(docs, lambda doc, adapter=adapter: f"/{adapter}-adapter-deployment.yaml\n" in doc,
                         f"{adapter} adapter")
        assert "serviceAccountName: osac-metering" in doc
        assert "attribution-token" not in doc
        assert "FULFILLMENT_" not in doc
    for doc in docs:
        if "kind: Role\n" in doc and "osac-metering" in doc:
            assert "serviceaccounts/token" not in doc
            assert 'resources: ["pods"]' not in doc


if __name__ == "__main__":
    for profile in PROFILES:
        check_profile(profile)
    check_adapter_isolation()
    print("Metering event-log profile render checks passed.")
