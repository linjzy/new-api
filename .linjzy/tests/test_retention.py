import importlib.util
import unittest
from pathlib import Path

BUNDLE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("retention", BUNDLE / "bin/retention.py")
retention = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)

VERSIONS = [
    {"id": 1, "created_at": "2026-09-08T06:00:00Z", "tags": ["verified-a-true-rc35-custom-new", "rc35-custom-new"]},
    {"id": 2, "created_at": "2026-09-08T05:00:00Z", "tags": ["verified-b-false-rc35-custom-unpromoted", "rc35-custom-unpromoted"]},
    {"id": 3, "created_at": "2026-09-08T03:00:00Z", "tags": ["rc35-custom-old", "candidate"]},
    {"id": 4, "created_at": "2026-09-07T04:00:00Z", "tags": []},
    {"id": 5, "created_at": "2026-09-06T04:00:00Z", "tags": ["rc34-custom-older", "previous"]},
]


class Retention(unittest.TestCase):
    def test_keeps_current_candidate_and_previous_only(self):
        keep_tags, delete = retention.plan(VERSIONS, "rc35-custom-new")
        self.assertEqual(keep_tags, {"rc35-custom-new", "rc35-custom-old", "rc34-custom-older"})
        self.assertEqual([v["id"] for v in delete], [2, 4])

    def test_promotion_moves_previous_and_drops_the_older_one(self):
        promoted = [dict(VERSIONS[0], tags=VERSIONS[0]["tags"] + ["candidate"]), VERSIONS[1],
                    dict(VERSIONS[2], tags=["rc35-custom-old", "previous"]), VERSIONS[3],
                    dict(VERSIONS[4], tags=["rc34-custom-older"])]
        keep_tags, delete = retention.plan(promoted, "rc35-custom-new")
        self.assertEqual(keep_tags, {"rc35-custom-new", "rc35-custom-old"})
        self.assertEqual([v["id"] for v in delete], [2, 4, 5])


if __name__ == "__main__":
    unittest.main()
