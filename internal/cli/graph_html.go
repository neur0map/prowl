package cli

// graphHTMLTemplate is a self-contained interactive dependency graph: one file,
// no external assets, opens offline. Data is injected at /*PROWL_DATA*/ and the
// title at /*PROWL_TITLE*/. The force simulation and rendering are vanilla JS on
// a canvas so the artifact has no build step and no runtime dependency.
const graphHTMLTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>prowl graph</title>
<style>
  :root { color-scheme: dark; }
  html,body { margin:0; height:100%; background:#181825; color:#cdd6f4;
    font:13px/1.4 ui-monospace,"JetBrains Mono",Menlo,monospace; overflow:hidden; }
  #hud { position:fixed; top:12px; left:14px; z-index:5; max-width:42vw; }
  #hud h1 { margin:0 0 2px; font-size:15px; color:#cdd6f4; font-weight:600; }
  #hud .sub { color:#7f849c; font-size:11px; }
  #legend { position:fixed; top:12px; right:14px; z-index:5; text-align:right;
    max-height:88vh; overflow:auto; }
  #legend div { white-space:nowrap; opacity:.85; cursor:default; }
  #legend .sw { display:inline-block; width:9px; height:9px; border-radius:2px; margin-left:6px; vertical-align:middle; }
  #search { position:fixed; bottom:14px; left:14px; z-index:5; background:#1e1e2e;
    border:1px solid #313244; color:#cdd6f4; padding:6px 9px; border-radius:7px;
    font:12px ui-monospace,monospace; outline:none; width:240px; }
  #tip { position:fixed; z-index:6; pointer-events:none; background:#11111b;
    border:1px solid #45475a; padding:3px 7px; border-radius:5px; font-size:11px;
    color:#cdd6f4; display:none; max-width:60vw; overflow:hidden; text-overflow:ellipsis; }
  #foot { position:fixed; bottom:14px; right:14px; z-index:5; color:#585b70; font-size:11px; }
  canvas { display:block; }
</style>
</head>
<body>
<div id="hud"><h1 id="title"></h1><div class="sub" id="stats"></div>
  <div class="sub">drag nodes · scroll to zoom · drag background to pan · type to highlight</div></div>
<div id="legend"></div>
<input id="search" placeholder="highlight files...">
<div id="tip"></div>
<div id="foot">prowl graph</div>
<canvas id="c"></canvas>
<script>
const TITLE = /*PROWL_TITLE*/;
const DATA = /*PROWL_DATA*/;
const nodes = DATA.nodes, links = DATA.links;
document.getElementById("title").textContent = TITLE + " - dependency graph";
const totalFiles = DATA.total || nodes.length;
const filesLabel = nodes.length < totalFiles ? (nodes.length + " of " + totalFiles + " files") : (totalFiles + " files");
document.getElementById("stats").textContent = filesLabel + " · " + links.length + " edges · node size = how many files depend on it";

// Stable color per subsystem.
const palette = ["#89b4fa","#a6e3a1","#f9e2af","#f38ba8","#cba6f7","#94e2d5","#fab387","#f5c2e7","#74c7ec","#eba0ac","#b4befe","#a6adc8"];
const subs = [...new Set(nodes.map(n => n.s))].sort();
const color = {};
subs.forEach((s,i) => color[s] = palette[i % palette.length]);
const legend = document.getElementById("legend");
subs.slice(0,24).forEach(s => {
  const d = document.createElement("div");
  d.innerHTML = s + '<span class="sw" style="background:'+color[s]+'"></span>';
  legend.appendChild(d);
});

const canvas = document.getElementById("c"), ctx = canvas.getContext("2d");
let W=0,H=0,dpr=Math.max(1,window.devicePixelRatio||1);
function resize(){ W=innerWidth; H=innerHeight; canvas.width=W*dpr; canvas.height=H*dpr;
  canvas.style.width=W+"px"; canvas.style.height=H+"px"; ctx.setTransform(dpr,0,0,dpr,0,0); }
addEventListener("resize", resize); resize();

// Initial layout: spread on a circle so the sim untangles fast.
nodes.forEach((n,i) => { const a=i/nodes.length*Math.PI*2;
  n.x=Math.cos(a)*Math.min(W,H)*0.35+W/2; n.y=Math.sin(a)*Math.min(W,H)*0.35+H/2; n.vx=0; n.vy=0;
  n.r=3+Math.sqrt(n.d)*1.6; });

let view={x:0,y:0,k:1}, alpha=1;
const REPULSE=1400, SPRING=0.012, LINKLEN=42, GRAV=0.015, DAMP=0.86;

function step(){
  if(alpha<0.02) return;
  for(let i=0;i<nodes.length;i++){ const a=nodes[i];
    for(let j=i+1;j<nodes.length;j++){ const b=nodes[j];
      let dx=a.x-b.x, dy=a.y-b.y, d2=dx*dx+dy*dy+0.01;
      if(d2>90000) continue;
      const f=REPULSE/d2, d=Math.sqrt(d2); dx/=d; dy/=d;
      a.vx+=dx*f; a.vy+=dy*f; b.vx-=dx*f; b.vy-=dy*f;
    }
    a.vx+=(W/2-a.x)*GRAV*0.02; a.vy+=(H/2-a.y)*GRAV*0.02;
  }
  for(const l of links){ const a=nodes[l.s], b=nodes[l.t];
    let dx=b.x-a.x, dy=b.y-a.y, d=Math.sqrt(dx*dx+dy*dy)+0.01;
    const f=(d-LINKLEN)*SPRING; dx/=d; dy/=d;
    a.vx+=dx*f; a.vy+=dy*f; b.vx-=dx*f; b.vy-=dy*f;
  }
  for(const n of nodes){ if(n===drag) continue;
    n.vx*=DAMP; n.vy*=DAMP; n.x+=n.vx*alpha; n.y+=n.vy*alpha; }
  alpha*=0.994;
}

let hoverNode=null, query="", drag=null, pan=null;
function draw(){
  ctx.clearRect(0,0,W,H);
  ctx.save(); ctx.translate(view.x,view.y); ctx.scale(view.k,view.k);
  ctx.globalAlpha=0.18; ctx.strokeStyle="#585b70"; ctx.lineWidth=0.6/view.k;
  ctx.beginPath();
  for(const l of links){ const a=nodes[l.s], b=nodes[l.t]; ctx.moveTo(a.x,a.y); ctx.lineTo(b.x,b.y); }
  ctx.stroke(); ctx.globalAlpha=1;
  for(const n of nodes){
    const hit = query && n.p.toLowerCase().includes(query);
    ctx.beginPath(); ctx.arc(n.x,n.y,n.r,0,7);
    ctx.fillStyle = color[n.s]||"#89b4fa";
    ctx.globalAlpha = query ? (hit?1:0.12) : 1;
    ctx.fill();
    if(n===hoverNode || hit){ ctx.lineWidth=1.5/view.k; ctx.strokeStyle="#f5e0dc"; ctx.stroke(); }
  }
  ctx.globalAlpha=1;
  // Labels for the biggest hubs and any highlighted/hovered node.
  ctx.fillStyle="#cdd6f4"; ctx.font=(11/view.k)+"px ui-monospace,monospace";
  for(const n of nodes){ const hit = query && n.p.toLowerCase().includes(query);
    if(n.r>7 || n===hoverNode || hit){
      const name=n.p.split("/").pop();
      ctx.globalAlpha = (query && !hit)?0.15:0.9;
      ctx.fillText(name, n.x+n.r+2, n.y+3);
    } }
  ctx.globalAlpha=1; ctx.restore();
}
function frame(){ step(); draw(); requestAnimationFrame(frame); } frame();

// Interaction
function toWorld(px,py){ return {x:(px-view.x)/view.k, y:(py-view.y)/view.k}; }
function pick(px,py){ const w=toWorld(px,py); let best=null,bd=100;
  for(const n of nodes){ const dx=n.x-w.x, dy=n.y-w.y, d=dx*dx+dy*dy;
    if(d<Math.max(bd,(n.r+4)*(n.r+4)) && d<bd){ bd=d; best=n; } } return best; }
// drag/pan declared above so the synchronous first frame() can read them.
canvas.addEventListener("mousedown", e=>{ const n=pick(e.clientX,e.clientY);
  if(n){ drag=n; } else { pan={x:e.clientX,y:e.clientY,vx:view.x,vy:view.y}; } alpha=Math.max(alpha,0.5); });
addEventListener("mousemove", e=>{
  const tip=document.getElementById("tip");
  if(drag){ const w=toWorld(e.clientX,e.clientY); drag.x=w.x; drag.y=w.y; drag.vx=0; drag.vy=0; }
  else if(pan){ view.x=pan.vx+(e.clientX-pan.x); view.y=pan.vy+(e.clientY-pan.y); }
  else { const n=pick(e.clientX,e.clientY); hoverNode=n;
    if(n){ tip.style.display="block"; tip.style.left=(e.clientX+12)+"px"; tip.style.top=(e.clientY+12)+"px";
      tip.textContent=n.p+"  ("+n.d+" dependents)"; } else tip.style.display="none"; } });
addEventListener("mouseup", ()=>{ drag=null; pan=null; });
canvas.addEventListener("wheel", e=>{ e.preventDefault();
  const s=Math.exp(-e.deltaY*0.001), mx=e.clientX, my=e.clientY;
  view.x=mx-(mx-view.x)*s; view.y=my-(my-view.y)*s; view.k*=s; }, {passive:false});
document.getElementById("search").addEventListener("input", e=>{ query=e.target.value.toLowerCase(); });
</script>
</body>
</html>`
