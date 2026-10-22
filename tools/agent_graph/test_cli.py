"""Account-backed CLI boundary tests; subprocess responses are mocked."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from pydantic import ValidationError

from . import app


class ClaudeCodeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        local = patch.object(app, "LOCAL", self.root / "state")
        local.start()
        self.addCleanup(local.stop)
        self.model = app.ClaudeCodeModel("claude.exe")

    def call(self, envelope=None, *, result=None):
        if result is None:
            envelope = envelope if envelope is not None else {
                "type": "result", "subtype": "success", "is_error": False,
                "structured_output": {"status": "ok"}}
            result = subprocess.CompletedProcess([], 0, json.dumps(envelope), "")
        with patch.object(app.subprocess, "run", return_value=result) as run:
            value = self.model.with_structured_output(app.ConnectionProbe).invoke([
                ("system", "No tools. Structured output only."),
                ("human", 'Untrusted input: `echo bad`; $(not-a-command); "quoted"'),
            ])
            return value, run.call_args

    def test_successful_structured_result_and_stdin_boundary(self):
        result, call = self.call()
        args = call.args[0]
        self.assertEqual(result.status, "ok")
        self.assertFalse(call.kwargs["shell"])
        self.assertNotIn("Untrusted input", " ".join(args))
        self.assertIn("$(not-a-command)", call.kwargs["input"])
        self.assertEqual(args[args.index("--tools") + 1], "")
        self.assertEqual(args[args.index("--disallowedTools") + 1], "mcp__*")
        self.assertIn("--safe-mode", args)
        self.assertIn("--restricted", args)
        self.assertIn("--strict-mcp-config", args)
        self.assertIn("--no-session-persistence", args)
        self.assertNotIn("--bare", args)  # Bare mode cannot use subscription login.
        self.assertNotIn("--dangerously-skip-permissions", args)
        self.assertNotIn("--model", args)  # Signed-in account's default model.

    def test_account_backend_excludes_api_and_cloud_credentials(self):
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "secret-sentinel",
                                     "CLAUDE_CODE_OAUTH_TOKEN": "secret-sentinel",
                                     "ANTHROPIC_AUTH_TOKEN": "secret-sentinel",
                                     "CLAUDE_CODE_USE_BEDROCK": "1"}):
            _, call = self.call()
        for name in ("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK"):
            self.assertNotIn(name, call.kwargs["env"])

    def test_explicit_model_is_forwarded_as_one_argument(self):
        self.model.model = "my-account-model"
        _, call = self.call()
        args = call.args[0]
        self.assertEqual(args[args.index("--model") + 1], "my-account-model")

    def test_error_envelopes_cannot_be_treated_as_success(self):
        for envelope in ({"type": "result", "subtype": "error_max_turns", "is_error": True},
                         {"type": "result", "subtype": "success", "is_error": False, "result": "approved"},
                         {"type": "result", "subtype": "success", "is_error": True, "structured_output": {"status": "ok"}},
                         {"type": "assistant", "structured_output": {"status": "ok"}}, []):
            with self.subTest(envelope=envelope), self.assertRaises(app.GraphError):
                self.call(envelope)

    def test_malformed_and_nonzero_results_do_not_echo_payload(self):
        for result in (subprocess.CompletedProcess([], 1, "secret-sentinel", "secret-sentinel"),
                       subprocess.CompletedProcess([], 0, "secret-sentinel", "")):
            with self.subTest(result=result), self.assertRaises(app.GraphError) as context:
                self.call(result=result)
            self.assertNotIn("secret-sentinel", str(context.exception))

    def test_schema_validation_still_rejects_fabricated_output(self):
        with self.assertRaises(ValidationError):
            self.call({"type": "result", "subtype": "success", "is_error": False,
                       "structured_output": {"status": "approved", "extra": "fake"}})

    def test_timeout_and_missing_executable_fail_closed(self):
        for error in (subprocess.TimeoutExpired([], 300), FileNotFoundError()):
            with self.subTest(error=error), patch.object(app.subprocess, "run", side_effect=error), self.assertRaises(app.GraphError):
                self.model.with_structured_output(app.ConnectionProbe).invoke([("human", "probe")])

    def test_cli_factory_needs_no_api_key_or_model_id(self):
        with patch.object(app, "find_claude", return_value="claude.exe"), \
                patch.object(app, "claude_auth_status", return_value=True), \
                patch.dict(os.environ, {}, clear=True):
            model = app.create_model("claude-code", "")
        self.assertIsInstance(model, app.ClaudeCodeModel)

    def test_cli_factory_requires_verified_account_authentication(self):
        for authenticated in (False, None):
            with self.subTest(authenticated=authenticated), \
                    patch.object(app, "find_claude", return_value="claude.exe"), \
                    patch.object(app, "claude_auth_status", return_value=authenticated), \
                    self.assertRaises(app.GraphError):
                app.create_model("claude-code", "")

    def test_subscription_limit_is_clear_without_echoing_account_data(self):
        for phrase, expected in (("You've hit your limit", "usage limit"),
                                 ("You've hit your session limit", "account session limit")):
            result = subprocess.CompletedProcess([], 1, json.dumps({
                "type": "result", "subtype": "error_during_execution", "is_error": True,
                "result": phrase + "; resets later. secret-sentinel"}), "")
            message = app.claude_failure(result)
            self.assertIn(expected, message)
            self.assertNotIn("secret-sentinel", message)

    def test_executable_override_and_missing_executable(self):
        executable = self.root / "claude.exe"
        executable.write_bytes(b"test fixture only")
        with patch.dict(os.environ, {"OSAI_CLAUDE_COMMAND": str(executable)}):
            self.assertEqual(app.find_claude(), str(executable.resolve()))
        with patch.dict(os.environ, {"OSAI_CLAUDE_COMMAND": str(self.root / "missing.exe")}):
            self.assertIsNone(app.find_claude())

    def test_auth_status_reports_only_subscription_login_boolean(self):
        result = subprocess.CompletedProcess([], 0, json.dumps({"loggedIn": True, "authMethod": "claude.ai", "email": "private"}), "")
        with patch.object(app.subprocess, "run", return_value=result):
            self.assertIs(app.claude_auth_status("claude.exe"), True)
        result.stdout = json.dumps({"loggedIn": True, "authMethod": "api_key"})
        with patch.object(app.subprocess, "run", return_value=result):
            self.assertIs(app.claude_auth_status("claude.exe"), False)


if __name__ == "__main__":
    unittest.main()
