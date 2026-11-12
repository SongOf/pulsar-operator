"""Transactional simulation and local audit/plan store. No block devices are opened."""
import hashlib
import json
import re
import sqlite3
import time
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

GiB = 1024 ** 3
NAME = re.compile(r'^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$')


class Problem(ValueError):
    def __init__(self, message, status=400):
        self.status = status
        super().__init__(message)


def name(value):
    if not isinstance(value, str) or not NAME.fullmatch(value):
        raise Problem('名称须为 1–63 位小写字母、数字或连字符，首尾须为字母或数字')
    return value


def node_name(value):
    if not isinstance(value, str) or len(value)>253 or not all(NAME.fullmatch(part) for part in value.split('.')):
        raise Problem('节点名须为有效的 DNS 子域名')
    return value


def size_gib(value):
    if isinstance(value, bool) or not isinstance(value, int) or not 1 <= value <= 1048576:
        raise Problem('容量必须为 1–1048576 的整数 GiB')
    return value * GiB


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.~-]{0,252}', value):
        raise Problem('资源 ID 必须为有效的字符串标识')
    return value


def now():
    return datetime.now(timezone.utc).isoformat()


class Store:
    def __init__(self, root):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.db = self.root / 'state.sqlite3'
        with self.transaction() as con:
            con.executescript('''
                CREATE TABLE IF NOT EXISTS objects (kind TEXT, id TEXT, node TEXT, name TEXT, state TEXT, data TEXT, PRIMARY KEY(kind,id));
                CREATE INDEX IF NOT EXISTS objects_lookup ON objects(kind,node,state);
                CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT, level TEXT, action TEXT, message TEXT, node TEXT);
                CREATE TABLE IF NOT EXISTS plans (id TEXT PRIMARY KEY, expires REAL, consumed INTEGER DEFAULT 0, data TEXT);
            ''')

    @contextmanager
    def transaction(self):
        con = sqlite3.connect(self.db, timeout=30)
        con.row_factory = sqlite3.Row
        con.execute('PRAGMA journal_mode=WAL')
        con.execute('BEGIN IMMEDIATE')
        try:
            yield con
            con.commit()
        except BaseException:
            con.rollback()
            raise
        finally:
            con.close()

    def put(self, con, kind, obj):
        con.execute('INSERT INTO objects VALUES (?,?,?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET node=excluded.node,name=excluded.name,state=excluded.state,data=excluded.data',
                    (kind, obj['id'], obj.get('node', ''), obj.get('name', ''), obj.get('state', obj.get('status', '')), json.dumps(obj)))

    def get(self, con, kind, key):
        key = identifier(key)
        row = con.execute('SELECT data FROM objects WHERE kind=? AND id=?', (kind, key)).fetchone()
        if row is None:
            raise Problem('资源不存在', 404)
        return json.loads(row[0])

    def event(self, con, action, message, node='', level='info'):
        con.execute('INSERT INTO events(time,level,action,message,node) VALUES (?,?,?,?,?)', (now(), level, action, message, node))

    def audit(self, action, message, node='', level='info'):
        with self.transaction() as con:
            self.event(con, action, message, node, level)

    def list(self, kind, limit=50, offset=0, node='', state='', q=''):
        terms, values = ['kind=?'], [kind]
        for column, value in [('node', node), ('state', state)]:
            if value:
                terms.append(column + '=?')
                values.append(value)
        if q:
            terms.append('(name LIKE ? ESCAPE "!" OR data LIKE ? ESCAPE "!")')
            escaped = q.replace('!', '!!').replace('%', '!%').replace('_', '!_')
            values.extend(['%' + escaped + '%'] * 2)
        where = ' AND '.join(terms)
        with self.transaction() as con:
            total = con.execute('SELECT COUNT(*) FROM objects WHERE ' + where, values).fetchone()[0]
            items = [json.loads(r[0]) for r in con.execute('SELECT data FROM objects WHERE ' + where + ' ORDER BY node,name LIMIT ? OFFSET ?', values + [limit, offset])]
            if kind == 'nodes':
                for item in items:
                    item['allocatedBytes'] = sum(json.loads(r[0])['allocatedBytes'] for r in con.execute("SELECT data FROM objects WHERE kind='pools' AND node=?", (item['name'],)))
        return dict(items=items, total=total, limit=limit, offset=offset)

    def events(self, limit=50, offset=0, **_):
        with self.transaction() as con:
            total = con.execute('SELECT COUNT(*) FROM events').fetchone()[0]
            items = [dict(r) for r in con.execute('SELECT * FROM events ORDER BY id DESC LIMIT ? OFFSET ?', (limit, offset))]
        return dict(items=items, total=total, limit=limit, offset=offset)

    def seed(self):
        with self.transaction() as con:
            if con.execute("SELECT COUNT(*) FROM objects WHERE kind='disks'").fetchone()[0]:
                return
            for n in range(1, 25):
                node = f'baremetal-{n:03}'
                for d in range(6):
                    disk = dict(id=f'disk-{n:03}-{d}', name=f'nvme{d}n1' if d < 3 else f'sd{chr(97+d)}', node=node,
                                path=f'/dev/disk/by-id/demo-{n:03}-{d}', serial=f'DEMO-{n:03}-{d}', sizeBytes=(480 if d == 0 else 1920 if d < 3 else 3840)*GiB,
                                media='NVMe' if d < 3 else 'SSD', model='模拟 NVMe' if d < 3 else '模拟 SATA SSD',
                                state='Protected' if d == 0 else 'Offline' if n == 24 and d == 5 else 'Available',
                                reasons=['模拟系统盘，禁止接管'] if d == 0 else ['模拟设备失联'] if n == 24 and d == 5 else [], poolId=None)
                    self.put(con, 'disks', disk)
                self.put(con, 'nodes', dict(id=node, name=node, node=node, zone=f'az-{1+(n-1)//8}', state='Ready', disks=6, capacityBytes=15840*GiB, allocatedBytes=0))
            self.event(con, 'demo.seed', '已生成 24 台模拟节点 / 144 块模拟磁盘；不代表本机实际硬件')
        for i in range(1, 7):
            p = self.plan(dict(name=f'nvme-pool-{i:02}', node=f'baremetal-{i:03}', diskIds=[f'disk-{i:03}-1']))
            pool = self.create_pool(p['planId'], p['confirmation'])
            self.create_volume(dict(name=f'app-data-{i:02}', namespace='default', poolId=pool['id'], sizeGiB=i*64))

    def save_plan(self, data):
        plan_id = str(uuid.uuid4())
        with self.transaction() as con:
            con.execute('DELETE FROM plans WHERE expires < ?', (time.time()-86400,))
            con.execute('INSERT INTO plans(id,expires,data) VALUES (?,?,?)', (plan_id, time.time()+600, json.dumps(data)))
        return dict(planId=plan_id, expiresAt=datetime.fromtimestamp(time.time()+600, timezone.utc).isoformat(), **data)

    def read_plan(self, con, plan_id, confirmation):
        plan_id = identifier(plan_id)
        if not isinstance(confirmation, str) or not 1 <= len(confirmation) <= 512:
            raise Problem('确认文本必须为 1–512 字符的字符串')
        row = con.execute('SELECT * FROM plans WHERE id=?', (plan_id,)).fetchone()
        if not row or row['expires'] < time.time():
            raise Problem('计划不存在或已过期，请重新生成', 409)
        data = json.loads(row['data'])
        if confirmation != data['confirmation']:
            raise Problem('确认文本不匹配', 409)
        return data, bool(row['consumed'])

    def plan(self, body):
        pool_name, node = name(body.get('name')), node_name(body.get('node'))
        ids = body.get('diskIds')
        if not isinstance(ids, list) or len(ids) != 1 or not isinstance(ids[0], str):
            raise Problem('每个池仅接管一块物理盘；批量操作会创建多个独立池')
        with self.transaction() as con:
            disk = self.get(con, 'disks', ids[0])
            if disk['node'] != node or disk['state'] != 'Available':
                raise Problem('磁盘不可接管或不属于选定节点', 409)
            if con.execute("SELECT 1 FROM objects WHERE kind='pools' AND name=?", (pool_name,)).fetchone():
                raise Problem('存储池名称已存在', 409)
        vg = 'df_' + hashlib.sha256((node + ':' + disk['serial']).encode()).hexdigest()[:16]
        return self.save_plan(dict(name=pool_name, node=node, diskIds=ids, disks=[disk], vgName=vg,
            confirmation='接管 ' + disk['serial'], commands=[['pvcreate', disk['path']], ['vgcreate', vg, disk['path']]],
            warnings=['模拟模式：不执行上述命令。', '真实模式会初始化所选空盘；LVM 不提供跨节点数据复制。']))

    def create_pool(self, plan_id, confirmation):
        with self.transaction() as con:
            plan, consumed = self.read_plan(con, plan_id, confirmation)
            pid = 'pool-' + plan['vgName'][3:]
            if consumed:
                return self.get(con, 'pools', pid)
            disk = self.get(con, 'disks', plan['diskIds'][0])
            expected = plan['disks'][0]
            if disk['state'] != 'Available' or any(disk.get(k) != expected.get(k) for k in ('serial','path','sizeBytes','node')):
                raise Problem('磁盘状态或身份已经改变，必须重新生成计划', 409)
            if con.execute("SELECT 1 FROM objects WHERE kind='pools' AND name=?", (plan['name'],)).fetchone():
                raise Problem('存储池名称已存在', 409)
            capacity = (disk['sizeBytes'] // GiB - 1) * GiB
            pool = dict(id=pid, name=plan['name'], node=plan['node'], vgName=plan['vgName'], capacityBytes=capacity,
                        allocatedBytes=0, availableBytes=capacity, state='Ready', diskIds=plan['diskIds'], storageClass='diskfleet-'+pid, createdAt=now())
            disk.update(state='Managed', poolId=pid)
            self.put(con, 'disks', disk)
            self.put(con, 'pools', pool)
            con.execute('UPDATE plans SET consumed=1 WHERE id=?', (plan_id,))
            self.event(con, 'pool.ready', f"模拟接管 {disk['serial']} → {pool['name']}", pool['node'])
            return pool

    def create_volume(self, body):
        vol_name, namespace = name(body.get('name')), name(body.get('namespace', 'default'))
        size = size_gib(body.get('sizeGiB'))
        with self.transaction() as con:
            for row in con.execute("SELECT data FROM objects WHERE kind='volumes' AND name=?", (vol_name,)):
                existing = json.loads(row[0])
                if existing['namespace'] == namespace and existing['status'] != 'Released':
                    if existing['poolId'] == body.get('poolId') and existing['sizeBytes'] == size:
                        return existing
                    raise Problem('同命名空间中已存在同名 PVC，参数不同', 409)
            pool = self.get(con, 'pools', body.get('poolId'))
            if pool['state'] != 'Ready' or size > pool['availableBytes']:
                raise Problem('存储池不可用或剩余容量不足', 409)
            key = str(uuid.uuid4())
            mount = self.root / 'volumes' / key
            mount.mkdir(parents=True, mode=0o700)
            volume = dict(id=key, name=vol_name, namespace=namespace, poolId=pool['id'], pool=pool['name'], node=pool['node'], sizeBytes=size,
                          status='Bound', storageClass=pool['storageClass'], pvName='demo-pv-'+key, mountPath=str(mount), createdAt=now())
            pool['allocatedBytes'] += size
            pool['availableBytes'] -= size
            self.put(con, 'pools', pool)
            self.put(con, 'volumes', volume)
            self.event(con, 'volume.bound', f'模拟 PVC {namespace}/{vol_name} 分配 {size//GiB} GiB（目录不实施容量配额）', pool['node'])
            return volume

    def expand(self, key, body):
        size = size_gib(body.get('sizeGiB'))
        with self.transaction() as con:
            volume = self.get(con, 'volumes', key)
            pool = self.get(con, 'pools', volume['poolId'])
            delta = size - volume['sizeBytes']
            if volume['status'] != 'Bound' or delta < 0:
                raise Problem('仅允许扩容已绑定的卷，不能缩容', 409)
            if delta > pool['availableBytes']:
                raise Problem('存储池剩余容量不足', 409)
            volume['sizeBytes'] = size
            pool['allocatedBytes'] += delta
            pool['availableBytes'] -= delta
            self.put(con, 'volumes', volume)
            self.put(con, 'pools', pool)
            self.event(con, 'volume.expanded', f"模拟 PVC {volume['name']} 扩容至 {size//GiB} GiB", pool['node'])
            return volume

    def release(self, key):
        with self.transaction() as con:
            volume = self.get(con, 'volumes', key)
            volume['status'] = 'Released'
            self.put(con, 'volumes', volume)
            self.event(con, 'volume.retained', f"PVC {volume['name']} 已释放，数据和容量继续保留", volume['node'])
            return volume

    def overview(self):
        with self.transaction() as con:
            counts = dict(con.execute('SELECT kind,COUNT(*) FROM objects GROUP BY kind'))
            pools = [json.loads(r[0]) for r in con.execute("SELECT data FROM objects WHERE kind='pools'")]
        return dict(mode='demo', totals=dict(nodes=counts.get('nodes',0),disks=counts.get('disks',0),pools=len(pools),volumes=counts.get('volumes',0),
            capacityBytes=sum(p['capacityBytes'] for p in pools),allocatedBytes=sum(p['allocatedBytes'] for p in pools),availableBytes=sum(p['availableBytes'] for p in pools)),
            recentEvents=self.events(8)['items'], alerts=[dict(level='info',message='模拟环境：容量为逻辑预留，挂载路径为普通目录；未执行 LVM 或 Kubernetes 调度。'),dict(level='warning',message='baremetal-024 有一块模拟离线磁盘，请检查节点连接。')])
