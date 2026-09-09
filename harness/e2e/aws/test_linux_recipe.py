import pathlib
import subprocess
import tempfile
import unittest

from linux_recipe import validate_shell


class LinuxRecipe(unittest.TestCase):
    def test_native_unterminated_heredoc_warning_is_rejected_despite_zero_exit(self):
        script = (pathlib.Path(__file__).parent/'testdata/linux-recipe-heredoc.sh').read_text()
        parsed = subprocess.run(['bash', '-n'], input=script, capture_output=True, text=True)
        self.assertEqual(parsed.returncode, 0)
        self.assertIn('here-document', parsed.stderr)
        with self.assertRaisesRegex(ValueError, 'parser warnings'):
            validate_shell(script)

    def test_validation_does_not_execute_preparation(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = pathlib.Path(directory)/'must-not-exist'
            validate_shell(f"touch '{marker}'\npython3 - <<'PYCODE'\nprint('valid')\nPYCODE\n")
            self.assertFalse(marker.exists())

    def test_shell_syntax_error_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'syntax errors'):
            validate_shell('if true; then\n')


if __name__ == '__main__':
    unittest.main()
