#!/usr/bin/env python3
"""docs/releases/v*.md are the GitHub Release bodies (docker.yml, job release).

Standard library only; reads files of the working tree, nothing else.
"""
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[3]
NOTES = sorted((ROOT / "docs" / "releases").glob("v*.md"))
REPO = "https://github.com/mutex-solutions/proxcloud/"
LINK = re.compile(r"\]\(([^)\s]+)\)")
BLOB = re.compile(r"https://github\.com/mutex-solutions/proxcloud/blob/([^/\s)]+)/([^\s)#]+)")
# A line that starts a block of its own: heading, list item, table row, blockquote.
BLOCK_START = re.compile(r"^\s*(#{1,6} |[-*+] |\d+\. |\||>)")
MAX_BODY = 120000  # the limit docker.yml enforces; GitHub's own is 125000


def wrapped_lines(text):
    """Line numbers that continue the paragraph or list item of the line before.

    A Release body renders every newline as a line break, so a paragraph or a
    list item must be one source line.
    """
    found, in_fence, previous_is_text = [], False, False
    for number, line in enumerate(text.splitlines(), 1):
        if line.strip().startswith("```"):
            in_fence, previous_is_text = not in_fence, False
            continue
        if in_fence or not line.strip():
            previous_is_text = False
            continue
        starts_block = bool(BLOCK_START.match(line))
        if previous_is_text and not starts_block:
            found.append(number)
        # A table row or a heading is never continued by a text line in these files.
        previous_is_text = not line.lstrip().startswith(("|", "#"))
    return found


class ReleaseNotes(unittest.TestCase):
    def test_there_are_release_notes(self):
        self.assertTrue(NOTES, "docs/releases holds no v*.md")

    def test_each_release_has_notes(self):
        """Without the file docker.yml falls back to the hard-wrapped changelog section."""
        changelog = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
        have = {path.stem[1:] for path in NOTES}
        for version in re.findall(r"(?m)^## \[(\d+\.\d+\.\d+)\]", changelog):
            if tuple(map(int, version.split("."))) >= (0, 8, 0):
                self.assertIn(version, have, f"docs/releases/v{version}.md is missing")

    def test_wrapped_lines_are_found(self):
        self.assertEqual(wrapped_lines("a\nb\n\n- c\n  d\n\n```\nx\ny\n```\n| t |\n| u |\n# h\n\n1. e\n2. f\n"), [2, 5])

    def test_each_file(self):
        changelog = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
        for path in NOTES:
            with self.subTest(path.name):
                version = path.stem[1:]
                text = path.read_text(encoding="utf-8")
                self.assertRegex(changelog, rf"(?m)^## \[{re.escape(version)}\]", "no CHANGELOG.md section")
                self.assertTrue(text.strip())
                self.assertLessEqual(len(text), MAX_BODY)
                for section in ("## Highlights", "## Upgrade"):
                    self.assertIn(f"\n{section}\n", text)
                self.assertEqual(wrapped_lines(text), [], "hard-wrapped lines: one paragraph or list item per line")
                for target in LINK.findall(text):
                    # A Release body has no relative base, and only blob/<tag>/ pins the release.
                    self.assertTrue(target.startswith("https://"), f"{target}: not an absolute link")
                    if target.startswith(REPO):
                        self.assertRegex(target[len(REPO):], r"^(blob|releases/tag)/", f"{target}: not a blob or release link")
                for tag, relative in BLOB.findall(text):
                    self.assertEqual(tag, path.stem, f"{relative}: link to another tag")
                    self.assertTrue((ROOT / relative).exists(), f"{relative} is not in the tree")


if __name__ == "__main__":
    unittest.main(verbosity=2)
