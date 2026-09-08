import importlib.util
import unittest
from pathlib import Path

BUNDLE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("retention", BUNDLE / "bin/retention.py")
retention = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)

VERSIONS = [
    {"id": 1, "created_at": "2026-09-08T05:00:00Z", "tags": ["verified-a-false-rc35-custom-new", "rc35-custom-new"]},
    {"id": 2, "created_at": "2026-09-08T03:00:00Z", "tags": ["verified-b-true-rc35-custom-old", "rc35-custom-old", "candidate"]},
    {"id": 3, "created_at": "2026-09-07T21:00:00Z", "tags": ["rc35-autorefresh-legacy"]},
    {"id": 4, "created_at": "2026-09-07T04:00:00Z", "tags": []},
    {"id": 5, "created_at": "2026-09-06T04:00:00Z", "tags": ["rc34-custom-older"]},
]


class Retention(unittest.TestCase):
    def test_keeps_current_candidate_and_newest_other_image(self):
        keep_tags, delete = retention.plan(VERSIONS, "rc35-custom-new")
        self.assertEqual(keep_tags, {"rc35-custom-new", "rc35-custom-old", "rc35-autorefresh-legacy"})
        self.assertEqual([v["id"] for v in delete], [4, 5])

    def test_promoted_current_keeps_one_previous_image(self):
        promoted = [dict(VERSIONS[0], tags=VERSIONS[0]["tags"] + ["candidate"]),
                    dict(VERSIONS[1], tags=VERSIONS[1]["tags"][:-1])] + VERSIONS[2:]
        keep_tags, delete = retention.plan(promoted, "rc35-custom-new")
        self.assertEqual(keep_tags, {"rc35-custom-new", "rc35-custom-old"})
        self.assertEqual([v["id"] for v in delete], [3, 4, 5])

    def test_untagged_versions_never_count_as_previous(self):
        keep_tags, delete = retention.plan(VERSIONS[:2] + VERSIONS[3:4], "rc35-custom-new")
        self.assertEqual(keep_tags, {"rc35-custom-new", "rc35-custom-old"})
        self.assertEqual([v["id"] for v in delete], [4])


if __name__ == "__main__":
    unittest.main()
