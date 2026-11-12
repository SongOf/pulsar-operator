import json
import io
import tempfile
import threading
import unittest
from unittest.mock import patch
from concurrent.futures import ThreadPoolExecutor
from http.server import ThreadingHTTPServer
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from diskfleet.api import App, handler
from diskfleet.store import GiB, Problem, Store


class StoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.store=Store(self.tmp.name)
        self.store.seed()
        self.pool=self.store.list('pools')['items'][0]

    def tearDown(self): self.tmp.cleanup()

    def test_seed_is_idempotent(self):
        self.store.seed()
        self.assertEqual(144,self.store.list('disks')['total'])
        self.assertEqual(6,self.store.list('pools')['total'])

    def test_paging_and_filter(self):
        page=self.store.list('disks',limit=3,offset=3,node='baremetal-001')
        self.assertEqual(6,page['total'])
        self.assertEqual(3,len(page['items']))
        self.assertEqual(0,self.store.list('disks',q='%')['total'])

    def test_plans_revalidate_identity_and_reject_system_disk(self):
        with self.assertRaises(Problem): self.store.plan(dict(name='bad',node='baremetal-001',diskIds=['disk-001-0']))
        plan=self.store.plan(dict(name='test-pool',node='baremetal-001',diskIds=['disk-001-2']))
        with self.store.transaction() as con:
            disk=self.store.get(con,'disks','disk-001-2'); disk['serial']='REPLACED'
            self.store.put(con,'disks',disk)
        with self.assertRaises(Problem): self.store.create_pool(plan['planId'],plan['confirmation'])

    def test_double_commit_and_expired_plans(self):
        plan=self.store.plan(dict(name='test-pool',node='baremetal-001',diskIds=['disk-001-2']))
        a=self.store.create_pool(plan['planId'],plan['confirmation'])
        b=self.store.create_pool(plan['planId'],plan['confirmation'])
        self.assertEqual(a,b)
        with self.store.transaction() as con: con.execute('UPDATE plans SET expires=0')
        with self.assertRaises(Problem): self.store.create_pool(plan['planId'],plan['confirmation'])

    def test_concurrent_allocation_never_overcommits(self):
        capacity=self.pool['availableBytes']
        def allocate(i):
            try: return self.store.create_volume(dict(name='race-'+str(i),poolId=self.pool['id'],sizeGiB=200))
            except Problem as error:
                self.assertEqual(409,error.status)
                return None
        with ThreadPoolExecutor(max_workers=16) as executor: result=list(executor.map(allocate,range(30)))
        accepted=sum(x is not None for x in result)
        self.assertEqual(capacity//(200*GiB),accepted)
        p=self.store.list('pools')['items'][0]
        self.assertGreaterEqual(p['availableBytes'],0)
        self.assertEqual(p['capacityBytes'],p['availableBytes']+p['allocatedBytes'])

    def test_duplicate_pvc_idempotent_and_conflict(self):
        body=dict(name='duplicate',namespace='test',poolId=self.pool['id'],sizeGiB=5)
        a=self.store.create_volume(body); b=self.store.create_volume(body)
        self.assertEqual(a['id'],b['id'])
        with self.assertRaises(Problem): self.store.create_volume(dict(body,sizeGiB=6))

    def test_expand_retain_and_restart_preserve_data_and_capacity(self):
        v=self.store.create_volume(dict(name='persist',poolId=self.pool['id'],sizeGiB=2))
        file=Path(v['mountPath'])/'sentinel.txt'; file.write_text('diskfleet-sentinel')
        self.store.expand(v['id'],dict(sizeGiB=3))
        with self.assertRaises(Problem): self.store.expand(v['id'],dict(sizeGiB=1))
        before=self.store.list('pools')['items'][0]['availableBytes']
        self.store.release(v['id'])
        restarted=Store(self.tmp.name)
        self.assertEqual(before,restarted.list('pools')['items'][0]['availableBytes'])
        self.assertEqual('diskfleet-sentinel',file.read_text())
        self.assertEqual('Released',next(x for x in restarted.list('volumes')['items'] if x['id']==v['id'])['status'])

    def test_node_totals_follow_pool(self):
        n=self.store.list('nodes',node=self.pool['node'])['items'][0]
        self.assertEqual(self.pool['allocatedBytes'],n['allocatedBytes'])


class ApiTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp=tempfile.TemporaryDirectory()
        cls.app=App(state_dir=cls.tmp.name,token='a-test-token-at-least-24-characters')

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def request(self,path,method='GET',body=None,auth=True):
        headers={'Authorization':'Bearer '+self.app.token} if auth else {}
        if body is not None: headers['Content-Type']='application/json'
        payload=json.dumps(body).encode() if body is not None else b''
        headers['Content-Length']=str(len(payload))
        raw=(method+' '+path+' HTTP/1.0\r\nHost: localhost\r\n'+''.join(k+': '+v+'\r\n' for k,v in headers.items())+'\r\n').encode()+payload
        return self.raw_request(raw)

    def raw_request(self,raw):
        # Exercise the actual HTTP parser, headers, handler and serializer without
        # requiring a listening socket (the execution sandbox disallows sockets).
        class Wire:
            def __init__(self): self.output=io.BytesIO()
            def makefile(self,*args,**kwargs): return io.BytesIO(raw)
            def sendall(self,data): self.output.write(data)
            def settimeout(self,*args): pass
        wire=Wire()
        handler(self.app)(wire,('127.0.0.1',12345),None)
        head,data=wire.output.getvalue().split(b'\r\n\r\n',1)
        return int(head.split()[1]),json.loads(data)

    def post_bytes(self,payload,extra_headers=b'',length=None):
        length=len(payload) if length is None else length
        raw=(b'POST /api/volumes HTTP/1.0\r\nHost: localhost\r\nAuthorization: Bearer '
             +self.app.token.encode()+b'\r\nContent-Type: application/json\r\nContent-Length: '
             +str(length).encode()+b'\r\n'+extra_headers+b'\r\n'+payload)
        return self.raw_request(raw)

    def test_public_config_and_protected_api(self):
        status,data=self.request('/api/config',auth=False)
        self.assertEqual(200,status); self.assertTrue(data['tokenRequired'])
        self.assertEqual(401,self.request('/api/disks',auth=False)[0])
        self.assertEqual(200,self.request('/api/disks')[0])

    def test_invalid_pagination_body_and_unknown_routes(self):
        for query in ('limit=0','limit=501','offset=-1','limit=abc'):
            self.assertEqual(400,self.request('/api/disks?'+query)[0])
        self.assertEqual(400,self.request('/api/volumes','POST',[])[0])
        self.assertEqual(404,self.request('/api/no-such-route')[0])

    def test_http_plan_commit_volume_lifecycle(self):
        status,plan=self.request('/api/pools/plan','POST',dict(name='http-pool',node='baremetal-020',diskIds=['disk-020-1']))
        self.assertEqual(200,status)
        self.assertEqual(409,self.request('/api/pools','POST',dict(planId=plan['planId'],confirmation='wrong'))[0])
        status,pool=self.request('/api/pools','POST',dict(planId=plan['planId'],confirmation=plan['confirmation']))
        self.assertEqual(200,status)
        status,vol=self.request('/api/volumes','POST',dict(name='http-pvc',namespace='default',poolId=pool['id'],sizeGiB=2))
        self.assertEqual('Bound',vol['status'])
        self.assertEqual(200,self.request('/api/volumes/'+vol['id']+'/expand','POST',dict(sizeGiB=3))[0])
        self.assertEqual('Released',self.request('/api/volumes/'+vol['id'],'DELETE')[1]['status'])

    def test_cluster_requires_token(self):
        with self.assertRaises(ValueError): App(mode='cluster',state_dir=self.tmp.name,token='')

    def test_cluster_delete_passes_one_explicit_pvc_uid_to_backend(self):
        with patch.object(self.app,'mode','cluster'), patch.object(self.app.backend,'release',return_value={'status':'Released'}) as release:
            status,_=self.request('/api/volumes/default~data?uid=pvc-uid','DELETE')
            self.assertEqual(status,200)
            release.assert_called_once_with('default~data',expected_uid='pvc-uid')
            release.reset_mock()
            self.assertEqual(self.request('/api/volumes/default~data?uid=one&uid=two','DELETE')[0],400)
            self.assertEqual(self.request('/api/volumes/default~data?uid=bad%2Fuid','DELETE')[0],400)
            release.assert_not_called()

    def test_malformed_identifiers_are_client_errors(self):
        for value in (None,[],{},True,24,'../outside','x/y','x\x00y'):
            with self.subTest(value=value):
                self.assertEqual(400,self.request('/api/pools','POST',dict(planId=value,confirmation='接管 TEST'))[0])
                self.assertEqual(400,self.request('/api/volumes','POST',dict(name='bad-type',sizeGiB=1,poolId=value))[0])
        self.assertEqual(404,self.request('/api/volumes/abc/extra/expand','POST',dict(sizeGiB=2))[0])
        self.assertEqual(400,self.request('/api/volumes/a%2Fb','DELETE')[0])

    def test_reject_ambiguous_or_truncated_http_framing(self):
        self.assertEqual(400,self.post_bytes(b'{}',b'Content-Length: 2\r\n')[0])
        self.assertEqual(400,self.post_bytes(b'{}',b'Transfer-Encoding: chunked\r\n')[0])
        self.assertEqual(400,self.post_bytes(b'{}',length=12)[0])
        self.assertEqual(400,self.post_bytes(b'{}',length='+2')[0])
        self.assertEqual(413,self.post_bytes(b'{}',length=65537)[0])
        self.assertEqual(400,self.post_bytes(b'{}',b'Authorization: Bearer second-token\r\n')[0])

    def test_invalid_json_is_400_not_internal_error(self):
        for payload in (b'{',b'{"name":"a","name":"b"}',b'{"sizeGiB":NaN}',b'{"a":'+b'['*1200+b']'*1200+b'}'):
            self.assertEqual(400,self.post_bytes(payload)[0])

    def test_non_ascii_auth_is_unauthorized_not_internal_error(self):
        raw=b'GET /api/disks HTTP/1.0\r\nHost: localhost\r\nAuthorization: Bearer \xff\r\n\r\n'
        self.assertEqual(401,self.raw_request(raw)[0])

    def test_static_path_traversal_and_query_limit(self):
        self.assertEqual(404,self.raw_request(b'GET /static/../../store.py HTTP/1.0\r\nHost: localhost\r\n\r\n')[0])
        self.assertEqual(400,self.request('/api/disks?'+'&'.join('q=x' for _ in range(101)))[0])

    def test_cli_reports_socket_permission_failure(self):
        from diskfleet.api import main
        stderr=io.StringIO()
        with patch('sys.argv',['diskfleet','--state-dir',self.tmp.name]), patch('diskfleet.api.ThreadingHTTPServer',side_effect=PermissionError('socket denied')), patch('sys.stderr',stderr):
            with self.assertRaises(SystemExit) as result: main()
        self.assertEqual(2,result.exception.code)
        self.assertIn('socket denied',stderr.getvalue())
        self.assertNotIn('Traceback',stderr.getvalue())


if __name__=='__main__': unittest.main()
