// Минимальный статический сервер для e2e MOCK-режима: раздаёт ../web с
// SPA-fallback на index.html (как nginx try_files $uri /index.html).
// Без зависимостей — только node stdlib. /api/* здесь НЕ обслуживается:
// эти запросы перехватывает Playwright route-mock (fixtures/api-mock.js).

import http from 'node:http';
import { createReadStream, existsSync, statSync } from 'node:fs';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = fileURLToPath(new URL('.', import.meta.url));
const ROOT = join(__dirname, '..', 'web');
const PORT = Number(process.argv[2] || 4173);

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.svg': 'image/svg+xml',
  '.mp3': 'audio/mpeg',
};

const server = http.createServer((req, res) => {
  const urlPath = decodeURIComponent((req.url || '/').split('?')[0]);
  // защита от traversal: нормализуем и запрещаем выход за ROOT
  const rel = normalize(urlPath).replace(/^(\.\.(\/|\\|$))+/, '');
  let file = join(ROOT, rel);

  if (!file.startsWith(ROOT)) {
    res.writeHead(403).end('forbidden');
    return;
  }
  // каталог или несуществующий путь → SPA fallback на index.html
  if (!existsSync(file) || statSync(file).isDirectory()) {
    file = join(ROOT, 'index.html');
  }
  const type = MIME[extname(file)] || 'application/octet-stream';
  res.writeHead(200, { 'content-type': type });
  createReadStream(file).pipe(res);
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`static web/ on http://127.0.0.1:${PORT}`);
});
