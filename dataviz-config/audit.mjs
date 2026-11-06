#!/usr/bin/env node
// audit.mjs — 图表 HTML 的几何审计
//
// 用途：在没有渲染器（rsvg / inkscape / chromium / playwright）的机器上，
// 用坐标级检查替代「把图渲染出来肉眼看一遍」。它只管几何，不管观感：
// 字体回退、暗色实拍、版式好不好看，一律要像素，这里看不到。
//
// 跑法：node audit.mjs <file.html> [more.html ...]
// 退出码：0 全过；1 有 FAIL。
//
// 每个 <svg> 独立审六项：
//   A  4px 栅格     节点/带/片/遮罩的原点与宽高被 4 整除（铺满画布的背景除外）
//   B  文本不溢出   按字宽预算算实际宽度，验它在内层容器里左右各留 4px
//   C  遮罩间隙     标签遮罩离每条连线 ≥6px，遮罩不能压住自己的线
//   D  遮罩不压节点 与节点相交却没完全落在节点内即失败（节点内的徽章遮罩合法）
//   E  扇出端口     同一节点同一条边上的连接线端点间距 ≥12px
//   F  画布内       所有元素落在 viewBox 里
//
// 字宽预算取自 diagram-design 的 style-guide.md：全角/宽字符 1em，
// 窄字符 sans 0.60em、mono 0.62em，letter-spacing 另加。
//
// 角色解析：元素上的 data-role 优先（mask / node），
// 否则按下面的类名表回退——skill 模板那套类名够用，
// 换了别的命名就显式写 data-role。

import { readFileSync } from 'node:fs';

const ROLE_CLASS = [
  // 顺序有意义：先认标签遮罩，再认节点。n-mask 是节点的不透明衬底，不是标签遮罩。
  [/lbl-?mask|label-?mask/, 'mask'],
  [/n-mask|(^|[-_\s])node($|[-_\s])/, 'node'],
];

const ADVANCE = { mono: 0.62, sans: 0.60 };
const WIDE = /[ᄀ-ᅟ⺀-〿぀-㏿㐀-䶿一-鿿ꀀ-꓏가-힯豈-﫿︰-﹏＀-｠￠-￦]/;
const TOL = 0.6;          // 端点贴边容差
const MASK_GAP = 6;       // §6 规则 2
const FAN_GAP = 12;       // §6 规则 4
const TEXT_PAD = 4;       // 文本在容器内的最小留白
const GRID = 4;           // §7 栅格

// ---------- 小工具 ----------
const attrs = (s) => Object.fromEntries([...s.matchAll(/([\w:-]+)="([^"]*)"/g)].map(m => [m[1], m[2]]));
const num = (v) => {
  const m = String(v ?? '').trim().match(/^(-?[\d.]+)(px|rem|em)?$/);
  if (!m) return null;
  const n = +m[1];
  return m[2] === 'rem' ? n * 16 : m[2] === 'em' ? n * 16 : n;
};
const emVal = (v) => {
  const m = String(v ?? '').trim().match(/^(-?[\d.]+)(px|em)?$/);
  return m ? { n: +m[1], unit: m[2] || 'px' } : null;
};

// ---------- CSS：把 <style> 里的单类规则收成表 ----------
function parseCSS(css) {
  const rules = [];
  for (const m of css.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
    const decls = {};
    for (const d of m[2].split(';')) {
      const [k, ...rest] = d.split(':');
      if (!k || !rest.length) continue;
      decls[k.trim()] = rest.join(':').trim();
    }
    for (const sel of m[1].split(',')) {
      const classes = [...sel.trim().matchAll(/\.([\w-]+)/g)].map(x => x[1]);
      if (!classes.length) continue;
      rules.push({ classes, decls, order: rules.length });
    }
  }
  return rules;
}

function cssFor(rules, classList) {
  const out = {};
  const hits = rules
    .filter(r => r.classes.every(c => classList.includes(c)))
    .sort((a, b) => a.order - b.order);
  for (const h of hits) Object.assign(out, h.decls);
  return out;
}

// ---------- 单个 svg 的解析 ----------
// CSS 在文档头里，不在 <svg> 里 —— 必须从全文取，否则字号全回落默认值。
function docCSS(src) {
  return parseCSS([...src.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map(m => m[1]).join('\n'));
}

function parseSVG(svg, rules) {
  const vb = svg.match(/viewBox="(-?[\d.]+)\s+(-?[\d.]+)\s+([\d.]+)\s+([\d.]+)"/);
  const viewBox = vb ? { x: +vb[1], y: +vb[2], w: +vb[3], h: +vb[4] } : null;

  const rects = [];
  for (const m of svg.matchAll(/<rect\b([^>]*?)\/>/g)) {
    const a = attrs(m[1]);
    if (a.x === undefined && !a.class) continue;
    const classList = (a.class ?? '').split(/\s+/).filter(Boolean);
    rects.push({
      cls: classList, role: a['data-role'] ?? '',
      x: +a.x, y: +a.y, w: +a.width, h: +a.height,
    });
  }

  const texts = [];
  for (const m of svg.matchAll(/<text\b([^>]*)>([\s\S]*?)<\/text>/g)) {
    const a = attrs(m[1]);
    const classList = (a.class ?? '').split(/\s+/).filter(Boolean);
    const css = cssFor(rules, classList);
    const size = num(a['font-size'] ?? css['font-size']) ?? 12;
    const ls = emVal(a['letter-spacing'] ?? css['letter-spacing']);
    const family = a['font-family'] ?? css['font-family'] ?? '';
    texts.push({
      cls: classList, s: m[2].replace(/<[^>]*>/g, '').trim(),
      x: +a.x, y: +a.y,
      anchor: a['text-anchor'] ?? css['text-anchor'] ?? 'start',
      size,
      lsEm: ls ? (ls.unit === 'em' ? ls.n : ls.n / size) : 0,
      kind: /mono/i.test(family) ? 'mono' : 'sans',
    });
  }

  // 连线：拆成线段用于间隙检查，同时保留真正的首末端点用于扇出检查
  const connectors = [];
  const seg = (x1, y1, x2, y2) => ({ x1, y1, x2, y2 });
  for (const m of svg.matchAll(/<line\b([^>]*?)\/>/g)) {
    const a = attrs(m[1]);
    if (a.x1 === undefined) continue;
    connectors.push({ segs: [seg(+a.x1, +a.y1, +a.x2, +a.y2)], ends: [{ x: +a.x1, y: +a.y1 }, { x: +a.x2, y: +a.y2 }] });
  }
  for (const m of svg.matchAll(/<path\b([^>]*?)d="([^"]+)"([^>]*?)\/>/g)) {
    const pts = [];
    let cx = 0, cy = 0;
    const tok = m[2].match(/[MHVQCSAZ]|-?[\d.]+/gi) ?? [];
    for (let i = 0; i < tok.length;) {
      const c = tok[i].toUpperCase();
      if (c === 'M' || c === 'L') { cx = +tok[i + 1]; cy = +tok[i + 2]; pts.push([cx, cy]); i += 3; }
      else if (c === 'H') { cx = +tok[i + 1]; pts.push([cx, cy]); i += 2; }
      else if (c === 'V') { cy = +tok[i + 1]; pts.push([cx, cy]); i += 2; }
      else if (c === 'Q') { cx = +tok[i + 3]; cy = +tok[i + 4]; pts.push([cx, cy]); i += 5; }
      else if (c === 'C') { cx = +tok[i + 5]; cy = +tok[i + 6]; pts.push([cx, cy]); i += 7; }
      else i++;
    }
    if (pts.length < 2) continue;
    const segs = [];
    for (let i = 1; i < pts.length; i++) segs.push(seg(pts[i - 1][0], pts[i - 1][1], pts[i][0], pts[i][1]));
    const e0 = pts[0], e1 = pts[pts.length - 1];
    connectors.push({ segs, ends: [{ x: e0[0], y: e0[1] }, { x: e1[0], y: e1[1] }] });
  }

  return { viewBox, rects, texts, connectors };
}

// ---------- 几何 ----------
const textWidth = (t) => {
  let em = 0;
  for (const ch of t.s) em += WIDE.test(ch) ? 1 : ADVANCE[t.kind];
  return em * t.size + t.s.length * t.lsEm * t.size;
};

const segDistToRect = (s, r) => {
  const ptRect = (px, py) => Math.hypot(Math.max(r.x - px, 0, px - (r.x + r.w)), Math.max(r.y - py, 0, py - (r.y + r.h)));
  const ptSeg = (px, py) => {
    const vx = s.x2 - s.x1, vy = s.y2 - s.y1, L2 = vx * vx + vy * vy;
    const t = L2 === 0 ? 0 : Math.max(0, Math.min(1, ((px - s.x1) * vx + (py - s.y1) * vy) / L2));
    return Math.hypot(px - (s.x1 + t * vx), py - (s.y1 + t * vy));
  };
  let d = Math.min(ptRect(s.x1, s.y1), ptRect(s.x2, s.y2));
  for (const [cx, cy] of [[r.x, r.y], [r.x + r.w, r.y], [r.x, r.y + r.h], [r.x + r.w, r.y + r.h]]) d = Math.min(d, ptSeg(cx, cy));
  return d;
};

const overlaps = (a, b) => a.x < b.x + b.w && a.x + a.w > b.x && a.y < b.y + b.h && a.y + a.h > b.y;
const contains = (outer, inner) => inner.x >= outer.x && inner.y >= outer.y
  && inner.x + inner.w <= outer.x + outer.w && inner.y + inner.h <= outer.y + outer.h;

const roleOf = (r) => {
  if (r.role) return r.role;
  for (const [re, name] of ROLE_CLASS) if (re.test(r.cls.join(' '))) return name;
  return '';
};

// ---------- 检查 ----------
function audit(svg, rules, label, out) {
  const { viewBox, rects, texts, connectors } = parseSVG(svg, rules);
  const say = (ok, msg) => { if (!ok) out.push(`${label}: ${msg}`); };

  if (!viewBox) { out.push(`${label}: 没有 viewBox`); return; }
  const { w: VW, h: VH } = viewBox;

  // A 4px 栅格 —— 铺满画布的背景不算布局件
  for (const r of rects) {
    if (r.w * r.h >= 0.95 * VW * VH) continue;
    for (const [k, v] of [['x', r.x], ['y', r.y], ['w', r.w], ['h', r.h]]) {
      say(Math.abs(v % GRID) < 1e-6, `A 栅格 ${r.cls.join('.') || '(no class)'} ${k}=${v} 不是 ${GRID} 的倍数`);
    }
  }

  // B 文本不溢出内层容器
  const containers = rects.filter(r => r.w * r.h < 0.95 * VW * VH);
  for (const t of texts) {
    if (!t.s) continue;
    const w = textWidth(t);
    const left = t.anchor === 'middle' ? t.x - w / 2 : t.anchor === 'end' ? t.x - w : t.x;
    const right = left + w;
    const host = containers
      .filter(r => t.x >= r.x - 1 && t.x <= r.x + r.w + 1 && t.y >= r.y && t.y <= r.y + r.h + 6)
      .sort((a, b) => a.w * a.h - b.w * b.h)[0];
    if (!host) continue;
    say(left >= host.x + TEXT_PAD,
      `B 文本溢出左 "${t.s}" left=${left.toFixed(1)} 容器=${host.cls.join('.') || '(no class)'}@${host.x}`);
    say(right <= host.x + host.w - TEXT_PAD,
      `B 文本溢出右 "${t.s}" right=${right.toFixed(1)} 容器端=${host.x + host.w}`);
  }

  // C 标签遮罩离每条连线 ≥ MASK_GAP
  const masks = rects.filter(r => roleOf(r) === 'mask');
  const allSegs = connectors.flatMap(c => c.segs);
  for (const m of masks) {
    let dmin = Infinity;
    for (const s of allSegs) dmin = Math.min(dmin, segDistToRect(s, m));
    say(dmin >= MASK_GAP,
      `C 遮罩 ${m.x},${m.y} ${m.w}x${m.h} 离最近连线只有 ${dmin === Infinity ? '∞' : dmin.toFixed(1)}px（要 ≥${MASK_GAP}）`);
  }

  // D 遮罩不得与节点相交却落在节点外（节点内的徽章遮罩合法）
  const nodes = rects.filter(r => roleOf(r) === 'node');
  for (const m of masks) {
    for (const n of nodes) {
      if (!overlaps(m, n)) continue;
      say(contains(n, m), `D 遮罩 ${m.x},${m.y} ${m.w}x${m.h} 压到节点 ${n.cls.join('.') || '(no class)'}@${n.x},${n.y} 却没完全落在里面`);
    }
  }

  // E 扇出端口：同一节点同一条边上的连线端点间距
  const ports = new Map();
  for (const n of nodes) {
    const edges = { top: [], right: [], bottom: [], left: [] };
    for (const c of connectors) for (const p of c.ends) {
      const onV = (Math.abs(p.x - n.x) < TOL || Math.abs(p.x - (n.x + n.w)) < TOL)
        && p.y >= n.y - TOL && p.y <= n.y + n.h + TOL;
      const onH = (Math.abs(p.y - n.y) < TOL || Math.abs(p.y - (n.y + n.h)) < TOL)
        && p.x >= n.x - TOL && p.x <= n.x + n.w + TOL;
      if (onV && Math.abs(p.x - n.x) < TOL) edges.left.push(p.y);
      else if (onV) edges.right.push(p.y);
      else if (onH && Math.abs(p.y - n.y) < TOL) edges.top.push(p.x);
      else if (onH) edges.bottom.push(p.x);
    }
    for (const [edge, coords] of Object.entries(edges)) {
      if (coords.length < 2) continue;
      coords.sort((a, b) => a - b);
      ports.set(`${n.x},${n.y}:${edge}`, coords);
      for (let i = 1; i < coords.length; i++) {
        say(coords[i] - coords[i - 1] >= FAN_GAP,
          `E 节点 ${n.cls.join('.') || '(no class)'}@${n.x},${n.y} ${edge} 边端口 ${coords[i - 1]}→${coords[i]} 间距不足 ${FAN_GAP}px`);
      }
    }
  }

  // F 全部在画布内
  for (const r of rects) {
    say(r.x >= viewBox.x && r.y >= viewBox.y && r.x + r.w <= viewBox.x + VW && r.y + r.h <= viewBox.y + VH,
      `F rect 出界 ${r.cls.join('.') || '(no class)'} ${r.x},${r.y} ${r.w}x${r.h}`);
  }
  for (const t of texts) {
    say(t.x >= viewBox.x && t.x <= viewBox.x + VW && t.y >= viewBox.y && t.y <= viewBox.y + VH,
      `F text 出界 "${t.s}" ${t.x},${t.y}`);
  }

  return {
    rects: rects.length, texts: texts.length,
    connectors: connectors.length, masks: masks.length, nodes: nodes.length,
  };
}

// ---------- main ----------
const files = process.argv.slice(2);
if (!files.length) {
  console.error('用法: node audit.mjs <file.html> [more.html ...]');
  process.exit(2);
}

let failed = 0;
for (const f of files) {
  const src = readFileSync(f, 'utf8');
  const rules = docCSS(src);
  const svgs = [...src.matchAll(/<svg\b[\s\S]*?<\/svg>/g)].map(m => m[0]);
  if (!svgs.length) { console.log(`SKIP ${f} — 没有 <svg>`); continue; }
  svgs.forEach((svg, i) => {
    const label = svgs.length > 1 ? `${f}#${i + 1}` : f;
    const out = [];
    const stat = audit(svg, rules, label, out);
    if (out.length) {
      failed += out.length;
      console.log(`FAIL ${label}`);
      for (const o of out) console.log('  - ' + o);
    } else {
      console.log(`OK   ${label}  (${stat.rects} rects · ${stat.texts} texts · ${stat.connectors} connectors · ${stat.masks} masks · ${stat.nodes} nodes)`);
    }
  });
}
process.exit(failed ? 1 : 0);
