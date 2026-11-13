#!/usr/bin/env python3
"""Small live lab probe: two directory PVCs, UID-10000 IO, pod recreation and cross-node DNS/HTTP.

Leaves its namespace, PVCs and data for inspection. It does not format devices,
delete PVCs/PVs, claim capacity enforcement, or validate CSI/physical disks.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubeconfig', default=os.environ.get('KUBECONFIG', '/etc/kubernetes/admin.conf'))
    parser.add_argument('--evidence-dir', type=Path, default=Path('/root/workspace/pulsar-deployment-evidence'))
    args = parser.parse_args()
    command = ['kubectl', '--kubeconfig=' + args.kubeconfig]

    def k(*parts, body=None):
        result = subprocess.run(command + list(parts), input=json.dumps(body) if body is not None else None,
                                capture_output=True, text=True, timeout=360)
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or result.stdout.strip())
        return result.stdout

    def get(kind, name=None, namespace=None):
        parts = ['get', kind] + ([name] if name else []) + (['-n', namespace] if namespace else [])
        return json.loads(k(*parts, '-o', 'json'))

    nodes = get('nodes')['items']
    expected = {'172.23.254.149': 'k8s-control-1', '172.23.254.150': 'iz6weeh0nfgmlsr99i8atmz'}
    selected = [n for n in nodes if n['metadata'].get('labels', {}).get('pulsar.songof.dev/two-node') == 'true']
    assert len(selected) == 2
    for address, name in expected.items():
        node = next(n for n in selected if n['metadata']['name'] == name)
        assert any(a['type'] == 'InternalIP' and a['address'] == address for a in node['status']['addresses'])
        assert any(c['type'] == 'Ready' and c['status'] == 'True' for c in node['status']['conditions'])
    sc = get('storageclass', 'pulsar-system-lab')
    assert sc['provisioner'] == 'pulsar.songof.dev/system-lab'
    assert sc['reclaimPolicy'] == 'Retain' and sc['volumeBindingMode'] == 'WaitForFirstConsumer'
    assert sc['metadata'].get('annotations', {}).get('storageclass.kubernetes.io/is-default-class') == 'false'
    stamp = datetime.now(timezone.utc).strftime('%Y%m%d-%H%M%S')
    ns = 'pulsar-system-storage-smoke-' + stamp
    run_id = uuid.uuid4().hex
    evidence = {'namespace': ns, 'runId': run_id, 'createdAt': datetime.now(timezone.utc).isoformat(),
                'storageClass': sc['metadata']['name'], 'passed': False, 'volumes': [],
                'limits': ['No disk initialization or CSI verification', 'Directory sizes are not quota enforced',
                           'PVC/PV deletion and released Retain lifecycle are not tested']}
    args.evidence_dir.mkdir(parents=True, exist_ok=True)
    out = args.evidence_dir / ('system-storage-smoke-' + stamp + '.json')
    k('create', '-f', '-', body={'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns,
      'labels': {'storage.pulsar.songof.dev/test': 'system-directory-smoke'}}})
    print('Created dedicated test namespace: ' + ns, flush=True)
    tolerations = [{'key': 'node-role.kubernetes.io/' + role, 'operator': 'Exists', 'effect': 'NoSchedule'}
                   for role in ['control-plane', 'master']]
    pods = {}
    try:
        for role, node_name in zip(['a', 'b'], expected.values()):
            name = 'probe-' + role
            marker = run_id + '-' + role
            k('create', '-f', '-', body={'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
              'metadata': {'name': name, 'namespace': ns}, 'spec': {'accessModes': ['ReadWriteOnce'],
              'storageClassName': 'pulsar-system-lab', 'resources': {'requests': {'storage': '1Mi'}}}})
            pod = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': ns, 'labels': {'app': name}},
              'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never',
                'nodeSelector': {'kubernetes.io/hostname': node_name, 'pulsar.songof.dev/two-node': 'true'},
                'tolerations': tolerations, 'securityContext': {'runAsUser': 10000, 'runAsGroup': 0,
                  'runAsNonRoot': True, 'fsGroup': 0, 'seccompProfile': {'type': 'RuntimeDefault'}},
                'containers': [{'name': 'probe', 'image': 'docker.io/library/busybox:1.37.0',
                  'command': ['sh', '-c', 'set -eu; test ! -e /data/marker; printf "%s\\n" "$MARKER" > /data/marker; sync; exec httpd -f -p 8080 -h /data'],
                  'env': [{'name': 'MARKER', 'value': marker}],
                  'ports': [{'containerPort': 8080, 'name': 'http'}],
                  'readinessProbe': {'httpGet': {'path': '/marker', 'port': 'http'}, 'periodSeconds': 2},
                  'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'}, 'limits': {'cpu': '100m', 'memory': '64Mi'}},
                  'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                    'capabilities': {'drop': ['ALL']}},
                  'volumeMounts': [{'name': 'data', 'mountPath': '/data'}]}],
                'volumes': [{'name': 'data', 'persistentVolumeClaim': {'claimName': name}}]}}
            pods[name] = pod
            k('create', '-f', '-', body=pod)
            k('create', '-f', '-', body={'apiVersion': 'v1', 'kind': 'Service',
              'metadata': {'name': name, 'namespace': ns}, 'spec': {'selector': {'app': name},
              'ports': [{'name': 'http', 'port': 8080, 'targetPort': 'http'}]}})
        k('wait', 'pods', '--all', '-n', ns, '--for=condition=Ready', '--timeout=300s')
        for role in ['a', 'b']:
            name = 'probe-' + role
            other = 'probe-' + ('b' if role == 'a' else 'a')
            pod = get('pod', name, ns)
            pvc = get('pvc', name, ns)
            assert pvc['status']['phase'] == 'Bound'
            pv = get('pv', pvc['spec']['volumeName'])
            assert pv['spec']['claimRef']['uid'] == pvc['metadata']['uid']
            assert pv['spec']['persistentVolumeReclaimPolicy'] == 'Retain'
            path = pv['spec']['hostPath']['path']
            assert path.startswith('/var/lib/pulsar-system-lab/pvc-')
            terms = pv['spec']['nodeAffinity']['required']['nodeSelectorTerms']
            assert any(any(x['key'] == 'kubernetes.io/hostname' and x['operator'] == 'In'
                           and x['values'] == [pod['spec']['nodeName']] for x in t.get('matchExpressions', []))
                       for t in terms)
            assert k('exec', '-n', ns, name, '--', 'id', '-u').strip() == '10000'
            assert k('exec', '-n', ns, name, '--', 'cat', '/data/marker').strip() == run_id + '-' + role
            k('exec', '-n', ns, name, '--', 'nslookup', 'kubernetes.default.svc.cluster.local')
            got = k('exec', '-n', ns, name, '--', 'wget', '-qO-', '-T', '10',
                    'http://' + other + '.' + ns + '.svc.cluster.local:8080/marker').strip()
            assert got == run_id + '-' + other[-1]
            evidence['volumes'].append({'node': pod['spec']['nodeName'], 'pvc': name, 'pvcUID': pvc['metadata']['uid'],
              'pv': pv['metadata']['name'], 'pvUID': pv['metadata']['uid'], 'hostPath': path,
              'oldPodUID': pod['metadata']['uid'], 'marker': run_id + '-' + role,
              'crossNodeDNSAndHTTP': True, 'pvNodeAffinityVerified': True,
              'retention': 'Bound PVC and host directory left in place'})
        for volume in evidence['volumes']:
            name = volume['pvc']
            current = get('pod', name, ns)
            assert current['metadata']['uid'] == volume['oldPodUID']
            k('delete', 'pod', name, '-n', ns, '--wait=true', '--timeout=60s')
            replacement = pods[name]
            replacement['spec']['containers'][0]['command'] = ['sh', '-c',
              'set -eu; test -f /data/marker; test "$(cat /data/marker)" = "$MARKER"; exec httpd -f -p 8080 -h /data']
            k('create', '-f', '-', body=replacement)
            k('wait', 'pod/' + name, '-n', ns, '--for=condition=Ready', '--timeout=120s')
            new_pod = get('pod', name, ns)
            new_pvc = get('pvc', name, ns)
            new_pv = get('pv', new_pvc['spec']['volumeName'])
            assert new_pod['metadata']['uid'] != volume['oldPodUID']
            assert new_pod['spec']['nodeName'] == volume['node']
            assert new_pvc['metadata']['uid'] == volume['pvcUID'] and new_pv['metadata']['uid'] == volume['pvUID']
            assert k('exec', '-n', ns, name, '--', 'cat', '/data/marker').strip() == volume['marker']
            volume.update(newPodUID=new_pod['metadata']['uid'], podRecreationPreservedData=True)
        evidence['passed'] = True
        evidence['verifiedAt'] = datetime.now(timezone.utc).isoformat()
        print('PASS: two node-specific PVCs, UID-10000 writes, cross-node DNS/HTTP, and preserved files after pod recreation.', flush=True)
    except Exception as error:
        evidence['error'] = str(error)
        raise
    finally:
        out.write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
        print('Evidence: ' + str(out), flush=True)
        print('Resources and small test data retained in namespace: ' + ns, flush=True)


if __name__ == '__main__':
    main()
