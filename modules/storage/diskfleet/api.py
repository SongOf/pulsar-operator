"""Dependency-free local management server. Use an authenticated TLS proxy in clusters."""
import argparse
import hmac
import json
import logging
import mimetypes
import os
import signal
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlsplit

from .kube import KubeError
from .store import Problem, Store, identifier

STATIC=Path(__file__).parent/'static'
LOG=logging.getLogger('diskfleet.api')


def json_object(pairs):
    result={}
    for key,value in pairs:
        if key in result: raise ValueError('Duplicate JSON object key')
        result[key]=value
    return result


def reject_json_constant(value):
    raise ValueError('Non-standard JSON constant: '+value)


class App:
    def __init__(self, mode='demo', state_dir='.state', token='', seed=True, backend=None):
        if mode not in ('demo','cluster'): raise ValueError('MODE must be demo or cluster')
        if mode=='cluster' and len(token)<24: raise ValueError('Cluster mode requires API_TOKEN of at least 24 characters')
        self.mode,self.token=mode,token
        self.store=Store(state_dir)
        if backend: self.backend=backend
        elif mode=='demo':
            if seed: self.store.seed()
            self.backend=self.store
        else:
            from .cluster import Cluster
            self.backend=Cluster(self.store)

    def route(self, method, path, query, body):
        if path=='/api/config' and method=='GET': return dict(mode=self.mode,tokenRequired=bool(self.token))
        if path=='/healthz' and method=='GET': return dict(status='ok',mode=self.mode)
        backend=self.backend
        if method=='GET':
            if path=='/api/overview': return backend.overview()
            kind=path.removeprefix('/api/')
            if kind in ('nodes','disks','pools','volumes','events'):
                try:
                    limit=int(query.get('limit',['50'])[0]); offset=int(query.get('offset',['0'])[0])
                except ValueError: raise Problem('分页参数须为整数')
                if not 1<=limit<=500 or offset<0: raise Problem('limit 须为 1–500，offset 不得为负数')
                kwargs=dict(limit=limit,offset=offset,**{k:query.get(k,[''])[0][:256] for k in ('node','state','q')})
                return backend.events(**kwargs) if kind=='events' else backend.list(kind,**kwargs)
        if method=='POST':
            if path=='/api/pools/plan': return backend.plan(body)
            if path=='/api/pools': return backend.create_pool(body.get('planId'),body.get('confirmation'))
            if path=='/api/volumes':
                identifier(body.get('poolId'))
                return backend.create_volume(body)
            if path.startswith('/api/volumes/') and path.endswith('/expand') and len(path.split('/'))==5:
                return backend.expand(identifier(unquote(path.split('/')[3])),body)
        if method=='DELETE' and path.startswith('/api/volumes/') and len(path.split('/'))==4:
            key=identifier(unquote(path.split('/')[3]))
            if self.mode=='cluster' and 'uid' in query:
                if len(query['uid'])!=1: raise Problem('只能提供一个 PVC UID')
                return backend.release(key,expected_uid=identifier(query['uid'][0]))
            return backend.release(key)
        raise Problem('接口不存在',404)


def handler(app):
    class Handler(BaseHTTPRequestHandler):
        server_version='DiskFleet/0.1'

        def setup(self):
            self.request.settimeout(15)
            super().setup()

        def log_message(self, fmt, *args):
            LOG.info('%s %s',self.client_address[0],fmt%args)

        def send(self,status,data,content_type='application/json; charset=utf-8'):
            payload=json.dumps(data,ensure_ascii=False).encode() if not isinstance(data,bytes) else data
            self.send_response(status)
            self.send_header('Content-Type',content_type)
            self.send_header('Content-Length',str(len(payload)))
            self.send_header('X-Content-Type-Options','nosniff')
            self.send_header('Cache-Control','no-store')
            self.send_header('Content-Security-Policy',"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'self'")
            self.send_header('X-Frame-Options','SAMEORIGIN')
            self.send_header('Connection','close')
            self.end_headers()
            self.wfile.write(payload)
            self.close_connection=True

        def process(self):
            self.connection.settimeout(15)
            try:
                try:
                    parsed=urlsplit(self.path); path=parsed.path
                    query=parse_qs(parsed.query,max_num_fields=100)
                except ValueError: raise Problem('无效 URL 或查询参数过多')
                if any(ord(c)<32 or ord(c)==127 for c in path): raise Problem('URL 路径包含控制字符')
                lengths=self.headers.get_all('Content-Length',[])
                if len(lengths)>1: raise Problem('不接受重复的 Content-Length')
                if self.headers.get_all('Transfer-Encoding',[]): raise Problem('不接受 Transfer-Encoding；请发送明确的 Content-Length')
                if len(self.headers.get_all('Authorization',[]))>1: raise Problem('不接受重复的 Authorization')
                if self.command=='GET' and (path=='/' or path.startswith('/static/')):
                    file=STATIC/'index.html' if path=='/' else STATIC/path.removeprefix('/static/')
                    file=file.resolve()
                    if not file.is_relative_to(STATIC.resolve()) or not file.is_file(): raise Problem('文件不存在',404)
                    return self.send(200,file.read_bytes(),(mimetypes.guess_type(file)[0] or 'application/octet-stream')+('; charset=utf-8' if file.suffix in ('.html','.css','.js') else ''))
                if path not in ('/api/config','/healthz') and app.token:
                    supplied=self.headers.get('Authorization','')
                    if not hmac.compare_digest(supplied.encode('utf-8'),('Bearer '+app.token).encode('utf-8')): raise Problem('需要有效的管理 Token',401)
                body={}
                if self.command=='POST':
                    if self.headers.get('Content-Type','').split(';')[0].strip().lower()!='application/json': raise Problem('需要 application/json',415)
                    if not lengths: raise Problem('需要 Content-Length',411)
                    raw_length=lengths[0].strip()
                    if not raw_length.isascii() or not raw_length.isdigit(): raise Problem('无效 Content-Length')
                    try: length=int(raw_length)
                    except ValueError: raise Problem('无效 Content-Length')
                    if not 0<length<=65536: raise Problem('请求体为空或超过 64 KiB',413)
                    payload=self.rfile.read(length)
                    if len(payload)!=length: raise Problem('请求体长度不足')
                    try: body=json.loads(payload,object_pairs_hook=json_object,parse_constant=reject_json_constant)
                    except (ValueError,UnicodeError,RecursionError): raise Problem('无效 JSON')
                    if not isinstance(body,dict): raise Problem('请求体必须为 JSON 对象')
                result=app.route(self.command,path,query,body)
                self.send(200,result)
            except (Problem,KubeError) as error:
                self.send(error.status,dict(detail=str(error)))
            except TimeoutError:
                self.send(408,dict(detail='读取请求超时'))
            except (BrokenPipeError,ConnectionResetError): pass
            except Exception:
                LOG.exception('request failed')
                self.send(500,dict(detail='服务内部错误，请查看服务日志'))

        do_GET=process
        do_POST=process
        do_DELETE=process
    return Handler


def main():
    parser=argparse.ArgumentParser(description='DiskFleet 管理页面与 REST API')
    parser.add_argument('--host',default=os.getenv('API_HOST','127.0.0.1'))
    parser.add_argument('--port',type=int,default=os.getenv('API_PORT','8080'))
    parser.add_argument('--mode',choices=['demo','cluster'],default=os.getenv('MODE','demo'))
    parser.add_argument('--state-dir',default=os.getenv('STATE_DIR','.state'))
    args=parser.parse_args()
    if not 1<=args.port<=65535: parser.error('端口须为 1–65535')
    logging.basicConfig(level=logging.INFO,format='%(asctime)s %(levelname)s %(message)s')
    token=os.getenv('API_TOKEN','')
    if args.host not in ('127.0.0.1','localhost','::1') and len(token)<24:
        parser.error('监听外部地址需要至少 24 字符的 API_TOKEN')
    try:
        app=App(args.mode,args.state_dir,token)
        server=ThreadingHTTPServer((args.host,args.port),handler(app))
    except (OSError,ValueError) as error:
        parser.exit(2,f'DiskFleet 启动失败：{error}。请检查状态目录、端口占用和运行环境的 socket 权限。\n')
    print(f'DiskFleet {args.mode}: http://{args.host}:{args.port}',flush=True)
    try: server.serve_forever()
    except KeyboardInterrupt: pass
    finally: server.server_close()


if __name__=='__main__': main()
