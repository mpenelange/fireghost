import ast
import re
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
DEV = ROOT / "dev"
OLD_DIRECTORY = "de" + "ploy"


def without_mapping_key_literals(text):
    """Exclude exact configuration keys, retaining all other path references."""
    try:
        tree = ast.parse(text)
    except (SyntaxError, ValueError):
        return text  # Keep the original conservative scan for invalid source.
    source = bytearray(text.encode("utf-8"))
    # AST columns count UTF-8 bytes, rather than Unicode characters.
    line_starts = [0] + [match.end() for match in re.finditer(b"\n", source)]
    for node in ast.walk(tree):
        if isinstance(node, ast.Subscript):
            keys = [node.slice]
        elif isinstance(node, ast.Dict):
            keys = node.keys
        else:
            continue
        for key in keys:
            if isinstance(key, ast.Constant) and key.value == OLD_DIRECTORY:
                start = line_starts[key.lineno - 1] + key.col_offset
                end = line_starts[key.end_lineno - 1] + key.end_col_offset
                source[start:end] = b" " * (end - start)
    return source.decode("utf-8")


class RepositoryLayoutTest(unittest.TestCase):
    def check_tracked_source(self, text, relative="example.py"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / relative
            source.parent.mkdir(parents=True, exist_ok=True)
            source.write_text(text)
            tracked = mock.Mock(stdout=(relative + "\0").encode())
            with mock.patch.dict(globals(), {"ROOT": root}), mock.patch("subprocess.run", return_value=tracked):
                self.test_tracked_files_do_not_reference_the_old_directory()

    def test_mapping_keys_do_not_count_as_old_directory_paths(self):
        for source in [
            f'limits = pipeline["{OLD_DIRECTORY}"]\n',
            f'limits = {{"{OLD_DIRECTORY}": {{"resources": {{}}}}}}\n',
            f'caption = "雪"; limits = pipeline["{OLD_DIRECTORY}"]\n',
        ]:
            with self.subTest(source=source):
                self.check_tracked_source(source)

    def test_actual_old_directory_paths_remain_blocked_next_to_mapping_keys(self):
        for reference in [
            f'ROOT / "{OLD_DIRECTORY}"',
            f'Path("{OLD_DIRECTORY}")',
            f'"{OLD_DIRECTORY}/compose.yaml"',
            f'{{"base": "{OLD_DIRECTORY}"}}',
            f'{{"{OLD_DIRECTORY}/compose.yaml": "contents"}}',
        ]:
            with self.subTest(reference=reference):
                source = f'limits = pipeline["{OLD_DIRECTORY}"]; path = {reference}\n'
                with self.assertRaises(AssertionError):
                    self.check_tracked_source(source)

    def test_old_directory_tracked_prefix_remains_blocked(self):
        with self.assertRaises(AssertionError):
            self.check_tracked_source("value = 1\n", OLD_DIRECTORY + "/example.py")

    def test_developer_appliance_has_an_unambiguous_home(self):
        self.assertTrue((DEV / "compose.yaml").is_file())
        self.assertTrue((DEV / "compose.staging.yaml").is_file())
        self.assertTrue((DEV / ".env.example").is_file())
        self.assertTrue((DEV / "stack.lock.json").is_file())
        self.assertFalse((ROOT / OLD_DIRECTORY).exists())

    def test_make_uses_the_developer_directory_as_compose_project_directory(self):
        makefile = (ROOT / "Makefile").read_text()
        expected = "COMPOSE = docker compose --project-directory dev -f dev/compose.yaml"
        self.assertIn(expected, makefile)

    def test_developer_appliance_reuses_public_crw_configuration(self):
        compose = (DEV / "compose.yaml").read_text()
        self.assertIn(
            "../deployment/crw.toml:/app/config/crw.toml:ro",
            compose,
        )
        self.assertFalse((DEV / "config" / "crw.toml").exists())

    def test_tracked_files_do_not_reference_the_old_directory(self):
        tracked = subprocess.run(
            ["git", "ls-files", "-z"],
            cwd=ROOT,
            check=True,
            capture_output=True,
        ).stdout.decode().split("\0")
        old_prefix = OLD_DIRECTORY + "/"
        offenders = []
        for relative in filter(None, tracked):
            if relative.startswith(old_prefix):
                offenders.append(relative)
                continue
            path = ROOT / relative
            try:
                text = path.read_text()
            except UnicodeDecodeError:
                continue
            if path.suffix == ".py":
                text = without_mapping_key_literals(text)
            old_reference = r"(?<![A-Za-z])" + re.escape(OLD_DIRECTORY) + r"(?:/|[\"'])"
            if re.search(old_reference, text):
                offenders.append(relative)
        self.assertEqual([], offenders)


if __name__ == "__main__":
    unittest.main()
