"""PRD ingestion, citation, phase dependency and read-only planning tests."""

from contextlib import closing, redirect_stdout
import io
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from pydantic import ValidationError
from pypdf import PdfWriter
from pypdf.generic import DictionaryObject, NameObject, DecodedStreamObject
from langgraph.checkpoint.sqlite import SqliteSaver

from . import app, planning
from .test_app import FakeModel


def write_pdf(path, text="Baseline PRD"):
    writer = PdfWriter()
    page = writer.add_blank_page(width=300, height=300)
    if text:
        font = DictionaryObject({NameObject("/Type"): NameObject("/Font"),
                                 NameObject("/Subtype"): NameObject("/Type1"),
                                 NameObject("/BaseFont"): NameObject("/Helvetica")})
        page[NameObject("/Resources")] = DictionaryObject({NameObject("/Font"):
            DictionaryObject({NameObject("/F1"): writer._add_object(font)})})
        stream = DecodedStreamObject()
        stream.set_data(f"BT /F1 12 Tf 10 100 Td ({text}) Tj ET".encode())
        page[NameObject("/Contents")] = writer._add_object(stream)
    with path.open("wb") as output:
        writer.write(output)


class PlanningTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for patcher in (patch.object(app, "ROOT", self.root),
                        patch.object(app, "LOCAL", self.root / ".cache"),
                        patch.object(app, "git", side_effect=self.git)):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.source = self.root / "services" / "example.go"
        self.source.parent.mkdir()
        self.source.write_bytes(b"package example\n")
        for name in app.KNOWLEDGE:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b"Status: proposed\nEngineering evidence, not approval.\n")
        self.prd = self.root / "prd.pdf"
        write_pdf(self.prd)
        self.context = planning.prepare(self.prd)
        citations = [{"path": "PRD#page=1", "start_line": 1, "end_line": 1, "quote": "Baseline PRD"}]
        citations += [{"path": name, "start_line": 1, "end_line": 1, "quote": "Status: proposed"} for name in app.KNOWLEDGE]
        citations.append({"path": "services/example.go", "start_line": 1, "end_line": 1, "quote": "package example"})
        self.alignment = planning.Alignment(summary="Bounded review", verification="static_source_review_only",
            requirements=[planning.Requirement(id="R01", prd_sections=list(range(1, 19)),
                decision="test coverage", status="partial", evidence=citations, gap="not implemented",
                recommendation="propose work", risk="high")], conflicts=[],
            missing_external_evidence=["Approved custody evidence"], limitations=["No runtime proof"])
        self.roadmap = planning.ProposedRoadmap(phases=[planning.Phase(number=n, title=f"Task {n}",
            track="A", owner_roles=["Engineering"],
            depends_on=[] if n == 5 else [5], requirement_ids=["R01"], tasks=["Review approved scope"],
            acceptance_criteria=["Execute and record the required tests"], external_dependencies=["Account mappings"],
            execution_status="proposal_only") for n in (5, 6)],
            decisions_before_execution=["Assign owner and approve scope"], execution_authorized=False)

    def git(self, *args):
        if args[0] == "ls-files":
            return "services/example.go\0"
        if args[0] == "rev-parse":
            return "test-head\n"
        if args[0] == "status":
            return " M services/example.go\n"
        raise AssertionError(args)

    def test_pdf_pages_and_all_eight_statuses_are_ingested(self):
        self.assertEqual(self.context["prd"]["pages"], 1)
        self.assertEqual(self.context["prd"]["sha256"], app.digest(self.prd.read_bytes()))
        for name in app.KNOWLEDGE:
            self.assertIn("not approval", self.context["documents"][name]["text"])
            self.assertFalse(self.context["documents"][name]["truncated"])
        self.assertIn("services/example.go", self.context["manifest"])

    def test_scanned_or_empty_pdf_is_not_silently_accepted(self):
        write_pdf(self.prd, "")
        with self.assertRaises(app.GraphError):
            planning.load_prd(self.prd)

    def test_missing_knowledge_document_blocks_full_comparison(self):
        (self.root / app.KNOWLEDGE[0]).unlink()
        with self.assertRaises(app.GraphError):
            planning.prepare(self.prd)

    def test_alignment_requires_all_sections_and_all_eight_documents(self):
        planning.validate_alignment(self.alignment, self.context)
        audit = self.alignment.model_copy(deep=True)
        audit.requirements[0].prd_sections = [1, 2]
        with self.assertRaises(app.GraphError):
            planning.validate_alignment(audit, self.context)
        audit = self.alignment.model_copy(deep=True)
        audit.requirements[0].evidence = [e for e in audit.requirements[0].evidence if e.path != app.KNOWLEDGE[0]]
        with self.assertRaises(app.GraphError):
            planning.validate_alignment(audit, self.context)

    def test_fabricated_prd_quote_is_rejected(self):
        audit = self.alignment.model_copy(deep=True)
        audit.requirements[0].evidence[0].quote = "All approvals granted"
        with self.assertRaises(app.GraphError):
            planning.validate_alignment(audit, self.context)

    def test_future_phase_cannot_be_authorized_by_model(self):
        data = self.roadmap.model_dump()
        data["execution_authorized"] = True
        with self.assertRaises(ValidationError):
            planning.ProposedRoadmap.model_validate(data)

    def test_cycles_or_ungated_future_phases_are_rejected(self):
        for dependencies in ([6], [], [99]):
            roadmap = self.roadmap.model_copy(deep=True)
            roadmap.phases[1].depends_on = dependencies
            with self.subTest(dependencies=dependencies), self.assertRaises(app.GraphError):
                planning.validate_roadmap(roadmap, self.alignment)

    def test_unknown_requirement_and_unmapped_gap_are_rejected(self):
        roadmap = self.roadmap.model_copy(deep=True)
        roadmap.phases[0].requirement_ids = ["R99"]
        with self.assertRaises(app.GraphError):
            planning.validate_roadmap(roadmap, self.alignment)
        audit = self.alignment.model_copy(deep=True)
        row = audit.requirements[0].model_copy(deep=True)
        row.id = "R02"
        audit.requirements.append(row)
        with self.assertRaises(app.GraphError):
            planning.validate_roadmap(self.roadmap, audit)

    def test_source_and_policy_drift_require_a_fresh_review(self):
        planning.validate_current_context(self.context)
        self.source.write_bytes(b"package changed\n")
        with self.assertRaises(app.GraphError):
            planning.validate_current_context(self.context)
        self.source.write_bytes(b"package example\n")
        (self.root / app.KNOWLEDGE[0]).write_bytes(b"Changed policy\n")
        with self.assertRaises(app.GraphError):
            planning.validate_current_context(self.context)

    def test_prepare_only_does_not_call_a_model_or_change_source(self):
        with patch.object(app, "create_model", side_effect=AssertionError("No model should run")), redirect_stdout(io.StringIO()):
            self.assertEqual(app.main(["plan", "--prd", str(self.prd), "--thread", "offline", "--prepare-only"]), 0)
        self.assertEqual(self.source.read_bytes(), b"package example\n")
        self.assertTrue((app.LOCAL / "runs" / "offline" / "planning-context.json").exists())

    def test_planning_graph_audits_then_proposes_without_source_changes(self):
        model = FakeModel(Alignment=[self.alignment], ProposedRoadmap=[self.roadmap])
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph = planning.build_graph(model, SqliteSaver(conn))
            state = graph.invoke({"context": self.context}, {"configurable": {"thread_id": "planning"}})
        self.assertEqual(model.calls, ["Alignment", "ProposedRoadmap"])
        self.assertEqual(state["status"], "proposal_only")
        planning.write_outputs(state, self.root)
        self.assertIn("Proposed Phase 6", (self.root / "proposed-phases.md").read_text(encoding="utf-8"))
        self.assertFalse(state["proposed_roadmap"]["execution_authorized"])
        self.assertEqual(self.source.read_bytes(), b"package example\n")

    def test_invalid_report_is_not_published_before_validation(self):
        roadmap = self.roadmap.model_copy(deep=True)
        roadmap.phases[1].depends_on = [6]
        with self.assertRaises(app.GraphError):
            planning.write_outputs({"context": self.context, "alignment": self.alignment.model_dump(),
                                    "proposed_roadmap": roadmap.model_dump()}, self.root)
        self.assertFalse((self.root / "planning-review.json").exists())

    def test_missing_prd_citation_is_corrected_and_attempt_preserved(self):
        invalid = self.alignment.model_copy(deep=True)
        invalid.requirements[0].evidence = [e for e in invalid.requirements[0].evidence
                                            if not e.path.startswith("PRD#")]
        model = FakeModel(Alignment=[invalid, self.alignment], ProposedRoadmap=[self.roadmap])
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph = planning.build_graph(model, SqliteSaver(conn), self.root)
            state = graph.invoke({"context": self.context}, {"configurable": {"thread_id": "repair"}})
        self.assertEqual(model.calls, ["Alignment", "Alignment", "ProposedRoadmap"])
        self.assertEqual(state["status"], "proposal_only")
        records = [json.loads(p.read_text(encoding="utf-8")) for p in self.root.glob("Alignment-*.json")]
        self.assertEqual(len(records), 2)
        self.assertEqual(sum("validation_error" in r for r in records), 1)

    def test_repeated_fabricated_quotes_stop_before_phase_proposal(self):
        invalid = self.alignment.model_copy(deep=True)
        invalid.requirements[0].evidence[0].quote = "Fabricated approval"
        model = FakeModel(Alignment=[invalid, invalid, invalid])
        with closing(sqlite3.connect(":memory:", check_same_thread=False)) as conn:
            graph = planning.build_graph(model, SqliteSaver(conn), self.root)
            with self.assertRaises(app.GraphError):
                graph.invoke({"context": self.context}, {"configurable": {"thread_id": "reject"}})
        self.assertEqual(model.calls, ["Alignment"] * 3)
        self.assertEqual(len(list(self.root.glob("Alignment-*.json"))), 3)
        self.assertFalse((self.root / "planning-review.json").exists())


if __name__ == "__main__":
    unittest.main()
