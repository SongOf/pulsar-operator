#!/usr/bin/env python3
"""Repository entry point; the storage module owns the integration contract."""
import runpy
from pathlib import Path

if __name__ == '__main__':
    runpy.run_path(str(Path(__file__).resolve().parents[1] /
                       'modules/storage/scripts/prepare-pulsar.py'), run_name='__main__')
