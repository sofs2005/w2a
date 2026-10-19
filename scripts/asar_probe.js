#!/usr/bin/env node
// asar_probe.js — 读取 Electron asar 归档：索引、单文件、或按关键字扫描。
//
// 用法（不依赖 cwd，路径用参数传）：
//   node scripts/asar_probe.js list   <app.asar> [pathPrefix]
//   node scripts/asar_probe.js cat    <app.asar> <internal/path>       # 打印文件内容
//   node scripts/asar_probe.js scan   <app.asar> <needle> [ctx] [max]  # 顺序扫描文本文件
//
// asar 格式：前 16 字节头里偏移 12 处 uint32 = JSON 索引长度；数据区从 8+len 开始。
// `unpacked: true` 的节点不在 asar 内，需读同级 `app.asar.unpacked/<path>`。
//
// 为什么用 indexOf 而不是 RegExp：带 g 标志的 exec 不重置 lastIndex 会死循环，
// 而且跨文件复用同一个正则更容易踩这个坑。
'use strict';
const fs = require('fs');
const path = require('path');

function openAsar(p) {
  const fd = fs.openSync(p, 'r');
  const head = Buffer.alloc(16);
  fs.readSync(fd, head, 0, 16, 0);
  const headerSize = head.readUInt32LE(12);
  const raw = Buffer.alloc(headerSize);
  fs.readSync(fd, raw, 0, headerSize, 16);
  const index = JSON.parse(raw.toString('utf8').replace(/\0+$/, ''));
  const base = 8 + headerSize;
  const entries = [];
  (function walk(node, prefix) {
    for (const [name, e] of Object.entries(node.files || {})) {
      const p = prefix + '/' + name;
      if (e.files) walk(e, p);
      else entries.push({ path: p, size: e.size || 0, offset: Number(e.offset || 0), unpacked: !!e.unpacked });
    }
  })(index, '');
  return { fd, base, entries };
}

function readEntry(asarPath, a, entry) {
  if (entry.unpacked) {
    const unpackedRoot = path.join(path.dirname(asarPath), path.basename(asarPath) + '.unpacked');
    return fs.readFileSync(path.join(unpackedRoot, entry.path));
  }
  const b = Buffer.alloc(entry.size);
  fs.readSync(a.fd, b, 0, entry.size, a.base + entry.offset);
  return b;
}

const [cmd, asarPath, ...rest] = process.argv.slice(2);
if (!cmd || !asarPath) {
  console.error('usage: asar_probe.js list|cat|scan <app.asar> [...]');
  process.exit(2);
}
const a = openAsar(asarPath);

if (cmd === 'list') {
  // 索引里的路径一律以 '/' 开头；这里把用户传的 "cli/" / "/cli/" 归一成同一种，
  // 否则最常见的两种写法都会静默返回空列表。
  const raw = rest[0] || '';
  const prefix = raw === '' ? '' : raw.startsWith('/') ? raw : '/' + raw;
  for (const e of a.entries) {
    if (e.path.startsWith(prefix)) console.log(`${e.size}\t${e.unpacked ? 'unpacked' : 'asar'}\t${e.path}`);
  }
} else if (cmd === 'cat') {
  const raw = rest[0];
  // 索引路径带前导 '/'，而 README/习惯写法通常不带——两种都接受。
  // 注意：不能拿它当裸相对路径传给 path.join，Windows 下会被解析到别处。
  const target = raw.startsWith('/') ? raw : '/' + raw;
  const entry = a.entries.find(e => e.path === target);
  if (!entry) {
    console.error(`not found: ${target}`);
    process.exit(1);
  }
  process.stdout.write(readEntry(asarPath, a, entry));
} else if (cmd === 'scan') {
  const needle = rest[0];
  const ctx = parseInt(rest[1] || '200', 10);
  const maxHitsPerFile = parseInt(rest[2] || '5', 10);
  if (!needle) {
    console.error('scan needs a needle');
    process.exit(2);
  }
  let hits = 0;
  const hitFiles = new Set();
  for (const e of a.entries) {
    if (!/\.(js|mjs|cjs|json|ts|html)$/.test(e.path)) continue;
    if (e.size > 40 * 1024 * 1024) continue; // 单个超大文件跳过，避免内存爆掉
    let s;
    try { s = readEntry(asarPath, a, e).toString('utf8'); } catch { continue; }
    let idx = s.indexOf(needle), n = 0;
    if (idx >= 0) hitFiles.add(e.path);
    while (idx >= 0 && n < maxHitsPerFile) {
      console.log(`=== ${e.path} @${idx}`);
      console.log(s.slice(Math.max(0, idx - ctx), idx + needle.length + ctx).replace(/\n/g, '⏎'));
      hits++; n++;
      idx = s.indexOf(needle, idx + needle.length);
    }
    if (hits > 80) break;
  }
  console.log(`TOTAL hits ${hits} in ${hitFiles.size} file(s)`);
} else {
  console.error(`unknown command: ${cmd}`);
  process.exit(2);
}
