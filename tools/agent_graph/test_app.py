"""Offline failure-path tests; no paid model calls, Go builds, or real funds."""

import json
from contextlib import closing, redirect_stderr, redirect_stdout
import io
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from langgraph.checkpoint.sqlite import SqliteSaver
from pydantic import ValidationError

from . import app


class FakeModel:
    def __init__(self, **responses):
        self.responses = {key: list(values) for key, values in responses.items()}
        self.calls = []

    def with_structured_output(self, schema):
        owner = self

        class Runnable:
            def invoke(self, messages):
                owner.calls.append(schema.__name__)
                response = owner.responses[schema.__name__].pop(0)
                if isinstance(response, Exception):
                    raise response
                return response

        return Runnable()


class AgentGraphTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root_patch = patch.object(app, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        self.name = "services/example/source.go"
        self.path = self.root / self.name
        self.path.parent.mkdir(parents=True)
        self.raw = b"package example\n// original local change\n"
        self.path.write_bytes(self.raw)
        self.sha = app.digest(self.raw)
        self.snap = {"head": "test", "worktree": "dirty", "inventory": [self.name],
                     "source": {self.name: {"sha256": self.sha, "text": self.raw.decode(), "truncated": False}},
                     "knowledge": {}, "omitted": [], "scope": "Phase 5"}
        self.audit = {"findings": [{"decision": "source review", "status": "partial",
                                   "evidence": [{"path": self.name, "start_line": 1, "end_line": 1,
                                                 "quote": "package example"}], "gap": "test gap",
                                   "recommendation": "review", "risk": "low"}], "limitations": ["bounded"]}
        self.plan = {"task": "review source", "paths": [self.name], "blocked": False, "reason": "authorized source edit"}
        self.proposal = {"summary": "preserve local change", "edits": [{"path": self.name,
                         "expected_sha256": self.sha, "content": self.raw.decode() + "// proposed\n"}],
                         "blocked": False, "reason": "source only"}
        self.verdict = {"decision": "approved", "reasons": ["source review only"]}

    def graph(self, conn, *, verdicts=None, proposals=None, plan=None):
        auditor = FakeModel(Audit=[self.audit], Plan=[plan or self.plan])
        coder = FakeModel(Proposal=proposals or [self.proposal])
        guardian = FakeModel(Verdict=verdicts or [self.verdict])
        graph = app.build_graph(auditor, coder, guardian, SqliteSaver(conn),
                                snapshot_loader=lambda: self.snap)
        return graph, coder, guardian

    def test_graph_emits_review_without_changing_dirty_source(self):
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph, _, _ = self.graph(conn)
            result = graph.invoke({"task": "Phase 5"}, {"configurable": {"thread_id": "one"}})
        self.assertEqual(result["status"], "needs_human")
        self.assertEqual(result["iteration"], 1)
        self.assertEqual(result["test_results"], [])
        self.assertEqual(self.path.read_bytes(), self.raw)
        app.save_report(result, self.root)
        self.assertIn("// proposed", (self.root / "proposal.patch").read_text())
        self.assertNotIn("done", result["status"])

    def test_rejected_guardian_stops_at_attempt_limit(self):
        rejection = {"decision": "rejected", "reasons": ["insufficient accounting authority"]}
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph, coder, guardian = self.graph(conn, verdicts=[rejection, rejection],
                                                proposals=[self.proposal, self.proposal])
            result = graph.invoke({"task": "Phase 5"}, {"configurable": {"thread_id": "two"}})
        self.assertEqual(result["iteration"], 2)
        self.assertEqual(coder.calls, ["Proposal", "Proposal"])
        self.assertEqual(guardian.calls, ["Verdict", "Verdict"])
        app.save_report(result, self.root)
        self.assertEqual((self.root / "proposal.patch").read_text(), "")
        with self.assertRaises(ValueError):
            app.apply_report(result)

    def test_missing_authority_stops_before_coder(self):
        plan = {**self.plan, "blocked": True, "reason": "No approved account mappings"}
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph, coder, _ = self.graph(conn, plan=plan)
            result = graph.invoke({"task": "Phase 5"}, {"configurable": {"thread_id": "blocked"}})
        self.assertEqual(coder.calls, [])
        self.assertEqual(result["status"], "needs_human")

    def test_sqlite_resumes_after_process_restart(self):
        checkpoint = self.root / "checkpoint.sqlite"
        config = {"configurable": {"thread_id": "resume"}}
        with closing(sqlite3.connect(checkpoint, check_same_thread=False)) as conn:
            graph, _, _ = self.graph(conn, proposals=[RuntimeError("provider unavailable")])
            with self.assertRaises(RuntimeError):
                graph.invoke({"task": "Phase 5"}, config)
            self.assertEqual(graph.get_state(config).next, ("code",))
        with closing(sqlite3.connect(checkpoint, check_same_thread=False)) as conn:
            graph, _, _ = self.graph(conn)
            result = graph.invoke(None, config)
        self.assertEqual(result["iteration"], 1)
        self.assertEqual(result["status"], "needs_human")

    def test_guardian_substring_is_not_approval(self):
        for decision in ("not approved", "APPROVED", "approved but rejected"):
            with self.subTest(decision=decision), self.assertRaises(ValidationError):
                app.Verdict(decision=decision, reasons=["text substring cannot grant approval"])

    def test_fabricated_evidence_fails_closed(self):
        audit = json.loads(json.dumps(self.audit))
        audit["findings"][0]["evidence"][0]["quote"] = "nonexistent function"
        with self.assertRaises(ValueError):
            app.validate_audit(app.Audit.model_validate(audit), self.snap)

    def test_uninspected_path_and_invalid_lines_fail_closed(self):
        for path, start, end in (("services/absent.go", 1, 1), (self.name, 2, 1), (self.name, 1, 500)):
            audit = json.loads(json.dumps(self.audit))
            audit["findings"][0]["evidence"][0].update(path=path, start_line=start, end_line=end)
            with self.subTest(path=path, end=end), self.assertRaises(ValueError):
                app.validate_audit(app.Audit.model_validate(audit), self.snap)

    def test_path_escape_and_private_files_are_refused(self):
        for path in ("../outside.go", "/tmp/source.go", "C:/source.go", "services/../source.go",
                     "services\\source.go", ".git/config", "services/.env/file.go"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                app.safe_path(path)
        for path in ("docs/handoffs/handoff.md", ".env", "go.mod", "deploy/local/docker-compose.yml"):
            self.assertFalse(app.source_path(path))

    def test_apply_preserves_local_edits_and_refuses_repeat(self):
        report = {"snapshot": self.snap, "proposal": self.proposal, "verdict": self.verdict}
        app.apply_report(report)
        self.assertIn("original local change", self.path.read_text())
        self.assertIn("proposed", self.path.read_text())
        with self.assertRaises(ValueError):
            app.apply_report(report)

    def test_drift_is_checked_before_any_file_mutates(self):
        another = "services/example/second.go"
        (self.root / another).write_bytes(self.raw)
        self.snap["source"][another] = dict(self.snap["source"][self.name])
        self.proposal["edits"].append({**self.proposal["edits"][0], "path": another})
        (self.root / another).write_text("changed after review\n")
        with self.assertRaises(ValueError):
            app.apply_report({"snapshot": self.snap, "proposal": self.proposal, "verdict": self.verdict})
        self.assertEqual(self.path.read_bytes(), self.raw)

    def test_truncated_and_duplicate_edits_are_refused(self):
        self.snap["source"][self.name]["truncated"] = True
        with self.assertRaises(ValueError):
            app.validate_proposal(app.Proposal.model_validate(self.proposal), self.snap)
        self.snap["source"][self.name]["truncated"] = False
        self.proposal["edits"].append(self.proposal["edits"][0])
        with self.assertRaises(ValueError):
            app.validate_proposal(app.Proposal.model_validate(self.proposal), self.snap)

    def test_test_failures_are_actual_results(self):
        def runner(args, **kwargs):
            return subprocess.CompletedProcess(args, 1, "assertion failed", "")
        result = app.run_check("focused", runner)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["exit_code"], 1)
        self.assertEqual(result["stdout"], "assertion failed")

    def test_timeout_and_missing_binary_never_pass(self):
        def timeout(args, **kwargs):
            raise subprocess.TimeoutExpired(args, 1800)
        def missing(args, **kwargs):
            raise FileNotFoundError()
        self.assertEqual(app.run_check("full", timeout)["status"], "timed_out")
        self.assertEqual(app.run_check("security", missing)["status"], "unavailable")

    def test_successful_go_command_does_not_claim_runtime_coverage(self):
        result = app.run_check("focused", lambda args, **kwargs: subprocess.CompletedProcess(args, 0, "ok", ""))
        self.assertEqual(result["status"], "passed")
        self.assertIn("not proven", result["runtime_coverage"])

    def test_policy_drift_invalidates_review_before_apply(self):
        policy = "policy.md"
        (self.root / policy).write_bytes(b"engineering proposal\n")
        self.snap["knowledge"][policy] = {"sha256": app.digest(b"engineering proposal\n")}
        (self.root / policy).write_bytes(b"changed policy\n")
        with self.assertRaises(app.GraphError):
            app.apply_report({"snapshot": self.snap, "proposal": self.proposal, "verdict": self.verdict})
        self.assertEqual(self.path.read_bytes(), self.raw)

    def test_doctor_reports_presence_without_credentials(self):
        output = io.StringIO()
        with patch.dict(os.environ, {"OPENAI_API_KEY": "secret-test-sentinel"}), \
                patch.object(app, "command", return_value=subprocess.CompletedProcess([], 0, "", "")), \
                patch.object(app, "find_claude", return_value=None), \
                redirect_stdout(output):
            self.assertEqual(app.main(["doctor"]), 0)
        self.assertTrue(json.loads(output.getvalue())["openai_key_configured"])
        self.assertNotIn("secret-test-sentinel", output.getvalue())

    def test_provider_exception_cannot_print_credentials(self):
        error = io.StringIO()
        with patch.object(app, "create_model", side_effect=RuntimeError("secret-test-sentinel")), redirect_stderr(error):
            self.assertEqual(app.main(["run", "--task", "Phase 5"]), 1)
        self.assertNotIn("secret-test-sentinel", error.getvalue())

    def test_baseline_check_is_labelled_before_apply(self):
        model = FakeModel(Audit=[self.audit], Plan=[self.plan], Verdict=[self.verdict])
        coder = FakeModel(Proposal=[self.proposal])
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn, \
                patch.object(app, "run_check", return_value={"status": "failed", "exit_code": 1}), \
                redirect_stdout(io.StringIO()):
            graph = app.build_graph(model, coder, model, SqliteSaver(conn),
                                    snapshot_loader=lambda: self.snap, baseline_checks=("focused",))
            result = graph.invoke({"task": "Phase 5"}, {"configurable": {"thread_id": "baseline"}})
        self.assertEqual(result["test_results"][0]["status"], "failed")
        self.assertIn("proposal NOT applied", result["test_results"][0]["scope"])
        self.assertEqual(self.path.read_bytes(), self.raw)

    def test_provider_clients_and_structured_schemas_construct_without_requests(self):
        with patch.dict(os.environ, {"OPENAI_API_KEY": "offline-test-key",
                                     "ANTHROPIC_API_KEY": "offline-test-key"}):
            for provider in ("openai", "anthropic"):
                with self.subTest(provider=provider):
                    model = app.create_model(provider, "offline-constructor-test")
                    for schema in (app.Audit, app.Plan, app.Proposal, app.Verdict):
                        self.assertIsNotNone(model.with_structured_output(schema))

    def test_junctions_are_rejected_without_following_target(self):
        with patch.object(Path, "is_junction", return_value=True), self.assertRaises(app.GraphError):
            app.safe_path(self.name)

    def test_patch_artifact_passes_git_check_for_windows_and_missing_newline(self):
        for raw, replacement in ((b"package example", "package example\n// proposed\n"),
                                  (b"package example\r\n", "package example\r\n// proposed\r\n")):
            with self.subTest(raw=raw):
                self.path.write_bytes(raw)
                self.snap["source"][self.name].update(sha256=app.digest(raw), text=raw.decode())
                self.proposal["edits"][0].update(expected_sha256=app.digest(raw), content=replacement)
                app.save_report({"snapshot": self.snap, "proposal": self.proposal, "verdict": self.verdict}, self.root)
                result = subprocess.run(["git", "apply", "--check", str(self.root / "proposal.patch")],
                                        cwd=self.root, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.path.read_bytes(), raw)

    def test_claude_cli_backend_runs_all_model_stages_without_applying(self):
        responses = [subprocess.CompletedProcess([], 0, json.dumps({
            "type": "result", "subtype": "success", "is_error": False,
            "structured_output": value}), "")
            for value in (self.audit, self.plan, self.proposal, self.verdict)]
        model = app.ClaudeCodeModel("claude.exe")
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn, \
                patch.object(app, "LOCAL", self.root / "state"), \
                patch.object(app.subprocess, "run", side_effect=responses) as run:
            graph = app.build_graph(model, model, model, SqliteSaver(conn),
                                    snapshot_loader=lambda: self.snap)
            result = graph.invoke({"task": "Phase 5"}, {"configurable": {"thread_id": "cli"}})
        self.assertEqual(run.call_count, 4)
        self.assertEqual(result["verdict"]["decision"], "approved")
        self.assertEqual(result["status"], "needs_human")
        self.assertEqual(self.path.read_bytes(), self.raw)


if __name__ == "__main__":
    unittest.main()
