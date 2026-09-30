"""Release-boundary tests; no real credentials, Docker or network access."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


publish = load('publish')
context = load('github_context').context


class ContextTests(unittest.TestCase):
    def setUp(self):
        self.env = dict(GITHUB_REPOSITORY=publish.REPOSITORY, GITHUB_SHA='a' * 40,
                        GITHUB_REF='refs/heads/ewu/v2.6.0', GITHUB_EVENT_NAME='push',
                        GITHUB_RUN_ID='123', GITHUB_RUN_ATTEMPT='1')

    def test_branch_build_does_not_publish(self):
        self.assertFalse(context(self.env, {})[1])

    def test_manual_publication_requires_explicit_choice(self):
        self.env['GITHUB_EVENT_NAME'] = 'workflow_dispatch'
        self.assertFalse(context(self.env, {})[1])
        self.assertFalse(context(self.env, {'inputs': {'publish': 'false'}})[1])
        self.assertTrue(context(self.env, {'inputs': {'publish': 'true'}})[1])

    def test_tag_release_and_invalid_tag(self):
        self.env['GITHUB_REF'] = 'refs/tags/v2.6.0-ewu.1'
        self.assertEqual(context(self.env, {}), ('v2.6.0-ewu.1', True))
        self.env['GITHUB_REF'] = 'refs/tags/v2.6.0'
        with self.assertRaises(ValueError): context(self.env, {})

    def test_retry_has_unique_candidate_version(self):
        old = context(self.env, {})[0]
        self.env['GITHUB_RUN_ATTEMPT'] = '2'
        self.assertNotEqual(old, context(self.env, {})[0])

    def test_pull_requests_and_other_repositories_are_rejected(self):
        self.env['GITHUB_EVENT_NAME'] = 'pull_request'
        with self.assertRaises(ValueError): context(self.env, {})
        self.env['GITHUB_REPOSITORY'] = 'someone/else'
        with self.assertRaises(ValueError): context(self.env, {})


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = Path(self.temp.name)
        for name in ('image-1.oci.tar',):
            (self.output / name).write_bytes(name.encode())
        self.release = dict(version='v2.6.0-ewu.1', revision='a' * 40,
                            runtimeManifestDigest='sha256:runtime',
                            rebuildRuntimeManifestDigest='sha256:runtime',
                            ociArchiveSHA256=publish.digest(self.output / 'image-1.oci.tar'))
        (self.output / 'release.json').write_text(json.dumps(self.release))
        self.env = dict(GITHUB_ACTIONS='true', EWU_PUBLISH='true', GH_TOKEN='dummy',
                        GITHUB_REPOSITORY=publish.REPOSITORY, GITHUB_SHA='a' * 40,
                        EWU_VERSION=self.release['version'], NEXSPENCE_IMAGE_REPOSITORY=publish.IMAGE)
        self.index = json.dumps({'manifests': [{'digest': 'sha256:runtime'}]}).encode()
        self.commands = []
        self.existing = None
        self.registry_error = b'manifest unknown'
        self.fail_create = False
        self.downloads = []

    def run_command(self, args, **kwargs):
        self.commands.append(args)
        if kwargs.get('capture_output'):
            return subprocess.CompletedProcess(args, 0 if self.existing else 1,
                                               self.existing or b'', self.registry_error)
        if self.fail_create and args[:3] == ('gh', 'release', 'create'):
            raise subprocess.CalledProcessError(1, args)
        return subprocess.CompletedProcess(args, 0)

    def invoke(self, remote=None):
        with patch.dict(os.environ, self.env, clear=True), \
             patch('sys.argv', ['publish.py', str(self.output)]), \
             patch.object(publish.subprocess, 'run', side_effect=self.run_command), \
             patch.object(publish.subprocess, 'check_output', side_effect=[self.index, remote or self.index]), \
             patch.object(publish, 'ensure_source_tag'), \
             patch.object(publish, 'verify_download', side_effect=lambda *a: self.downloads.append(a)), \
             contextlib.redirect_stdout(io.StringIO()):
            publish.main()

    def test_local_invocation_is_rejected(self):
        self.env.pop('GITHUB_ACTIONS')
        with self.assertRaisesRegex(RuntimeError, 'only allowed'): self.invoke()
        self.assertFalse(self.commands)

    def test_checksum_mismatch_stops_before_network(self):
        (self.output / 'image-1.oci.tar').write_bytes(b'changed')
        with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'): self.invoke()
        self.assertFalse(self.commands)

    def test_other_revision_is_rejected(self):
        self.env['GITHUB_SHA'] = 'b' * 40
        with self.assertRaisesRegex(RuntimeError, 'revision/version'): self.invoke()
        self.assertFalse(self.commands)

    def test_existing_image_conflict_stops_before_release_creation(self):
        self.existing = b'changed attestations'
        with self.assertRaisesRegex(RuntimeError, 'different content'): self.invoke()
        self.assertEqual(len(self.commands), 1)

    def test_registry_auth_failure_is_not_absence(self):
        self.registry_error = b'unauthorized'
        with self.assertRaisesRegex(RuntimeError, 'Cannot verify'): self.invoke()
        self.assertEqual(len(self.commands), 1)

    def test_release_creation_failure_does_not_push_image(self):
        self.fail_create = True
        with self.assertRaises(subprocess.CalledProcessError): self.invoke()
        self.assertFalse(any('copy' in cmd for cmd in self.commands))

    def test_publication_order_and_private_image_archive(self):
        self.invoke()
        create = next(i for i,c in enumerate(self.commands) if c[:3] == ('gh','release','create'))
        copy = next(i for i,c in enumerate(self.commands) if 'copy' in c)
        edit = next(i for i,c in enumerate(self.commands) if c[:3] == ('gh','release','edit'))
        self.assertLess(create, copy)
        self.assertLess(copy, edit)
        self.assertIn('--draft', self.commands[create])
        self.assertNotIn(str(self.output / 'image-1.oci.tar'), self.commands[create])
        self.assertNotIn('image-1.oci.tar', (self.output / 'SHA256SUMS').read_text())
        self.assertEqual(len(self.downloads), 3)
        self.assertFalse(any(str(arg).endswith(".tar.gz") for arg in self.commands[create]))
        self.assertIn("--verify-tag", self.commands[create])
        notes = (self.output / 'release-notes.md').read_text()
        self.assertIn('https://github.com/' + publish.REPOSITORY, notes)
        self.assertNotIn('/releases/download/', notes)
        proof = json.loads((self.output / 'publication.json').read_text())
        self.assertEqual(proof['sourceURL'], 'https://github.com/' + publish.REPOSITORY)
        self.assertEqual(proof['sourceTag'], self.release['version'])
        self.assertTrue(proof['sourceRevisionURL'].endswith('/tree/' + 'a' * 40))
        self.assertEqual(proof['imageIndexDigest'], 'sha256:' + hashlib.sha256(self.index).hexdigest())

    def test_remote_digest_mismatch_keeps_release_draft(self):
        with self.assertRaisesRegex(RuntimeError, 'index digest differs'):
            self.invoke(remote=b'changed')
        self.assertFalse(any(c[:3] == ('gh','release','edit') for c in self.commands))
        self.assertFalse(self.downloads)

    def test_public_download_checksum(self):
        with patch.object(publish.urllib.request, 'urlopen', return_value=io.BytesIO(b'bad')):
            with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
                publish.verify_download('https://example.test/source', 'wrong')


class SourceTagTests(unittest.TestCase):
    def invoke(self, results, annotated=None):
        sha = 'a' * 40
        with patch.object(publish.urllib.request, 'urlopen', return_value=io.BytesIO(json.dumps({'sha': sha}).encode())), \
             patch.object(publish.subprocess, 'run', side_effect=results) as commands, \
             patch.object(publish.subprocess, 'check_output', return_value=json.dumps({'object': annotated}).encode()):
            publish.ensure_source_tag('v2.6.0-ewu.1', sha)
            return commands.call_args_list

    def response(self, sha, kind='commit'):
        return subprocess.CompletedProcess([], 0, json.dumps({'object': {'type': kind, 'sha': sha}}).encode(), b'')

    def test_matching_lightweight_tag_is_preserved(self):
        calls = self.invoke([self.response('a' * 40)])
        self.assertEqual(len(calls), 1)

    def test_annotated_tag_resolves_to_commit(self):
        self.invoke([self.response('b' * 40, 'tag')], {'type': 'commit', 'sha': 'a' * 40})

    def test_different_tag_is_rejected(self):
        with self.assertRaisesRegex(RuntimeError, 'does not point'):
            self.invoke([self.response('b' * 40)])

    def test_missing_tag_created_at_exact_commit(self):
        calls = self.invoke([subprocess.CompletedProcess([], 1, b'', b'HTTP 404'),
                             subprocess.CompletedProcess([], 0), self.response('a' * 40)])
        self.assertIn('sha=' + 'a' * 40, calls[1].args[0])
        self.assertIn('ref=refs/tags/v2.6.0-ewu.1', calls[1].args[0])

    def test_permission_failure_does_not_create_tag(self):
        with self.assertRaisesRegex(RuntimeError, 'Cannot verify'):
            self.invoke([subprocess.CompletedProcess([], 1, b'', b'HTTP 403')])


if __name__ == '__main__':
    unittest.main()
