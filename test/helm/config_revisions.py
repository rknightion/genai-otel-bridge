#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Render the chart and check the mounted revision and pruning permissions."""

import base64
import gzip
import hashlib
import json
import os
import shlex
import shutil
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


def map_delete_rules(objects):
    return [rule for obj in objects if obj["kind"] in ("Role", "ClusterRole")
            for rule in obj["rules"] if {"configmaps", "*"}.intersection(rule["resources"])
            and {"delete", "deletecollection", "*"}.intersection(rule["verbs"])]


@contextmanager
def api_fixture(tmp, maps, workloads=None):
    """Serve the Kubernetes reads and record deletes at the local HTTP boundary."""
    path = "/api/v1/namespaces/default/configmaps"
    deletes = {}
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
        **(workloads or {}),
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

        def do_DELETE(self):
            target = self.path.split("?", 1)[0]
            # Raw kubectl reads DeleteOptions from stdin, which Go sends chunked.
            if self.headers.get("Transfer-Encoding") == "chunked":
                body = bytearray()
                while size := int(self.rfile.readline(), 16):
                    body.extend(self.rfile.read(size))
                    self.rfile.read(2)  # CRLF
                self.rfile.readline()
            else:
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            deletes[target.removeprefix(path + "/")] = json.loads(body)
            self.send_response(200)
            self.end_headers()

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
            yield config, deletes
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

    def test_prune_is_default_off_even_with_discovered_history(self):
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, [configmap(self.hash_name("1"))]) as (config, _):
            objects = upgrade(config)
        self.assertFalse(any(obj["metadata"]["name"] == "genai-otel-bridge-config-revisions" for obj in objects))
        # The pre-existing uninstall hook may only delete the checkpoint ConfigMap.
        for rule in map_delete_rules(objects):
            self.assertEqual(rule.get("resourceNames"), ["genai-otel-bridge-checkpoints"])
            self.assertEqual(rule["verbs"], ["delete"])

    @staticmethod
    def hash_name(digit):
        return "genai-otel-bridge-config-" + digit * 64

    def test_enabled_prune_grant_is_limited_to_discovered_old_owned_names(self):
        desired, _, _, _ = revision(render())
        old = self.hash_name("1")
        legacy = "genai-otel-bridge-config"
        candidates = [configmap(desired), configmap(old), configmap(legacy),
                      configmap(self.hash_name("2"), owner="another-release"),
                      configmap(self.hash_name("3"), component="other"), configmap("unrelated")]
        args = ("--set", "configRevisionPruning.enabled=true,configRevisionPruning.historyLimit=1")
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, candidates) as (config, _):
            objects = upgrade(config, *args)
        hook = [obj for obj in objects if obj["metadata"]["name"] == "genai-otel-bridge-config-revisions"]
        self.assertEqual({obj["kind"] for obj in hook}, {"Job", "ServiceAccount", "Role", "RoleBinding"})
        self.assertTrue(all(obj["metadata"]["annotations"]["helm.sh/hook"] == "post-upgrade" for obj in hook))
        self.assertEqual(map_delete_rules(hook), [
            {"apiGroups": [""], "resources": ["configmaps"], "resourceNames": [legacy, old], "verbs": ["get", "delete"]},
        ])
        role, = (obj for obj in hook if obj["kind"] == "Role")
        for rule in role["rules"]:
            self.assertTrue(set(rule["resources"]) <= {"configmaps", "replicasets", "pods", "deployments"})
        job, = (obj for obj in hook if obj["kind"] == "Job")
        env = {item["name"]: item.get("value") for item in job["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["REVISION_NAMES"].split(), [legacy, old])
        self.assertEqual(env["KEEP_UNUSED"], "1")
        # No old revisions are discoverable without a live Helm connection, or on initial install.
        for mode in ((), ("--is-upgrade",)):
            self.assertFalse(any(obj["metadata"]["name"] == "genai-otel-bridge-config-revisions"
                                 for obj in render(*mode, *args)))

    def test_migration_keeps_the_fixed_name_map_without_rewriting_its_config(self):
        legacy = configmap("genai-otel-bridge-config", component="legacy")
        del legacy["metadata"]["annotations"]["helm.sh/resource-policy"]
        legacy["data"]["config.yaml"] = "# legacy\u0085log: {}\r\n\n"
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, [legacy]) as (config, _):
            objects = upgrade(config)
        saved, = (obj for obj in objects if obj["metadata"]["name"] == legacy["metadata"]["name"])
        self.assertEqual(saved["data"], legacy["data"])
        self.assertEqual(saved["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotEqual(revision(objects)[0], saved["metadata"]["name"])

    def test_first_upgrade_with_reused_legacy_values_keeps_pruning_off(self):
        # A deployed Helm revision from before this option existed. Helm's upgrade path
        # (not `template --is-upgrade`) reuses the stored, older chart defaults.
        chart = Path("deploy/helm")
        values = yaml.safe_load((chart / "values.yaml").read_text())
        values.pop("configRevisionPruning", None)
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
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, [legacy], reads) as (config, _):
            result = subprocess.run([
                os.getenv("HELM", "helm"), "upgrade", "test-release", str(chart), "--namespace", "default",
                "--reuse-values", "--kubeconfig", str(config), "--dry-run=server", "--disable-openapi-validation",
                "--output", "json",
            ], text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        upgraded = json.loads(result.stdout)
        objects = [obj for obj in yaml.safe_load_all(upgraded["manifest"]) if obj]
        saved, = (obj for obj in objects if obj["kind"] == "ConfigMap" and obj["metadata"]["name"] == legacy["metadata"]["name"])
        self.assertEqual(saved["data"], legacy["data"])
        self.assertEqual(saved["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertFalse(any(hook["name"] == "genai-otel-bridge-config-revisions" for hook in upgraded.get("hooks", [])))

    def test_prune_job_keeps_referenced_and_newest_unused_revisions(self):
        old, recent, in_rs, in_pod, in_deploy = (self.hash_name(str(n)) for n in range(1, 6))
        maps = [configmap(name) for name in (old, recent, in_rs, in_pod, in_deploy)]
        for day, obj in enumerate(maps, 1):
            obj["metadata"].update(creationTimestamp=f"2020-01-0{day}T00:00:00Z", uid=f"uid-{day}", resourceVersion=str(day))
        workloads = {
            "/apis/apps/v1/namespaces/default/deployments/genai-otel-bridge": {
                "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "genai-otel-bridge"},
                "spec": {"template": {"spec": {"volumes": [{"configMap": {"name": in_deploy}}]}}},
            },
            "/apis/apps/v1/namespaces/default/replicasets": {"apiVersion": "apps/v1", "kind": "ReplicaSetList", "items": [
                {"spec": {"replicas": 0, "template": {"spec": {
                    "initContainers": [{"env": [{"valueFrom": {"configMapKeyRef": {"name": in_rs}}}]}],
                }}}},
            ]},
            "/api/v1/namespaces/default/pods": {"apiVersion": "v1", "kind": "PodList", "items": [
                {"spec": {"ephemeralContainers": [{"envFrom": [{"configMapRef": {"name": in_pod}}]}]}},
            ]},
        }
        kubectl = shlex.quote(os.path.abspath(shutil.which(os.getenv("KUBECTL", "kubectl"))))
        with tempfile.TemporaryDirectory() as tmp, api_fixture(tmp, maps, workloads) as (config, deletes):
            # Use real kubectl requests and Go output templates. Replace only in-cluster
            # credential setup with the fixture's scratch kubeconfig (no cluster or token).
            wrapper = Path(tmp, "kubectl")
            wrapper.write_text("#!/bin/sh\n[ \"$1\" != config ] || exit 0\nshift 2\n"
                               f"exec {kubectl} --kubeconfig={shlex.quote(str(config))} \"$@\"\n")
            wrapper.chmod(0o700)
            for limit, victims in ((1, [old]), (0, [recent, old])):
                with self.subTest(historyLimit=limit):
                    objects = upgrade(config, "--set", f"configRevisionPruning.enabled=true,configRevisionPruning.historyLimit={limit}")
                    job, = (obj for obj in objects if obj["kind"] == "Job"
                            and obj["metadata"]["name"] == "genai-otel-bridge-config-revisions")
                    container, = job["spec"]["template"]["spec"]["containers"]
                    env = {item["name"]: item["value"] for item in container["env"] if "value" in item}
                    env.update(PATH=f"{tmp}:{os.environ['PATH']}", HOME=tmp, TMPDIR=tmp,
                               POD_NAMESPACE="default", KUBERNETES_SERVICE_HOST="unused", KUBERNETES_SERVICE_PORT="443")
                    deletes.clear()
                    result = subprocess.run(container["command"] + container["args"], env=env, timeout=25,
                                            text=True, capture_output=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(list(deletes), victims, result.stdout)
                    for obj in maps:
                        meta = obj["metadata"]
                        if meta["name"] in victims:
                            self.assertEqual(deletes[meta["name"]]["preconditions"],
                                             {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]})

    def test_prune_cannot_be_enabled_by_false_as_a_string(self):
        with self.assertRaisesRegex(ValueError, "enabled must be a boolean"):
            render("--set-string", "configRevisionPruning.enabled=false")

    def test_prune_history_requires_nonnegative_integer(self):
        for value in ("-1", "1.5", "x"):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "historyLimit must be a non-negative integer"):
                render("--set", f"configRevisionPruning.enabled=true,configRevisionPruning.historyLimit={value}")


if __name__ == "__main__":
    unittest.main()
