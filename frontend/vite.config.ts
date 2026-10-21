import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// 本机开发默认走 127.0.0.1:3000，真机/局域网调试用 SMARTGATE_DEV_TARGET 覆盖
// 例如：SMARTGATE_DEV_TARGET=http://192.168.1.1:3000 npm run dev
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const target =
    env.SMARTGATE_DEV_TARGET ||
    env.VITE_DEV_PROXY ||
    'http://127.0.0.1:3000'

  return {
    plugins: [react(), tailwindcss()],
    server: {
      port: 5173,
      proxy: {
        '/api': {
          target,
          changeOrigin: true,
          ws: true,
        },
        '/healthz': {
          target,
          changeOrigin: true,
        },
      },
    },
  }
})
