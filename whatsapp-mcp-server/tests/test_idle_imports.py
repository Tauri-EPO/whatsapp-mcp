"""Keep expensive optional media parsers out of text-only server startup."""

import os
import subprocess
import sys
from pathlib import Path

import main


def test_startup_does_not_load_optional_media_decoders():
    subprocess.run(
        [
            sys.executable,
            "-c",
            "import main,sys; assert not {'pypdfium2','openpyxl','docx'}.intersection(sys.modules)",
        ],
        cwd=Path(main.__file__).parent,
        env={
            **{key: os.environ[key] for key in ("PATH", "SYSTEMROOT", "WINDIR") if key in os.environ},
            "WHATSAPP_MCP_TRANSPORT": "stdio",
            "WHATSAPP_PARENT_WATCHDOG_S": "0",
            "TRANSCRIBE_ON_INGEST": "0",
            "WHATSAPP_API_URL": "http://127.0.0.1:1/api",
        },
        check=True,
        timeout=20,
    )
