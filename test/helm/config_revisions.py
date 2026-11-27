#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Render the chart and check that the Deployment mounts the revision named by its exact bytes."""

import base64
import gzip
import hashlib
import json
import os
import subprocess
import tempfile
import threading
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import yaml


def render(*args):
    result = subprocess.run(
        [os.getenv("HELM", "helm"), "template", "test-release", "deploy/helm", "--namespace", "default", *args],
        text=True, capture_output=True, timeout=10,
    )
    if result.returncode:
        raise ValueError(result.stderr)
    return [obj for obj in yaml.safe_load_all(result.stdout) if obj]


def revision(objects):
    deployment, = (obj for obj in objects if obj["kind"] == "Deployment")
    template = deployment["spec"]["template"]
    name, = (vol["configMap"]["name"] for vol in template["spec"]["volumes"] if vol["name"] == "config")
    configmap, = (obj for obj in objects if obj["kind"] == "ConfigMap" and obj["metadata"]["name"] == name)
    if "config.yaml" in configmap.get("binaryData", {}):
        payload = base64.b64decode(configmap["binaryData"]["config.yaml"], validate=True)
    else:
        payload = configmap["data"]["config.yaml"].encode()
    return name, payload, configmap, template


@contextmanager
def api_fixture(tmp, maps, reads=None):
    """Serve the Kubernetes reads Helm makes at the local HTTP boundary."""
    path = "/api/v1/namespaces/default/configmaps"
    groups = {
        "v1": {"configmaps": "ConfigMap", "serviceaccounts": "ServiceAccount", "services": "Service", "pods": "Pod"},
        "apps/v1": {"deployments": "Deployment", "replicasets": "ReplicaSet"},
        "batch/v1": {"jobs": "Job"},
        "networking.k8s.io/v1": {"networkpolicies": "NetworkPolicy"},
        "policy/v1": {"poddisruptionbudgets": "PodDisruptionBudget"},
        "rbac.authorization.k8s.io/v1": {"roles": "Role", "rolebindings": "RoleBinding"},
    }
    responses = {
        "/version": {"gitVersion": "v1.32.0", "major": "1", "minor": "32"},
        "/api": {"kind": "APIVersions", "versions": ["v1"]},
        "/apis": {"kind": "APIGroupList", "groups": [
            {"name": group.split("/")[0], "versions": [{"version": "v1", "groupVersion": group}],
             "preferredVersion": {"version": "v1", "groupVersion": group}} for group in groups if group != "v1"
        ]},
        path: {"apiVersion": "v1", "kind": "ConfigMapList", "items": maps},
        **{f"{path}/{obj['metadata']['name']}": obj for obj in maps},
        **(reads or {}),
    }
    for group, resources in groups.items():
        prefix = "/api" if group == "v1" else "/apis"
        responses[f"{prefix}/{group}"] = {"kind": "APIResourceList", "groupVersion": group, "resources": [
            {"name": plural, "kind": kind, "namespaced": True} for plural, kind in resources.items()
        ]}


    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            result = responses.get(self.path.split("?", 1)[0])
            self.send_response(200 if result else 404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(result or {"apiVersion": "v1", "kind": "Status",
                                                  "status": "Failure", "reason": "NotFound", "code": 404}).encode())

        def log_message(self, *_):
            pass

    with HTTPServer(("127.0.0.1", 0), Handler) as server:
        config = Path(tmp, "kubeconfig")
        config.write_text(yaml.safe_dump({
            "apiVersion": "v1", "kind": "Config", "current-context": "test",
            "clusters": [{"name": "test", "cluster": {"server": f"http://127.0.0.1:{server.server_port}"}}],
            "contexts": [{"name": "test", "context": {"cluster": "test"}}],
        }))
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            yield config
        finally:
            server.shutdown()
            thread.join()


def upgrade(config, *args):
    return render("--is-upgrade", "--dry-run=server", "--disable-openapi-validation",
                  "--kubeconfig", str(config), "--kube-context", "test", *args)


def configmap(name, owner="test-release", component="config-revision"):
    return {
        "apiVersion": "v1", "kind": "ConfigMap", "data": {"config.yaml": "log: {}\n"},
        "metadata": {"name": name, "namespace": "default", "labels": {"app.kubernetes.io/component": component},
                     "annotations": {"helm.sh/resource-policy": "keep", "meta.helm.sh/release-name": owner,
                                     "meta.helm.sh/release-namespace": "default"}},
    }


class ConfigRevisions(unittest.TestCase):
    def assert_revision(self, *args):
        name, payload, configmap, template = revision(render(*args))
        digest = hashlib.sha256(payload).hexdigest()
        self.assertEqual(name, "genai-otel-bridge-config-" + digest)
        self.assertIs(configmap.get("immutable"), True)
        retention = configmap["metadata"]["annotations"]
        self.assertEqual(retention["helm.sh/resource-policy"], "keep")
        self.assertEqual(set(retention.get("argocd.argoproj.io/sync-options", "").split(",")),
                         {"Prune=false", "Delete=false"})
        annotations = template["metadata"].get("annotations", {})
        if "rolloutOnConfigChange=false" in args:
            self.assertNotIn("checksum/config", annotations)
        else:
            self.assertEqual(annotations["checksum/config"], digest)
        return name, payload, template

    def test_generated_config_change_changes_the_deployment_mount(self):
        args = ("--set", "rolloutOnConfigChange=false")
        name, _, _, _ = revision(render(*args))
        changed, _, _, _ = revision(render(*args, "--set", "config.log.format=json"))
        self.assertNotEqual(name, changed, "a config change must change the Deployment volume reference")
        self.assertEqual(name, self.assert_revision()[0])
        self.assertEqual(name, self.assert_revision(*args, "--set", "replicas=1")[0])
        self.assertEqual(changed, self.assert_revision(*args, "--set", "config.log.format=json")[0])

    def test_override_hashes_and_mounts_exact_bytes(self):
        payload = b'# caf\xc3\xa9\xc2\x85tenants: {team-a: {}}\r\nlog: {format: logfmt}\r\n\r\n'
        args = ("--set", "rollingUpdate.maxUnavailable=0")
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp, "config.yaml")
            path.write_bytes(payload)
            name, data, pod = self.assert_revision(*args, "--set-file", f"configOverride={path}")
            self.assertEqual(data, payload)
            self.assertIn("genai-otel-bridge-tenant-team-a", [
                v.get("secret", {}).get("secretName") for v in pod["spec"]["volumes"]
            ])
            changed = payload.replace(b"caf\xc3\xa9", b"different comment")
            path.write_bytes(changed)
            new_name, data, _ = self.assert_revision(*args, "--set-file", f"configOverride={path}")
            self.assertEqual(data, changed)
            self.assertNotEqual(name, new_name)

    def test_generated_tenant_config_excludes_legacy_keys(self):
        _, payload, _ = self.assert_revision("-f", "deploy/helm/examples/tenants-values.yaml")
        config = yaml.safe_load(payload)
        self.assertTrue(config["tenants"])
        self.assertNotIn("sources", config)
        self.assertNotIn("telemetry", config["emit"])

    def test_every_config_byte_is_hashed_and_mounted(self):
        # A name that ignores any byte of config.yaml (trimmed whitespace, a line ending, one
        # character), or a Deployment that mounts another name, would let an edited config
        # reuse a revision a running Pod already mounts.
        base = b"log: {format: logfmt}\n# a\n"
        variants = [base, base[:-1], base + b"\n", base.replace(b"\n", b"\r\n"), base.replace(b"# a", b"# b"),
                    b" " + base]
        names = set()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp, "config.yaml")
            for payload in variants:
                path.write_bytes(payload)
                name, data, _ = self.assert_revision("--set-file", f"configOverride={path}")
                self.assertEqual(data, payload)
                names.add(name)
        self.assertEqual(len(names), len(variants), "two different configs share a revision name")

    def test_migration_keeps_the_fixed_name_map_without_rewriting_its_config(self):
        legacy = configmap("genai-otel-bridge-config", component="legacy")
        del legacy["metadata"]["annotations"]["helm.sh/resource-policy"]
        legacy["data"]["config.yaml"] = "# legacy\u0085log: {}\r\n\n"
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, [legacy]) as config:
            objects = upgrade(config)
        saved, = (obj for obj in objects if obj["metadata"]["name"] == legacy["metadata"]["name"])
        self.assertEqual(saved["data"], legacy["data"])
        self.assertEqual(saved["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotEqual(revision(objects)[0], saved["metadata"]["name"])

    def test_first_upgrade_with_reused_legacy_values_keeps_the_fixed_name_map(self):
        # A deployed Helm revision from before config revisions, upgraded through Helm's real
        # upgrade path (not `template --is-upgrade`) with the stored values reused.
        chart = Path("deploy/helm")
        values = yaml.safe_load((chart / "values.yaml").read_text())
        legacy = configmap("genai-otel-bridge-config", component="legacy")
        del legacy["metadata"]["annotations"]["helm.sh/resource-policy"]
        release = {
            "name": "test-release", "namespace": "default", "version": 1,
            "info": {"status": "deployed"}, "config": {}, "manifest": yaml.safe_dump(legacy),
            "chart": {"metadata": yaml.safe_load((chart / "Chart.yaml").read_text()), "values": values},
        }
        history = {
            "apiVersion": "v1", "kind": "Secret", "type": "helm.sh/release.v1",
            "metadata": {"name": "sh.helm.release.v1.test-release.v1", "namespace": "default",
                         "labels": {"name": "test-release", "owner": "helm", "status": "deployed", "version": "1"}},
            "data": {"release": base64.b64encode(base64.b64encode(gzip.compress(json.dumps(release).encode()))).decode()},
        }
        reads = {"/api/v1/namespaces/default/secrets": {"apiVersion": "v1", "kind": "SecretList", "items": [history]}}
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, [legacy], reads) as config:
            command = [os.getenv("HELM", "helm"), "upgrade", "test-release", str(chart), "--namespace", "default",
                       "--kubeconfig", str(config), "--dry-run=server", "--disable-openapi-validation", "--output", "json",
                       "--reuse-values"]
            result = subprocess.run(command, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        upgraded = json.loads(result.stdout)
        objects = [obj for obj in yaml.safe_load_all(upgraded["manifest"]) if obj]
        saved, = (obj for obj in objects if obj["kind"] == "ConfigMap" and obj["metadata"]["name"] == legacy["metadata"]["name"])
        self.assertEqual(saved["data"], legacy["data"])
        self.assertEqual(saved["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotEqual(revision(objects)[0], saved["metadata"]["name"])



if __name__ == "__main__":
    unittest.main()
