"""Keep expensive optional media parsers out of text-only server startup."""

import subprocess
import sys


def test_startup_does_not_load_optional_media_decoders():
    subprocess.run(
        [
            sys.executable,
            "-c",
            "import main,sys; assert not {'pypdfium2','openpyxl','docx'}.intersection(sys.modules)",
        ],
        check=True,
    )
