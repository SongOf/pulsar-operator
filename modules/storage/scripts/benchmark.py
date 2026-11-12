#!/usr/bin/env python3
"""Local metadata benchmark, NOT a Kubernetes or block-storage benchmark."""
import argparse
import json
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from diskfleet.store import Store, GiB

parser=argparse.ArgumentParser()
parser.add_argument('--disks',type=int,default=10000)
args=parser.parse_args()
if not 1<=args.disks<=1000000: parser.error('--disks must be 1..1000000')
with tempfile.TemporaryDirectory(prefix='diskfleet-bench-') as tmp:
    store=Store(tmp)
    start=time.perf_counter()
    with store.transaction() as con:
        for i in range(args.disks):
            store.put(con,'disks',dict(id=f'disk-{i:08}',name=f'disk-{i:08}',node=f'node-{i//12:05}',state='Available',serial=f'BENCH-{i}',sizeBytes=1920*GiB))
    inserted=time.perf_counter()-start
    times=[]
    for i in range(100):
        start=time.perf_counter()
        page=store.list('disks',limit=50,offset=(i*50)%args.disks)
        assert page['total']==args.disks
        times.append((time.perf_counter()-start)*1000)
    print(json.dumps(dict(scope='SQLite metadata only; not real disk or Kubernetes performance',disks=args.disks,insertSeconds=round(inserted,3),pageMedianMs=round(sorted(times)[50],3),pageP95Ms=round(sorted(times)[95],3)),indent=2))
