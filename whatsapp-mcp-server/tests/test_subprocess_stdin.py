"""Audio children must not read the stdio MCP protocol channel (#605)."""

import ast
import inspect
import shutil
import subprocess
import sys
import wave
from pathlib import Path

import pytest

import audio
import transcribe


@pytest.mark.parametrize("module", [audio, transcribe])
def test_every_audio_child_disconnects_protocol_stdin(module):
    calls = [
        node
        for node in ast.walk(ast.parse(inspect.getsource(module)))
        if isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and isinstance(node.func.value, ast.Name)
        and node.func.value.id == "subprocess"
        and node.func.attr in {"run", "Popen"}
    ]
    assert calls, "The module scan did not find its child processes"
    for call in calls:
        stdin = next((keyword.value for keyword in call.keywords if keyword.arg == "stdin"), None)
        assert isinstance(stdin, ast.Attribute) and ast.unparse(stdin) == "subprocess.DEVNULL", call.lineno


@pytest.mark.parametrize("mode", ["run", "pipe", "wav"])
def test_real_conversion_leaves_parent_protocol_bytes_unread(tmp_path, mode):
    if not shutil.which("ffmpeg"):
        pytest.skip("Real ffmpeg is exercised in the shipped-image gate")
    source = tmp_path / "source.wav"
    output = tmp_path / ("output.wav" if mode == "wav" else "output.ogg")
    with wave.open(str(source), "wb") as writer:
        writer.setnchannels(1)
        writer.setsampwidth(2)
        writer.setframerate(16000)
        writer.writeframes(bytes(2 * 16000 * 2))
    # Slow the real child to input time so its interactive reader gets a turn;
    # an unthrottled tiny conversion can finish before it ever checks stdin.
    program = """
import sys
from pathlib import Path
import audio, transcribe
source, output, mode = sys.argv[1:]
native = audio.subprocess.Popen if mode == 'pipe' else audio.subprocess.run
def realtime(command, *args, **kwargs):
    return native([command[0], '-re', *command[1:]], *args, **kwargs)
if mode == 'pipe':
    audio.subprocess.Popen = realtime
    audio.convert_to_opus_ogg(source, output, write_chunk=lambda handle, chunk: handle.write(chunk))
elif mode == 'wav':
    audio.subprocess.run = realtime
    transcribe.convert_to_wav16k(source, output)
else:
    audio.subprocess.run = realtime
    audio.convert_to_opus_ogg(source, output)
assert Path(output).stat().st_size > 0
sys.stdout.buffer.write(sys.stdin.buffer.read())
"""
    protocol = b"q\n" + b"x" * 3998
    result = subprocess.run(
        [sys.executable, "-c", program, str(source), str(output), mode],
        cwd=Path(audio.__file__).parent,
        input=protocol,
        capture_output=True,
        timeout=20,
    )
    assert result.returncode == 0, result.stderr.decode(errors="replace")
    assert result.stdout == protocol, f"Child consumed {len(protocol) - len(result.stdout)} protocol bytes"
