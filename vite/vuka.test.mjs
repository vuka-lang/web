// node --test vite/
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import vuka, { HOT_FILE, devOrigin } from './vuka.js'

function project(extra = {}) {
  const root = mkdtempSync(join(tmpdir(), 'vuka-vite-'))
  const config = { root, base: '/', command: 'serve', envPrefix: 'VITE_', define: {}, server: {}, build: { outDir: 'dist' }, ...extra }
  return { root, config, hot: join(root, 'dist', HOT_FILE) }
}

function fakeServer(address) {
  const http = new EventEmitter()
  http.address = () => address
  return { httpServer: http }
}

test('the hot file names the dev server while it runs', async () => {
  const { root, config, hot } = project()
  const p = vuka()
  p.configResolved(config)
  const server = fakeServer({ address: '::1', port: 5174 })
  p.configureServer(server)
  assert.equal(existsSync(hot), false)
  server.httpServer.emit('listening')
  const data = JSON.parse(readFileSync(hot, 'utf8'))
  assert.deepEqual(data, { origin: 'http://localhost:5174', base: '/', pid: process.pid })
  assert.equal(config.server.origin, 'http://localhost:5174', 'asset URLs point at the dev server')

  rmSync(hot) // a vite build emptied the output directory
  await new Promise((r) => setTimeout(r, 1200))
  assert.equal(existsSync(hot), true, 'put back while the server runs')

  server.httpServer.emit('close')
  assert.equal(existsSync(hot), false)
  rmSync(root, { recursive: true })
})

test('a hot file of another process is left alone', () => {
  const { root, config, hot } = project({ base: '/admin/', server: { origin: 'http://dev.test:5173' } })
  const p = vuka()
  p.configResolved(config)
  const server = fakeServer({ address: '0.0.0.0', port: 5173 })
  p.configureServer(server)
  server.httpServer.emit('listening')
  assert.deepEqual(JSON.parse(readFileSync(hot, 'utf8')), { origin: 'http://dev.test:5173', base: '/admin/', pid: process.pid })
  writeFileSync(hot, JSON.stringify({ origin: 'http://localhost:5199', pid: 1 }))
  server.httpServer.emit('close')
  assert.equal(existsSync(hot), true)
  rmSync(root, { recursive: true })
})

test('devOrigin', () => {
  assert.equal(devOrigin({ address: '127.0.0.1', port: 1 }, false), 'http://localhost:1')
  assert.equal(devOrigin({ address: '192.168.1.4', port: 2 }, true), 'https://192.168.1.4:2')
  assert.equal(devOrigin({ address: 'fe80::1', port: 3 }, false), 'http://[fe80::1]:3')
})

test('vite build writes the manifest and keeps .gitkeep', () => {
  const { root, config } = project({ command: 'build' })
  const p = vuka()
  assert.deepEqual(p.config({}, { command: 'build' }), { build: { manifest: true } })
  assert.equal(p.config({}, { command: 'serve' }), undefined)
  p.configResolved(config)
  mkdirSync(join(root, 'dist'), { recursive: true })
  writeFileSync(join(root, 'dist', '.gitkeep'), '')
  p.buildStart()
  rmSync(join(root, 'dist'), { recursive: true }) // emptyOutDir
  p.closeBundle()
  assert.equal(existsSync(join(root, 'dist', '.gitkeep')), true)
  rmSync(root, { recursive: true })
})

test('import.meta.env reads the values the app passes', () => {
  const { config } = project({ envPrefix: ['VITE_', 'PUBLIC_'], define: { 'import.meta.env.BUILT': '"x"' } })
  const p = vuka()
  p.configResolved(config)
  const src = 'const a = import.meta.env.API_URL, b = import.meta.env.MODE, c = import.meta.env.VITE_X, d = import.meta.env.PUBLIC_Y, e = import.meta.env.BUILT, f = import.meta.env'
  assert.equal(
    p.transform(src, '/p/src/main.tsx').code,
    'const a = globalThis.__VUKA_ENV__?.API_URL, b = import.meta.env.MODE, c = import.meta.env.VITE_X, d = import.meta.env.PUBLIC_Y, e = import.meta.env.BUILT, f = import.meta.env',
  )
  assert.equal(p.transform('import.meta.env.MODE', '/p/src/a.ts'), undefined)
  assert.equal(p.transform('import.meta.env.API_URL', '/p/node_modules/lib/index.js'), undefined)
  assert.equal(p.transform('import.meta.env.API_URL', '/p/src/a.css'), undefined)
  assert.match(p.transform('<script setup>const u = import.meta.env.API_URL</script>', '/p/src/App.vue').code, /__VUKA_ENV__\?\.API_URL/)
})
