#!/usr/bin/env python3
"""Read-only preparation of SongOf Pulsar integration manifests; never applies them."""
import argparse
import json
import sys
import tempfile
from pathlib import Path

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from diskfleet.cluster import Cluster
from diskfleet.fleet import pulsar_plan, validate_two_node_targets, PULSAR_PROFILES, TWO_NODE_PROFILES, TWO_NODE_LABEL, SOURCE_REPOSITORY, SOURCE_REVISION
from diskfleet.kube import KubeClient
from diskfleet.store import Store


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pools-json',type=Path,help='Offline reference only: pools list (or {items:[...]}), without live Node checks; otherwise reads Kubernetes')
    parser.add_argument('--zookeeper-pools',required=True,help='Comma-separated DiskPool names')
    parser.add_argument('--journal-pools',required=True)
    parser.add_argument('--ledger-pools',required=True)
    parser.add_argument('--namespace',required=True)
    parser.add_argument('--name',default='diskfleet-pulsar')
    parser.add_argument('--replicas',type=int,default=3)
    parser.add_argument('--profile',choices=PULSAR_PROFILES,default='three-node-e2e',
        help='three-node-e2e: Required anti-affinity (default); two-node-lab: separate journal/ledger disks on 2 nodes; two-node-shared-disk-lab: all 3 roles share one data disk per node (>=94Gi free each). Both two-node profiles use Preferred and are not host-failure HA')
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    try:
        if args.pools_json:
            pools=json.loads(args.pools_json.read_text())
            if isinstance(pools,dict): pools=pools['items']
        else:
            kube=KubeClient()
            kube.request('GET','/apis/pulsar.songof.dev/v1alpha1')
            with tempfile.TemporaryDirectory(prefix='diskfleet-pulsar-plan-') as tmp:
                pools=Cluster(Store(tmp),kube=kube).read()['pools']
        assignments={role:[x.strip() for x in getattr(args,role+'_pools').split(',') if x.strip()] for role in ('zookeeper','journal','ledger')}
        manifest=pulsar_plan(pools,assignments,args.namespace,args.name,args.replicas,profile=args.profile)
        if args.profile in TWO_NODE_PROFILES and not args.pools_json:
            nodes=kube.list('/api/v1/nodes',label_selector=TWO_NODE_LABEL+'=true')
            validate_two_node_targets(pools,assignments,nodes,profile=args.profile)
        if args.output.exists():
            parser.error('输出文件已存在；选择新路径，避免覆盖已经审核的计划')
        args.output.parent.mkdir(parents=True,exist_ok=True)
        args.output.write_text(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n')
        if args.profile in TWO_NODE_PROFILES:
            if args.profile == 'two-node-shared-disk-lab':
                print('two-node-shared-disk-lab：3 ZooKeeper + 3 Bookie 使用 Preferred 反亲和，不能保证 2+1 分布；非主机故障高可用，不适用原三节点 E2E。三个角色共用每节点同一块数据盘，没有物理盘或 I/O 隔离；每个池须至少 94Gi 实际可用空间。',file=sys.stderr)
            else:
                print('two-node-lab：3 ZooKeeper + 3 Bookie 使用 Preferred 反亲和，不能保证 2+1 分布；非主机故障高可用，不适用原三节点 E2E。每个共用 ZK/journal 池须至少 30Gi 可用，每个 ledger 池须至少 64Gi 可用。',file=sys.stderr)
            if args.pools_json:
                print('离线输入仅供参考：未验证实时节点标签、Ready/封锁状态或真实存储池身份，不能作为已完成部署预检的证据。',file=sys.stderr)
        print(json.dumps(dict(output=str(args.output),source=SOURCE_REPOSITORY,revision=SOURCE_REVISION,
            mode='offline-input' if args.pools_json else 'live-read-only',resources=len(manifest['items']),
            namespace=args.namespace,cluster=args.name,profile=args.profile,applied=False),ensure_ascii=False))
    except Exception as error:
        parser.exit(1,'准备失败：'+str(error)+'\n')


if __name__=='__main__': main()
