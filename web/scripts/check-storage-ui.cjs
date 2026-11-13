#!/usr/bin/env node
"use strict";

// No network or third-party packages. This checks template code against real
// Go API demo snapshots. It does not emulate browser layout or interactions.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const {spawnSync} = require("node:child_process");
const {pathToFileURL} = require("node:url");
const repositoryRoot = path.resolve(__dirname, "../..");
const source = fs.readFileSync(path.join(repositoryRoot, "web/storage/static/app.js"), "utf8");
let passed = 0;
const pass = message => { passed += 1; console.log(`PASS ${message}`); };
new Function(source); // Parse the entire browser bundle without executing boot.
pass("app.js JavaScript syntax");

const builtAPI = path.join(repositoryRoot, "bin/diskfleet-api");
const binary = process.env.DISKFLEET_API || (fs.existsSync(builtAPI) ? builtAPI : null);
const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "diskfleet-ui-check-"));
let snapshot;
try {
  snapshot = spawnSync(binary || "go", [
    ...(binary ? [] : ["run", "./cmd/storage-api"]),
    "--mode", "demo", "--host", "127.0.0.1", "--snapshot", "--state-dir", stateDir,
  ], {
    cwd: repositoryRoot,
    env: {...process.env, MODE: "demo", API_HOST: "127.0.0.1", API_PORT: "8080", API_TOKEN: ""},
    encoding: "utf8",
    maxBuffer: 8 * 1024 * 1024,
    timeout: 120000,
  });
} finally {
  fs.rmSync(stateDir, {recursive: true, force: true});
}
if (snapshot.error) throw snapshot.error;
if (snapshot.status !== 0) throw new Error(snapshot.stderr || "Go API snapshot failed");
const state = JSON.parse(snapshot.stdout);
function page(kind, limit, offset = 0) {
  const collection = state[kind];
  assert.ok(Array.isArray(collection.items), `Missing Go snapshot collection: ${kind}`);
  assert.ok(collection.items.length >= Math.min(collection.total, offset + limit), `${kind} snapshot is incomplete`);
  return {items: structuredClone(collection.items.slice(offset, offset + limit)), total: collection.total, limit, offset};
}
// The snapshot contains real backend objects. Slice independent response pages
// for the exact requests made by these templates; HTTP pagination is Go-tested.
const fixtures = {
  "/api/overview": state.overview,
  "/api/nodes?limit=6&offset=0": page("nodes", 6),
  "/api/disks?limit=72&offset=0": page("disks", 72),
  "/api/nodes?limit=12&offset=0": page("nodes", 12),
  "/api/disks?limit=12&offset=0&q=&state=&node=": page("disks", 12),
  "/api/pools?limit=12&offset=0": page("pools", 12),
  "/api/volumes?limit=12&offset=0": page("volumes", 12),
  "/api/events?limit=12&offset=0": page("events", 12),
  "/api/config": {mode: state.overview.mode, tokenRequired: false},
};
assert.equal(state.overview.mode, "demo");
pass("Go API snapshot without a listening socket");

const noop = () => {};
const views = ["overview", "nodePage", "diskPage", "poolPage", "volumePage", "eventPage", "guidePage"];
assert.match(source, /\nboot\(\);\s*$/, "Update the harness when the application entrypoint changes");
const testSource = source.replace(/\nboot\(\);\s*$/, `\nreturn {${views.join(",")},volumeDetail,expandVolume,releaseVolume,volumeReadOnlyReason,volumeReleasePath,escapeHTML,state,api,boot,navigate,reportEmbedded};`);
function createConsole(pathname, expectedPrefix, search = "") {
  const elements = new Map(), requests = [], tokenKeys = [], messages = [], listeners = new Map(), classes = new Set();
  const element = () => ({hidden: true, value: "0", addEventListener: noop, querySelector: () => null, querySelectorAll: () => [], append: noop, remove: noop, focus: noop, showModal: noop, classList: {toggle: noop}});
  const documentStub = {querySelector: selector => {
    if (!elements.has(selector)) elements.set(selector, element());
    return elements.get(selector);
  }, querySelectorAll: () => [], createElement: element,
  documentElement: {classList: {toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name)}}};
  const parent = {postMessage: (message, origin) => messages.push({message, origin})};
  const windowStub = {addEventListener: (kind, listener) => listeners.set(kind, listener), parent};
  const location = {hash: "", pathname, search, origin: "https://console.test"};
  const fetchStub = async (url, options) => {
    assert.ok(url.startsWith(`${expectedPrefix}/api/`), `API prefix missing or incorrect: ${url}`);
    const route = url.slice(expectedPrefix.length);
    assert.ok(Object.hasOwn(fixtures, route), `Unexpected API request: ${url}`);
    requests.push({url, options});
    return {ok: true, status: 200, json: async () => fixtures[route]};
  };
  const consoleApp = new Function("document", "window", "location", "sessionStorage", "fetch", "URLSearchParams", "setInterval", "clearInterval", testSource)(
    documentStub, windowStub, location,
    {getItem: key => { tokenKeys.push(key); return key === "diskfleet-token" ? "storage-only-test" : "pulsar-token-must-not-be-used"; }},
    fetchStub, URLSearchParams, noop, noop,
  );
  return {consoleApp, elements, requests, tokenKeys, classes, messages, listeners, parent, location};
}

async function check() {
  for (const [pathname, prefix, search] of [["/", "", ""], ["/storage/", "/storage", ""], ["/storage/", "/storage", "?embedded=1"]]) {
    const {consoleApp, elements, requests, tokenKeys, classes} = createConsole(pathname, prefix, search);
    for (const name of views) {
      const html = await consoleApp[name]();
      assert.equal(typeof html, "string");
      assert.ok(html.length > 100, `${name} is empty`);
      assert.ok(!html.includes("undefined"), `${name} contains a missing API field`);
      pass(`${pathname}${search} ${name} (${html.length} characters)`);
    }
    await consoleApp.api("/api/config");
    assert.equal(elements.get("#pulsar-console-link").hidden, !prefix || search === "?embedded=1");
    assert.equal(classes.has("embedded"), search === "?embedded=1");
    assert.ok(requests.length > 0);
    assert.ok(requests.every(request => request.options.headers.Authorization === "Bearer storage-only-test"));
    assert.ok(tokenKeys.length > 0 && tokenKeys.every(key => key === "diskfleet-token"));
    pass(`${pathname}${search} API prefix, return navigation and independent storage token`);
  }
  for (const [pathname, prefix] of [["/storage", "/storage"], ["/storage-other/", ""]]) {
    const {consoleApp, elements} = createConsole(pathname, prefix);
    await consoleApp.api("/api/config");
    assert.equal(elements.get("#pulsar-console-link").hidden, !prefix);
    pass(`${pathname} route boundary`);
  }
  for (const search of ["", "?embedded=0", "?other=embedded%3D1"]) {
    const app = createConsole("/storage/", "/storage", search);
    assert.ok(!app.classes.has("embedded"));
    app.consoleApp.reportEmbedded("route");
    assert.equal(app.messages.length, 0);
  }
  pass("embedding requires the explicit embedded=1 query parameter");
  const embedded = createConsole("/storage/", "/storage", "?embedded=1");
  embedded.consoleApp.reportEmbedded("ready");
  assert.equal(embedded.messages.length, 0, "Do not report ready before the API check finishes");
  await embedded.consoleApp.boot();
  const readyMessage = embedded.messages.find(item => item.message.kind === "ready");
  assert.equal(readyMessage.message.apiReady, true);
  assert.equal(readyMessage.origin, embedded.location.origin);
  assert.ok(!JSON.stringify(embedded.messages).includes("storage-only-test"));
  assert.ok(embedded.messages.every(item => Object.keys(item.message).every(key => ["channel", "kind", "page", "apiReady"].includes(key))));
  pass("embedded API readiness and token-free messages use an exact target origin");
  embedded.location.hash = "#nodes";
  embedded.listeners.get("hashchange")();
  assert.equal(embedded.messages.at(-1).message.page, "nodes");
  embedded.location.hash = "#https://untrusted.invalid/";
  embedded.consoleApp.navigate();
  assert.equal(embedded.messages.at(-1).message.page, "overview");
  pass("embedded hash navigation reports only known pages");
  const before = embedded.messages.length;
  const request = {channel: "pulsar.storage", kind: "request-state"};
  const receive = embedded.listeners.get("message");
  receive({origin: "https://untrusted.invalid", source: embedded.parent, data: request});
  receive({origin: embedded.location.origin, source: {}, data: request});
  receive({origin: embedded.location.origin, source: embedded.parent, data: {...request, kind: "navigate", url: "https://untrusted.invalid"}});
  assert.equal(embedded.messages.length, before);
  receive({origin: embedded.location.origin, source: embedded.parent, data: request});
  assert.equal(embedded.messages.length, before + 1);
  pass("embedded parent messages validate origin, source and protocol kind");

  // Execute the shell's pure protocol/endpoint logic; JSX rendering needs a browser/build.
  const workspace = fs.readFileSync(path.join(repositoryRoot, "web/src/StorageWorkspace.jsx"), "utf8");
  const logic = workspace.split("export function StorageWorkspace(")[0].replace(/^import .*$/gm, "").replace(/^export \{.*$/gm, "").replace(/^export /gm, "");
  const routes = await import(pathToFileURL(path.join(repositoryRoot, "web/src/routes.js")).href);
  const shell = new Function("STORAGE_PAGES", "isStoragePage", `${logic}\nreturn {STORAGE_PAGES,isStoragePage,readStorageMessage,checkStorageEndpoint};`)(routes.STORAGE_PAGES, routes.isStoragePage);
  assert.equal(shell.STORAGE_PAGES.length, 7);
  const frame = {}, origin = "https://console.test";
  const valid = {origin, source: frame, data: {channel: "pulsar.storage", kind: "route", page: "disks"}};
  assert.equal(shell.readStorageMessage(valid, frame, origin).page, "disks");
  assert.equal(shell.readStorageMessage({...valid, origin: "https://untrusted.invalid"}, frame, origin), null);
  assert.equal(shell.readStorageMessage({...valid, source: {}}, frame, origin), null);
  assert.equal(shell.readStorageMessage({...valid, data: {...valid.data, page: "https://untrusted.invalid"}}, frame, origin), null);
  assert.equal(shell.readStorageMessage({...valid, data: {...valid.data, kind: "ready", apiReady: "true"}}, frame, origin), null);
  assert.equal(shell.readStorageMessage({...valid, data: {...valid.data, kind: "ready", apiReady: false}}, frame, origin).apiReady, false);
  pass("Pulsar shell validates embedded message origin, source, route and readiness type");
  let probe;
  await shell.checkStorageEndpoint(async (url, options) => {
    probe = {url, options};
    return {ok: true, headers: {get: () => "application/json; charset=utf-8"}, json: async () => fixtures["/api/config"]};
  });
  assert.equal(probe.url, "/storage/api/config");
  assert.equal(probe.options.headers.Authorization, undefined);
  for (const [contentType, config] of [["text/html", {}], ["application/json", {mode: "wrong", tokenRequired: false}]]) {
    let rejected = false;
    try { await shell.checkStorageEndpoint(async () => ({ok: true, headers: {get: () => contentType}, json: async () => config})); }
    catch { rejected = true; }
    assert.ok(rejected, "Wrong backend / SPA fallback must be rejected");
  }
  pass("Pulsar shell checks storage API contract and rejects HTML fallback without forwarding tokens");
  const {consoleApp} = createConsole("/", "");
  const unsafe = '<img src=x onerror="alert(1)">';
  assert.equal(consoleApp.escapeHTML(unsafe), "&lt;img src=x onerror=&quot;alert(1)&quot;&gt;");
  fixtures["/api/disks?limit=12&offset=0&q=&state=&node="].items[0].name = unsafe;
  const diskHTML = await consoleApp.diskPage();
  assert.ok(!diskHTML.includes(unsafe), "Disk metadata must never be executable HTML");
  assert.ok(diskHTML.includes(consoleApp.escapeHTML(unsafe)));
  pass("metadata escaping");
  const volume = fixtures["/api/volumes?limit=12&offset=0"].items[0];
  volume.status = "ReleaseRequested";
  consoleApp.state.mode = "cluster";
  const volumeHTML = await consoleApp.volumePage();
  assert.ok(volumeHTML.includes("已请求释放"));
  assert.ok(!volumeHTML.includes(`data-action="expand-volume" data-id="${volume.id}"`));
  assert.ok(!volumeHTML.includes(`data-action="release-volume" data-id="${volume.id}"`));
  assert.ok(volumeHTML.includes("WaitForFirstConsumer"));
  pass("release-in-progress action guards");
  volume.status = "Bound";
  volume.canManage = false;
  volume.management = "external";
  volume.owners = [{kind: "StatefulSet", name: "pulsar-bookie"}];
  const externalHTML = await consoleApp.volumePage();
  assert.ok(externalHTML.includes("外部 Operator · 只读"));
  assert.ok(!externalHTML.includes(`data-action="expand-volume" data-id="${volume.id}"`));
  assert.ok(!externalHTML.includes(`data-action="release-volume" data-id="${volume.id}"`));
  pass("external Operator action guards");
  volume.management = "diskfleet";
  volume.bindingVerified = false;
  volume.node = "";
  volume.pool = "";
  volume.poolId = "";
  volume.bindingMessage = `PV 身份不匹配 ${unsafe}`;
  const failedBindingHTML = await consoleApp.volumePage();
  assert.ok(failedBindingHTML.includes("绑定身份待核验"));
  assert.ok(failedBindingHTML.includes("归属待核验"));
  assert.ok(failedBindingHTML.includes(consoleApp.escapeHTML(volume.bindingMessage)));
  assert.ok(!failedBindingHTML.includes(unsafe));
  assert.ok(!failedBindingHTML.includes(`data-action="expand-volume" data-id="${volume.id}"`));
  assert.ok(!failedBindingHTML.includes(`data-action="release-volume" data-id="${volume.id}"`));
  assert.equal(consoleApp.volumeReadOnlyReason(volume), volume.bindingMessage);
  assert.ok(!consoleApp.volumeReadOnlyReason(volume).includes("外部 Operator"));
  pass("unverified DiskFleet binding shows its actual escaped reason and remains read-only");

  const details = createConsole("/storage/", "/storage");
  await details.consoleApp.volumePage();
  details.consoleApp.volumeDetail(volume.id);
  let detailHTML = details.elements.get("#dialog-body").innerHTML;
  assert.ok(detailHTML.includes(consoleApp.escapeHTML(volume.bindingMessage)));
  assert.ok(detailHTML.includes("待核验（见提示）"));
  assert.ok(!detailHTML.includes(unsafe));
  pass("volume details retain and escape the binding failure instead of promising Retain");

  volume.status = "ReleaseRequested";
  delete volume.bindingMessage;
  volume.message = `保留 PV 身份或策略需要人工核对 ${unsafe}`;
  const retainedHTML = await details.consoleApp.volumePage();
  assert.ok(retainedHTML.includes(consoleApp.escapeHTML(volume.message)));
  assert.ok(!retainedHTML.includes(unsafe));
  details.consoleApp.volumeDetail(volume.id);
  detailHTML = details.elements.get("#dialog-body").innerHTML;
  assert.ok(detailHTML.includes(consoleApp.escapeHTML(volume.message)));
  assert.ok(!detailHTML.includes(unsafe));
  pass("Retain history identity/policy warnings are visible and escaped in rows and details");

  const pool = fixtures["/api/pools?limit=12&offset=0"].items[0];
  pool.state = "Conflict";
  pool.message = `多个池认领同一磁盘 ${unsafe}`;
  const conflictHTML = await details.consoleApp.poolPage();
  assert.ok(conflictHTML.includes("认领冲突"));
  assert.ok(conflictHTML.includes(consoleApp.escapeHTML(pool.message)));
  assert.ok(!conflictHTML.includes(unsafe));
  assert.ok(!conflictHTML.includes(`data-action="create-volume" data-id="${pool.id}"`));
  pass("conflicting pool claims expose the escaped reason and do not offer provisioning");
  details.consoleApp.state.mode = "cluster";
  const reviewed = {id: "team~claim", pvcUid: "reviewed-uid/with?characters"};
  assert.equal(details.consoleApp.volumeReleasePath(reviewed), "/api/volumes/team~claim?uid=reviewed-uid%2Fwith%3Fcharacters");
  details.consoleApp.state.records.set(reviewed.id, {...reviewed, pvcUid: "replacement-uid"});
  assert.ok(details.consoleApp.volumeReleasePath(reviewed).includes("reviewed-uid%2F"), "The confirmation must retain the reviewed UID even if polling sees a replacement");
  assert.throws(()=>details.consoleApp.volumeReleasePath({id: reviewed.id}), /PVC 身份缺失/);
  details.consoleApp.state.mode = "demo";
  assert.equal(details.consoleApp.volumeReleasePath(reviewed), "/api/volumes/team~claim");
  pass("cluster release targets the reviewed PVC UID, fails closed without it, and preserves the demo API");
  console.log(`PASS ${passed} checks`);
  console.log("LIMIT: no browser DOM, layout, socket, or user interaction verification.");
}
check().catch(error => { console.error(error); process.exitCode = 1; });
