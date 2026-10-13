// The Vite plugin for frontends served by a github.com/vuka-lang/web app
// (web.Frontend). vuka run and vuka build copy it into the frontend's
// directory as vuka/vite.js; in vite.config:
//
//	import vuka from './vuka/vite.js'
//	export default defineConfig({ plugins: [react(), vuka()] })
//
// It does three things:
//
//   - While the dev server runs it writes <outDir>/.vite/vuka-hot.json with
//     the server's origin, base and pid, and removes it when the server
//     stops. A development app finds the file beside the build it serves and
//     loads its pages' modules from the dev server, so the browser stays on
//     the app's address. It also sets server.origin, so the URLs of assets
//     the modules import point at the dev server.
//   - vite build writes the manifest (.vite/manifest.json): the app caches
//     the files it lists for good. A .gitkeep in the output directory,
//     which emptyOutDir removes, is put back.
//   - import.meta.env.KEY, for a KEY that isn't Vite's own (MODE, DEV,
//     PROD, SSR, BASE_URL, those with envPrefix, those in define), reads the
//     value the app passes the page (web.FrontendEnv), at load time.
//
// It also drives the UI-island build. When vuka build (or vuka run) runs
// `vite build` with VUKA_ISLAND_BUILD set to a spec file, the plugin turns
// the user's own Vite project — their [vue(), ui(), tailwindcss()] plugins
// intact — into a multi-entry island build: it replaces rollupOptions.input
// with the island entries vuka generated (a framework adapter, one
// `export {default}` entry per @ui.Island component, and a Tailwind/UI CSS
// entry), redirects the output to vuka's own temp directory, and forces the
// manifest on. vuka then reads that manifest and serves the island chunks and
// CSS from /_vuka/i/. This is how an island reaches a Vite-plugin UI library
// (e.g. Nuxt UI) whose components and Tailwind v4 CSS only exist after its
// Vite plugin runs. The spec is {outDir, input:{key:relPath}}.
//
// No dependencies but Node's own modules.

import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'

export const HOT_FILE = '.vite/vuka-hot.json'
export const ENV_GLOBAL = '__VUKA_ENV__'
/** The env var naming the island-build spec file; set by vuka build/run. */
export const ISLAND_ENV = 'VUKA_ISLAND_BUILD'

/** The island-build spec vuka wrote, or null when not an island build. */
function islandSpec() {
  const file = process.env[ISLAND_ENV]
  if (!file) return null
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

const VITE_ENV = new Set(['MODE', 'DEV', 'PROD', 'SSR', 'BASE_URL'])
const ENV_REF = /\bimport\.meta\.env\.([A-Za-z_$][\w$]*)/g
const SCRIPT = /\.(?:[cm]?[jt]sx?|vue|svelte|astro)(?:\?|$)/

/** @returns {import('vite').Plugin} */
export default function vuka() {
  let config
  let hotPath = ''
  let keep = false // the output directory had a .gitkeep
  const envKeep = (key) =>
    VITE_ENV.has(key) ||
    [].concat(config?.envPrefix ?? 'VITE_').some((p) => p && key.startsWith(p)) ||
    Object.keys(config?.define ?? {}).includes('import.meta.env.' + key)

  return {
    name: 'vuka',
    enforce: 'pre',

    config(cfg, { command }) {
      const spec = islandSpec()
      if (spec) {
        // An island build: keep the user's plugins, replace the entry set
        // with vuka's island entries, and send the output to vuka's temp dir.
        cfg.build = cfg.build || {}
        cfg.build.manifest = true
        cfg.build.outDir = spec.outDir
        cfg.build.emptyOutDir = true
        cfg.build.copyPublicDir = false
        const ro = (cfg.build.rollupOptions = cfg.build.rollupOptions || {})
        ro.input = spec.input
        if (ro.preserveEntrySignatures == null) ro.preserveEntrySignatures = 'allow-extension'
        ro.output = {
          ...(ro.output || {}),
          format: 'es',
          entryFileNames: '[name].[hash].js',
          chunkFileNames: 'chunk.[hash].js',
          assetFileNames: '[name].[hash][extname]',
        }
        return
      }
      if (command === 'build') return { build: { manifest: true } }
    },

    configResolved(resolved) {
      config = resolved
      hotPath = join(resolve(config.root, config.build.outDir), HOT_FILE)
    },

    configureServer(server) {
      const http = server.httpServer
      if (!http) return // middleware mode: the host serves the pages
      let written = null
      const write = () => {
        const addr = http.address()
        if (!addr || typeof addr === 'string') return
        const origin = config.server.origin || devOrigin(addr, !!config.server.https)
        if (!config.server.origin) config.server.origin = origin
        written = { origin, base: config.base || '/', pid: process.pid }
        writeHot(hotPath, written)
      }
      const remove = () => {
        if (written) removeHot(hotPath, process.pid)
        written = null
      }
      http.once('listening', write)
      http.once('close', remove)
      process.once('exit', remove)
      // A vite build of the same project empties the output directory: put
      // the file back while this server runs.
      const timer = setInterval(() => {
        if (written && !existsSync(hotPath)) writeHot(hotPath, written)
      }, 1000)
      timer.unref?.()
      http.once('close', () => clearInterval(timer))
    },

    buildStart() {
      if (config.command === 'build') keep = existsSync(join(resolve(config.root, config.build.outDir), '.gitkeep'))
    },

    closeBundle() {
      const out = resolve(config.root, config.build.outDir)
      if (config.command === 'build' && keep && !existsSync(join(out, '.gitkeep'))) {
        mkdirSync(out, { recursive: true })
        writeFileSync(join(out, '.gitkeep'), '')
      }
    },

    transform(code, id) {
      if (!code.includes('import.meta.env.') || id.includes('/node_modules/') || id.startsWith('\0') || !SCRIPT.test(id)) return
      let changed = false
      const out = code.replace(ENV_REF, (ref, key) => {
        if (envKeep(key)) return ref
        changed = true
        return `globalThis.${ENV_GLOBAL}?.${key}`
      })
      if (changed) return { code: out, map: null }
    },
  }
}

/** The dev server's origin, as the browser reaches it. */
export function devOrigin(addr, https) {
  let host = addr.address
  if (['::', '0.0.0.0', '::1', '127.0.0.1', '::ffff:127.0.0.1'].includes(host)) host = 'localhost'
  else if (host.includes(':')) host = `[${host}]`
  return `${https ? 'https' : 'http'}://${host}:${addr.port}`
}

function writeHot(file, data) {
  mkdirSync(dirname(file), { recursive: true })
  const tmp = `${file}.${process.pid}.tmp`
  writeFileSync(tmp, JSON.stringify(data, null, 2) + '\n')
  renameSync(tmp, file) // the app never reads half a file
}

function removeHot(file, pid) {
  try {
    if (JSON.parse(readFileSync(file, 'utf8')).pid === pid) unlinkSync(file)
  } catch {}
}
