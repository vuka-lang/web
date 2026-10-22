import type { Plugin } from 'vite'

/** The path of the dev server's hot file under the build's output directory. */
export const HOT_FILE: string
/** The global holding the values the app passes the page (web.FrontendEnv). */
export const ENV_GLOBAL: string
/** The env var naming the island-build spec file; set by vuka build/run. */
export const ISLAND_ENV: string

/**
 * The Vite plugin for frontends served by a github.com/vuka-lang/web/v2 app:
 * it announces the dev server to the app, writes the build manifest the app
 * caches by, and turns import.meta.env.KEY into the app's value for KEY.
 */
export default function vuka(): Plugin

/** The dev server's origin for the address it listens on. */
export function devOrigin(addr: { address: string; port: number }, https: boolean): string
