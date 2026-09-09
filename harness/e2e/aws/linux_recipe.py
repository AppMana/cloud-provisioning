"""Validate image-builder shell without executing its root-level operations."""
import subprocess


def validate_shell(script):
    result = subprocess.run(['bash', '-n'], input=script, capture_output=True, text=True)
    # Bash exits zero for an unterminated heredoc. Accepting that warning let
    # embedded Python consume subsequent shell during a native image build.
    if result.returncode or result.stderr.strip():
        raise ValueError('Image recipe has shell syntax errors or parser warnings; inspect it locally with bash -n')
