import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import vuka from './vuka/vite.js'

export default defineConfig({
  plugins: [react(), vuka()],
})
