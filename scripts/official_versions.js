#!/usr/bin/env node
// official_versions.js — 一次读出官方 WorkBuddy 两域（CN / global）当前的
// 桌面端版本、内置 CLI 版本与安装路径，用于同步 w2a 的内置版本号。
//
// 用法：
//   node scripts/official_versions.js                        # 自动探测 %USERPROFILE%
//   node scripts/official_versions.js --root D:/PG/workbuddyAI --home C:/Users/me
//
// 为什么要读这么多处：官方把版本号分散在 4 个文件里，且两域**发布节奏不保证同步**
// （2026-10-29 实测两域均为 5.9.3，此前 2026-10 曾出现 CN 5.8.3 / global 5.8.4 不同步）。
// 任何一处都不能当唯一真源——
//   ~/.workbuddy/last-launch.json            CN 桌面端运行时版本（最贴近实际发包）
//   ~/.workbuddy-ai/last-launch.json         global 同上
//   <install>/resources/install-manifest.json appVersion
//   <install>/resources/app.asar!/cli/product.json          genieVersion + isOversea + endpoint
//   <install>/resources/app.asar!/cli/package.json          version（CLI；5.9.3 起
//                                                          publishConfig.customPackage 已移除）
//
// 本脚本只读不写，输出可直接对照 internal/upstream/headers.go 的
// defaultClientVersionCN / defaultClientVersionGlobal / defaultCliVersion。
'use strict';
const fs = require('fs');
const path = require('path');

function arg(name, def) {
  const i = process.argv.indexOf(name);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
}

const home = arg('--home', process.env.USERPROFILE || process.env.HOME || '');
const roots = [];
const rootArg = arg('--root', '');
if (rootArg) roots.push(rootArg);
// 常见安装位置：用户显式目录 + 两个默认盘位。找不到不报错，只少一行输出。
for (const guess of [
  'D:/PG/workbuddyAI', 'D:/PG/workbuddy', 'C:/Program Files/WorkBuddy', 'C:/Program Files/WorkBuddy AI',
  path.join(home, 'AppData/Local/Programs/WorkBuddy'), path.join(home, 'AppData/Local/Programs/workbuddy'),
]) {
  if (fs.existsSync(guess) && !roots.includes(guess)) roots.push(guess);
}

function readJSON(p) {
  try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch { return null; }
}

// ---- asar 最小读取（与 asar_probe.js 同源逻辑，不引依赖）----
function openAsar(p) {
  const fd = fs.openSync(p, 'r');
  const head = Buffer.alloc(16);
  fs.readSync(fd, head, 0, 16, 0);
  const headerSize = head.readUInt32LE(12);
  const raw = Buffer.alloc(headerSize);
  fs.readSync(fd, raw, 0, headerSize, 16);
  const index = JSON.parse(raw.toString('utf8').replace(/\0+$/, ''));
  const base = 8 + headerSize;
  const map = new Map();
  (function walk(node, prefix) {
    for (const [name, e] of Object.entries(node.files || {})) {
      const q = prefix + '/' + name;
      if (e.files) walk(e, q);
      else map.set(q, { size: e.size || 0, offset: Number(e.offset || 0), unpacked: !!e.unpacked });
    }
  })(index, '');
  return { fd, base, map, dir: path.dirname(p), name: path.basename(p) };
}

function asarRead(a, internal) {
  const e = a.map.get(internal);
  if (!e) return null;
  if (e.unpacked) return fs.readFileSync(path.join(a.dir, a.name + '.unpacked', internal));
  const b = Buffer.alloc(e.size);
  fs.readSync(a.fd, b, 0, e.size, a.base + e.offset);
  return b;
}

function asarJSON(a, internal) {
  const b = asarRead(a, internal);
  if (!b) return null;
  try { return JSON.parse(b.toString('utf8')); } catch { return null; }
}

const out = [];
const push = (k, v) => { if (v !== undefined && v !== null && v !== '') out.push([k, String(v)]); };

// ---- 1) 运行时 last-launch.json（两域各一份）----
for (const [label, dir] of [['CN', '.workbuddy'], ['global', '.workbuddy-ai']]) {
  const p = path.join(home, dir, 'last-launch.json');
  const j = readJSON(p);
  if (j) push(`runtime.${label}.version`, j.version);
}

// ---- 2) 安装体（可能一台机器装两域，共用一份 asar）----
for (const root of roots) {
  const res = path.join(root, 'resources');
  if (!fs.existsSync(res)) continue;
  const manifest = readJSON(path.join(res, 'install-manifest.json'));
  if (manifest && manifest.appVersion) push(`install.${root}.appVersion`, manifest.appVersion);

  const asarPath = path.join(res, 'app.asar');
  if (!fs.existsSync(asarPath)) continue;
  let a;
  try { a = openAsar(asarPath); } catch (e) { push(`install.${root}.asar`, `read failed: ${e.message}`); continue; }

  const product = asarJSON(a, '/cli/product.json');
  if (product) {
    push(`install.${root}.productName`, product.productName);
    push(`install.${root}.applicationName`, product.applicationName);
    push(`install.${root}.isOversea`, product.isOversea);
    push(`install.${root}.endpoint`, product.endpoint);
    push(`install.${root}.genieVersion`, product.genieVersion);
  }
  const cliPkg = asarJSON(a, '/cli/package.json');
  // 5.9.3 起官方把顶层 version 填成真实 CLI 版本（0.0.0 是未填充占位），
  // 旧的 publishConfig.customPackage.version 通道已移除；两条都读以兼容老包。
  const cliVer = (cliPkg && cliPkg.version && cliPkg.version !== '0.0.0')
    ? cliPkg.version
    : (cliPkg && cliPkg.publishConfig && cliPkg.publishConfig.customPackage
      ? cliPkg.publishConfig.customPackage.version : null);
  if (cliVer) push(`install.${root}.cliVersion`, cliVer);
  a.fd && fs.closeSync(a.fd);
}

// ---- 3) 注册表（仅 Windows；读不到就跳过）----
if (process.platform === 'win32') {
  try {
    const { execFileSync } = require('child_process');
    const ps = [
      "$p=@('HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\*',",
      "'HKLM:\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\*');",
      "Get-ItemProperty $p -ErrorAction SilentlyContinue |",
      "Where-Object { $_.DisplayName -like '*WorkBuddy*' } |",
      "ForEach-Object { $_.DisplayName + '|' + $_.DisplayVersion + '|' + $_.DisplayIcon }",
    ].join(' ');
    const s = execFileSync('powershell', ['-NoProfile', '-Command', ps], { encoding: 'utf8' });
    for (const line of s.split(/\r?\n/)) {
      const [name, ver, icon] = line.split('|');
      if (name && ver) push(`registry.${name}`, `${ver}  ${icon || ''}`.trim());
    }
  } catch { /* 注册表不可用是正常情况（非 Windows / 权限） */ }
}

if (!out.length) {
  console.error('未发现任何官方安装痕迹。用 --root <安装目录> --home <用户目录> 指定。');
  process.exit(1);
}
const w = Math.max(...out.map(([k]) => k.length));
for (const [k, v] of out) console.log(`${k.padEnd(w)}  ${v}`);
