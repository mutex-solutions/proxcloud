"""Source-only contracts for the build smoke; never import it or start kernels.

These checks are not evidence that the target image's kernels actually run. The
Docker build gate performs that separately in hosted CI. Authored, not run here.
"""
import ast
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
SMOKE = ROOT / "deploy/templates/code-interpreter/smoke-kernels.py"
DOCKERFILE = ROOT / "deploy/templates/code-interpreter/Dockerfile"


class InterpreterSmokeSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = SMOKE.read_text()
        cls.tree = ast.parse(cls.source)
        cls.dockerfile = DOCKERFILE.read_text()
        cls.constants = {
            node.targets[0].id: ast.literal_eval(node.value)
            for node in cls.tree.body
            if isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name)
        }

    def test_exact_installed_kernel_names_and_fixed_known_answers(self):
        self.assertEqual(self.constants["MARKER"], "PROXCLOUD_KERNEL_OK")
        self.assertEqual(self.constants["CASES"], (
            ("python3", "import sys; print('PROXCLOUD_KERNEL_OK'); print(sys.version)"),
            ("javascript", "console.log('PROXCLOUD_KERNEL_OK'); console.log(process.version);"),
            ("bash", "printf 'PROXCLOUD_KERNEL_OK\\n'"),
        ))

    def test_standard_client_api_not_e2b_server_or_shell_execution(self):
        imports = [node.module for node in ast.walk(self.tree) if isinstance(node, ast.ImportFrom)]
        self.assertIn("jupyter_client", imports)
        calls = [node for node in ast.walk(self.tree) if isinstance(node, ast.Call)]
        self.assertEqual(sum(isinstance(node.func, ast.Name) and node.func.id == "KernelManager"
                             for node in calls), 1)
        self.assertFalse(any(isinstance(node.func, ast.Name) and node.func.id in {"eval", "exec"}
                             for node in calls))
        self.assertNotIn("requests", imports)
        self.assertNotIn("urllib", imports)
        self.assertNotIn("subprocess.run", self.source)
        self.assertNotIn("os.system", self.source)

    def test_private_connection_files_loopback_and_preserved_kernelspec_home(self):
        self.assertIn('tempfile.TemporaryDirectory(prefix="proxcloud-kernel-")', self.source)
        self.assertIn('ip="127.0.0.1"', self.source)
        self.assertIn('connection_file=str(Path(directory) / "kernel.json")', self.source)
        self.assertIn('dict(os.environ, JUPYTER_RUNTIME_DIR=directory, IPYTHONDIR=directory)', self.source)
        self.assertIn('km.start_kernel(cwd=directory, env=env, stdout=DEVNULL, stderr=DEVNULL)', self.source)
        self.assertNotIn('HOME=', self.source)

    def test_deadlines_and_outer_process_group_kill_bound(self):
        self.assertEqual(self.constants["KERNEL_SECONDS"], 20)
        self.assertEqual(self.constants["TOTAL_SECONDS"], 75)
        self.assertIn('min(overall_deadline, time.monotonic() + KERNEL_SECONDS)', self.source)
        self.assertIn('client.wait_for_ready(timeout=remaining(deadline))', self.source)
        self.assertIn('client.get_iopub_msg(timeout=min(1, remaining(deadline)))', self.source)
        self.assertIn('client.get_shell_msg(timeout=min(1, remaining(deadline)))', self.source)
        self.assertIn('RUN timeout --kill-after=5s 85s python /tmp/proxcloud-smoke-kernels.py', self.dockerfile)

    def test_matching_reply_idle_marker_and_error_are_all_required(self):
        self.assertIn('message.get("parent_header", {}).get("msg_id") != msg_id', self.source)
        self.assertIn('reply.get("parent_header", {}).get("msg_id") == msg_id', self.source)
        self.assertIn('kind == "status" and content.get("execution_state") == "idle"', self.source)
        self.assertIn('elif kind == "error":\n                    error = True', self.source)
        self.assertIn('reply["msg_type"] != "execute_reply"', self.source)
        self.assertIn('reply["content"].get("status") != "ok"', self.source)
        self.assertIn('or error or MARKER not in "".join(output).splitlines()', self.source)
        self.assertIn('store_history=False, allow_stdin=False', self.source)
        # Both teardown operations must be inside a finally, including on failure.
        finalizers = [ast.unparse(node) for block in ast.walk(self.tree)
                      if isinstance(block, ast.Try) for node in block.finalbody]
        self.assertTrue(any('client.stop_channels()' in block for block in finalizers))
        self.assertTrue(any('km.shutdown_kernel(now=True)' in block for block in finalizers))

    def test_docker_gate_uses_image_python_before_setup_and_removes_script(self):
        install = self.dockerfile.index('&& ijsinstall --install=global')
        copy = self.dockerfile.index('COPY code-interpreter/smoke-kernels.py /tmp/proxcloud-smoke-kernels.py')
        gate = self.dockerfile.index('RUN timeout --kill-after=5s 85s python /tmp/proxcloud-smoke-kernels.py && rm /tmp/proxcloud-smoke-kernels.py')
        setup = self.dockerfile.index('COPY common/setup-user.sh')
        self.assertLess(install, copy)
        self.assertLess(copy, gate)
        self.assertLess(gate, setup)
        self.assertIn('RUN proxcloud-setup-user\nWORKDIR /home/user', self.dockerfile)


if __name__ == "__main__":
    unittest.main(verbosity=2)
