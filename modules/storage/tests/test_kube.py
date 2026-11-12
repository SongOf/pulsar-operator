import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, urlsplit

from diskfleet.kube import KubeClient, KubeError


def response(body):
    return io.BytesIO(json.dumps(body).encode())


class KubeClientTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.token=Path(self.temp.name)/'token'
        self.client=KubeClient(url='https://cluster.example:6443',token_file=self.token)

    def test_http_is_only_allowed_for_loopback_proxy(self):
        for url in ('http://example.com','http://10.0.0.1:8001','file:///tmp/token'):
            with self.subTest(url=url), self.assertRaises(ValueError):
                KubeClient(url=url)
        for url in ('http://localhost:8001','http://127.0.0.1:8001','http://[::1]:8001'):
            with self.subTest(url=url):
                self.assertEqual(KubeClient(url=url).url,url)

    def test_list_follows_continuation_without_losing_selector(self):
        pages=[response({'items':[{'metadata':{'name':'disk-a'}}],'metadata':{'continue':'page+2/token='}}),
               response({'items':[{'metadata':{'name':'disk-b'}}],'metadata':{}})]
        with patch('diskfleet.kube.urlopen',side_effect=pages) as call:
            items=self.client.list('/api/v1/nodes',label_selector='storage.diskfleet.io/enabled=true',field_selector='metadata.name=node-a')
        self.assertEqual([i['metadata']['name'] for i in items],['disk-a','disk-b'])
        self.assertEqual(call.call_count,2)
        first=parse_qs(urlsplit(call.call_args_list[0].args[0].full_url).query)
        second=parse_qs(urlsplit(call.call_args_list[1].args[0].full_url).query)
        self.assertNotIn('continue',first)
        self.assertEqual(second['continue'],['page+2/token='])
        self.assertEqual(second['labelSelector'],['storage.diskfleet.io/enabled=true'])
        self.assertEqual(second['fieldSelector'],['metadata.name=node-a'])
        self.assertEqual(second['limit'],['500'])

    def test_service_account_token_is_reread_after_rotation(self):
        self.token.write_text('old-token\n')
        with patch('diskfleet.kube.urlopen',side_effect=[response({}),response({})]) as call:
            self.client.request('GET','/api/v1/nodes')
            self.token.write_text('rotated-token\n')
            self.client.request('GET','/api/v1/nodes')
        self.assertEqual(call.call_args_list[0].args[0].get_header('Authorization'),'Bearer old-token')
        self.assertEqual(call.call_args_list[1].args[0].get_header('Authorization'),'Bearer rotated-token')

    def test_patch_uses_merge_patch_content_type(self):
        body={'status':{'phase':'Ready'}}
        with patch('diskfleet.kube.urlopen',return_value=response({})) as call:
            self.client.request('PATCH','/apis/storage.diskfleet.io/v1alpha1/diskpools/test/status',body)
        req=call.call_args.args[0]
        self.assertEqual(req.get_header('Content-type'),'application/merge-patch+json')
        self.assertEqual(json.loads(req.data),body)
        self.assertEqual(req.method,'PATCH')

    def test_kubernetes_http_status_and_message_are_preserved(self):
        error=HTTPError('https://cluster.example',409,'Conflict',{},response({'message':'resourceVersion mismatch'}))
        with patch('diskfleet.kube.urlopen',side_effect=error):
            with self.assertRaises(KubeError) as ctx:
                self.client.request('PATCH','/resource',{})
        self.assertEqual(ctx.exception.status,409)
        self.assertEqual(str(ctx.exception),'resourceVersion mismatch')

    def test_non_json_http_error_is_handled(self):
        error=HTTPError('https://cluster.example',502,'Bad Gateway',{},io.BytesIO(b'<html>upstream unavailable</html>'))
        with patch('diskfleet.kube.urlopen',side_effect=error):
            with self.assertRaises(KubeError) as ctx:
                self.client.request('GET','/resource')
        self.assertEqual(ctx.exception.status,502)

    def test_connection_failure_is_service_unavailable(self):
        with patch('diskfleet.kube.urlopen',side_effect=URLError('DNS failure')):
            with self.assertRaises(KubeError) as ctx:
                self.client.request('GET','/resource')
        self.assertEqual(ctx.exception.status,503)

    def test_empty_successful_response_is_supported(self):
        with patch('diskfleet.kube.urlopen',return_value=io.BytesIO(b'')):
            self.assertEqual(self.client.request('DELETE','/resource',{}),{})


if __name__=='__main__':
    unittest.main()
