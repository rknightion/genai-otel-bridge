#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Assert content-addressed chart config using the decoded volume payload."""

import hashlib
import os
import subprocess
import unittest

import yaml


def render(*args):
    return subprocess.run(
        [os.getenv("HELM", "helm"), "template", "test-release", os.getenv("CHART", "deploy/helm"), *args],
        text=True,
        capture_output=True,
        check=True,
    ).stdout


def revision(*args):
    objects = [obj for obj in yaml.safe_load_all(render(*args)) if obj]
    deployment, = (obj for obj in objects if obj["kind"] == "Deployment")
    volumes = deployment["spec"]["template"]["spec"]["volumes"]
    name, = (volume["configMap"]["name"] for volume in volumes if volume["name"] == "config")
    configmap, = (obj for obj in objects if obj["kind"] == "ConfigMap" and obj["metadata"]["name"] == name)
    return name, configmap


class ConfigRevisions(unittest.TestCase):
    def assert_revision(self, *args):
        name, configmap = revision(*args)
        payload = configmap["data"]["config.yaml"]
        self.assertEqual(name, "genai-otel-bridge-config-" + hashlib.sha256(payload.encode()).hexdigest())
        self.assertIs(configmap.get("immutable"), True, "the mounted ConfigMap must be immutable")
        return name, payload

    def test_generated_payload(self):
        name, _ = revision("--set", "rolloutOnConfigChange=false")
        changed, _ = revision("--set", "rolloutOnConfigChange=false,config.log.format=json")
        self.assertNotEqual(name, changed, "changing config must change the pod's ConfigMap reference")
        self.assert_revision()
        # Changing chart settings that are not part of the file must not mint a revision.
        self.assertEqual(name, self.assert_revision("--set", "replicas=1")[0])

    def test_tenant_payload_excludes_legacy_keys(self):
        _, payload = self.assert_revision("-f", "deploy/helm/examples/tenants-values.yaml")
        config = yaml.safe_load(payload)
        self.assertTrue(config["tenants"])
        self.assertNotIn("sources", config)
        self.assertNotIn("telemetry", config["emit"])

    def test_override_retains_exact_bytes_and_changes_pod_reference(self):
        payload = '# not part of any logical plan\r\n  log: {format: "json"}\r\n\r\n'
        name, data = self.assert_revision("--set-string", f"configOverride={payload}")
        self.assertEqual(data, payload)
        changed = payload.replace("logical plan", "runtime plan")
        new_name, _ = self.assert_revision("--set-string", f"configOverride={changed}")
        self.assertNotEqual(name, new_name)

    def test_offline_delete_is_bounded_to_rendered_revision(self):
        expected, _ = revision()
        objects = [obj for obj in yaml.safe_load_all(render()) if obj]
        role, = (obj for obj in objects if obj["kind"] == "Role"
                 and obj["metadata"]["name"] == "genai-otel-bridge-config-revisions")
        rules = [rule for rule in role["rules"] if {"configmaps", "*"}.intersection(rule["resources"])
                 and {"delete", "deletecollection", "*"}.intersection(rule["verbs"])]
        self.assertTrue(rules, "the hook must be able to remove the rendered revision on uninstall")
        for rule in rules:
            self.assertEqual(rule["resourceNames"], [expected])
            self.assertTrue(set(rule["verbs"]) <= {"get", "delete"})

    def test_history_limit_requires_nonnegative_integer(self):
        for value in ["-1", "1.5"]:
            with self.subTest(value=value):
                with self.assertRaises(subprocess.CalledProcessError) as error:
                    render("--set", f"configRevisionHistoryLimit={value}")
                self.assertIn("configRevisionHistoryLimit must be a non-negative integer", error.exception.stderr)


if __name__ == "__main__":
    unittest.main()
