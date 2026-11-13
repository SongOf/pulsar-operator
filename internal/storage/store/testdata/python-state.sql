-- Exported with the original Python diskfleet.store.Store and sqlite3.iterdump.
BEGIN TRANSACTION;
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT, level TEXT, action TEXT, message TEXT, node TEXT);
INSERT INTO "events" VALUES(1,'2026-11-13T10:12:30.957342+00:00','info','python.fixture','原 Python 数据库兼容夹具','worker-01');
CREATE TABLE objects (kind TEXT, id TEXT, node TEXT, name TEXT, state TEXT, data TEXT, PRIMARY KEY(kind,id));
INSERT INTO "objects" VALUES('disks','python-disk','worker-01','vdb','Available','{"id": "python-disk", "name": "vdb", "node": "worker-01", "path": "/dev/disk/by-id/fixture", "serial": "PYTHON-SERIAL", "sizeBytes": 9007199254740993, "state": "Available", "reasons": [], "poolId": null}');
INSERT INTO "objects" VALUES('release-intents','retained-old-pvc-uid','','data (retained)','ReleaseRequested','{"id": "retained-old-pvc-uid", "name": "data (retained)", "pvcKey": "default~data", "pvcUid": "old-pvc-uid", "pvUid": "old-pv-uid", "status": "ReleaseRequested", "sizeBytes": 9007199254740993}');
CREATE TABLE plans (id TEXT PRIMARY KEY, expires REAL, consumed INTEGER DEFAULT 0, data TEXT);
INSERT INTO "plans" VALUES('python-plan',4102444800.0,0,'{"name": "python-pool", "node": "worker-01", "diskIds": ["python-disk"], "disks": [{"id": "python-disk", "name": "vdb", "node": "worker-01", "path": "/dev/disk/by-id/fixture", "serial": "PYTHON-SERIAL", "sizeBytes": 9007199254740993, "state": "Available", "reasons": [], "poolId": null}], "vgName": "df_python", "confirmation": "\u63a5\u7ba1 PYTHON-SERIAL"}');
CREATE INDEX objects_lookup ON objects(kind,node,state);
DELETE FROM "sqlite_sequence";
INSERT INTO "sqlite_sequence" VALUES('events',1);
COMMIT;
