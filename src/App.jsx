import React, {useMemo, useState} from 'react';
import {createRoot} from 'react-dom/client';
import './style.css';

const initial = [
  {kind:'Pulsar', name:'demo-pulsar', namespace:'default', phase:'Running', replicas:{brokers:3, bookies:3}, storage:{journal:'10Gi', ledger:'50Gi'}, updatedAt:new Date().toISOString()},
  {kind:'BookKeeper', name:'demo-bookkeeper', namespace:'default', phase:'Running', replicas:{bookies:3}, storage:{journal:'10Gi', ledger:'50Gi'}, updatedAt:new Date().toISOString()}
];

function App(){
  const [clusters,setClusters]=useState(initial);
  const [form,setForm]=useState({kind:'Pulsar', name:'', namespace:'default', brokers:3, bookies:3, journal:'10Gi', ledger:'50Gi'});
  const totals=useMemo(()=>clusters.reduce((a,c)=>({brokers:a.brokers+(c.replicas.brokers||0),bookies:a.bookies+(c.replicas.bookies||0)}),{brokers:0,bookies:0}),[clusters]);
  function createCluster(e){e.preventDefault(); if(!form.name)return; setClusters([{kind:form.kind,name:form.name,namespace:form.namespace,phase:'Reconciling',replicas:{brokers:+form.brokers||0,bookies:+form.bookies||0},storage:{journal:form.journal,ledger:form.ledger},updatedAt:new Date().toISOString()},...clusters]); setForm({...form,name:''});}
  function scale(i, component, delta){setClusters(clusters.map((c,idx)=>idx===i?{...c,phase:'Scaling',replicas:{...c.replicas,[component]:Math.max(0,(c.replicas[component]||0)+delta)},updatedAt:new Date().toISOString()}:c));}
  function restart(i, component){setClusters(clusters.map((c,idx)=>idx===i?{...c,phase:`Restarting ${component}`,updatedAt:new Date().toISOString()}:c));}
  function storage(i, key, value){setClusters(clusters.map((c,idx)=>idx===i?{...c,phase:'Updating storage',storage:{...c.storage,[key]:value},updatedAt:new Date().toISOString()}:c));}
  return <main>
    <header><h1>Pulsar Operator Console</h1><p>Manage Pulsar brokers and BookKeeper clusters, including scale operations and journal / ledger PVC sizes.</p></header>
    <section className="metrics"><article><b>{clusters.length}</b><span>Clusters</span></article><article><b>{totals.brokers}</b><span>Brokers</span></article><article><b>{totals.bookies}</b><span>Bookies</span></article></section>
    <section className="panel"><h2>Create cluster</h2><form onSubmit={createCluster} className="grid">
      {['name','namespace','brokers','bookies','journal','ledger'].map(k=><label key={k}>{k}<input value={form[k]} type={['brokers','bookies'].includes(k)?'number':'text'} onChange={e=>setForm({...form,[k]:e.target.value})}/></label>)}
      <label>kind<select value={form.kind} onChange={e=>setForm({...form,kind:e.target.value})}><option>Pulsar</option><option>BookKeeper</option></select></label><button>Create</button>
    </form></section>
    <section className="cards">{clusters.map((c,i)=><article className="card" key={c.namespace+c.name}><div className="row"><div><h3>{c.name}</h3><small>{c.kind} · {c.namespace}</small></div><span className="phase">{c.phase}</span></div>
      <div className="ops"><h4>Operations</h4>{['brokers','bookies'].map(comp=><div className="op" key={comp}><span>{comp}: {c.replicas[comp]||0}</span><button onClick={()=>scale(i,comp,1)}>Scale out</button><button onClick={()=>scale(i,comp,-1)}>Scale in</button><button onClick={()=>restart(i,comp)}>Restart</button></div>)}
      <h4>BookKeeper PV / PVC storage</h4>{['journal','ledger'].map(k=><label className="storage" key={k}>{k} PVC<input value={c.storage[k]} onChange={e=>storage(i,k,e.target.value)}/></label>)}</div></article>)}</section>
  </main>
}

createRoot(document.getElementById('root')).render(<App/>);
