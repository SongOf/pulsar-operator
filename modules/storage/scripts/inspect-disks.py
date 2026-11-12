#!/usr/bin/env python3
"""Read-only inventory. No enrollment, formatting, mounts or LVM writes."""
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from diskfleet.storage import discover_disks

if __name__ == "__main__":
    print(json.dumps(discover_disks(), indent=2, ensure_ascii=False))
