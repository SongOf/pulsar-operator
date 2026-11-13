"""Test fixture for bounded fake kubectl/curl processes; not a GNU timeout emulator."""
import subprocess
import sys

if __name__ == "__main__":
    duration = sys.argv[1]
    seconds = float(duration[:-1] if duration.endswith("s") else duration)
    try:
        result = subprocess.run(sys.argv[2:], timeout=seconds)
        sys.exit(result.returncode)
    except subprocess.TimeoutExpired:
        sys.exit(124)
