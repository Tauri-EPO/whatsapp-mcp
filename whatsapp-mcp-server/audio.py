import os
import subprocess
import tempfile
import threading

DEFAULT_FFMPEG_TIMEOUT_S = 120


def ffmpeg_timeout_s() -> int:
    """Seconds an ffmpeg conversion may take (FFMPEG_TIMEOUT_S, default 120)."""
    raw = os.getenv("FFMPEG_TIMEOUT_S", "").strip()
    try:
        value = int(raw) if raw else DEFAULT_FFMPEG_TIMEOUT_S
    except ValueError:
        return DEFAULT_FFMPEG_TIMEOUT_S
    return value if value > 0 else DEFAULT_FFMPEG_TIMEOUT_S


def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000, write_chunk=None):
    """
    Convert an audio file to Opus format in an Ogg container.

    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
        write_chunk: Optional quota writer (file handle, encoded bytes). When
                     supplied, ffmpeg output is piped through it under the same deadline.

    Returns:
        str: Path to the converted file

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")

    # If no output file is specified, replace the extension with .ogg
    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"

    # Ensure the output directory exists
    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir)

    # Build the ffmpeg command
    cmd = [
        "ffmpeg",
        "-i",
        input_file,
        "-c:a",
        "libopus",
        "-b:a",
        bitrate,
        "-ar",
        str(sample_rate),
        "-application",
        "voip",  # Optimize for voice
        "-vbr",
        "on",  # Variable bitrate
        "-compression_level",
        "10",  # Maximum compression
        "-frame_duration",
        "60",  # 60ms frames (good for voice)
        "-y",  # Overwrite output file if it exists
        output_file,
    ]

    timeout = ffmpeg_timeout_s()
    if write_chunk is not None:
        # Pipe encoded bytes through the caller's storage quota. Letting ffmpeg
        # write directly would bypass the bounds shared with incoming uploads.
        cmd[-1:] = ["-f", "ogg", "pipe:1"]
        with tempfile.TemporaryFile() as diagnostics, open(output_file, "wb", buffering=0) as target:
            with subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=diagnostics) as process:
                expired = threading.Event()

                def stop():
                    expired.set()
                    try:
                        process.kill()
                    except OSError:
                        pass

                timer = threading.Timer(timeout, stop)
                timer.daemon = True
                timer.start()
                try:
                    assert process.stdout is not None
                    while chunk := process.stdout.read(64 * 1024):
                        write_chunk(target, chunk)
                    result = process.wait()
                    if expired.is_set():
                        raise RuntimeError(f"ffmpeg timed out after {timeout}s converting {input_file}")
                    if result:
                        size = diagnostics.seek(0, os.SEEK_END)
                        diagnostics.seek(max(0, size - 8192))
                        detail = diagnostics.read(8192).decode("utf-8", errors="replace")
                        raise RuntimeError(f"Failed to convert audio. You likely need to install ffmpeg {detail}")
                finally:
                    timer.cancel()
                    if process.poll() is None:
                        process.kill()
                        process.wait()
        return output_file
    try:
        # Run the ffmpeg command and capture output
        subprocess.run(cmd, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True, timeout=timeout)
        return output_file
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"ffmpeg timed out after {timeout}s converting {input_file}") from None
    except subprocess.CalledProcessError as e:
        raise RuntimeError(f"Failed to convert audio. You likely need to install ffmpeg {e.stderr}")


def convert_to_opus_ogg_temp(input_file, bitrate="32k", sample_rate=24000, directory=None, write_chunk=None):
    """
    Convert an audio file to Opus format in an Ogg container and store in a temporary file.

    Args:
        input_file (str): Path to the input audio file
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
        directory (str, optional): Where the file is created (made if missing);
                                   the system temp directory when None. The bridge
                                   only reads inside WHATSAPP_MEDIA_ROOTS, so send
                                   paths pass the outbox here.
        write_chunk: Optional quota writer forwarded to convert_to_opus_ogg.

    Returns:
        str: Path to the temporary file with the converted audio

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    # Create a temporary file with .ogg extension
    if directory:
        os.makedirs(directory, exist_ok=True)
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False, dir=directory)
    temp_file.close()

    try:
        # Convert the audio
        convert_to_opus_ogg(input_file, temp_file.name, bitrate, sample_rate, write_chunk=write_chunk)
        return temp_file.name
    except Exception as e:
        # Clean up the temporary file if conversion fails
        if os.path.exists(temp_file.name):
            os.unlink(temp_file.name)
        raise e


if __name__ == "__main__":
    # Example usage
    import sys

    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)

    input_file = sys.argv[1]

    try:
        result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
