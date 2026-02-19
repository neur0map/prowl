import { defineConfig } from 'electron-vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import wasm from 'vite-plugin-wasm'
import topLevelAwait from 'vite-plugin-top-level-await'
import { viteStaticCopy } from 'vite-plugin-static-copy'
import { resolve } from 'path'

export default defineConfig({
  main: {
    build: {
      outDir: 'dist/main',
      rollupOptions: {
        input: {
          index: resolve(__dirname, 'electron/main.ts')
        },
        external: ['chokidar', 'ws']
      }
    }
  },
  preload: {
    build: {
      outDir: 'dist/preload',
      lib: {
        entry: resolve(__dirname, 'electron/preload.ts'),
      },
      rollupOptions: {
        output: {
          format: 'cjs',
          entryFileNames: 'index.js',
        },
      },
    }
  },
  renderer: {
    root: '.',
    build: {
      outDir: 'dist/renderer',
      rollupOptions: {
        input: {
          index: resolve(__dirname, 'index.html')
        }
      }
    },
    plugins: [
      react(),
      tailwindcss(),
      wasm(),
      topLevelAwait(),
      viteStaticCopy({
        targets: [
          {
            src: 'node_modules/kuzu-wasm/kuzu_wasm_worker.js',
            dest: 'assets'
          }
        ]
      }),
    ],
    resolve: {
      alias: {
        '@': resolve(__dirname, './src'),
        '@anthropic-ai/sdk/lib/transform-json-schema': resolve(__dirname, 'node_modules/@anthropic-ai/sdk/lib/transform-json-schema.mjs'),
        'mermaid': resolve(__dirname, 'node_modules/mermaid/dist/mermaid.esm.min.mjs'),
      },
    },
    define: {
      global: 'globalThis',
    },
    optimizeDeps: {
      exclude: ['kuzu-wasm'],
      include: ['buffer'],
    },
    worker: {
      format: 'es' as const,
      plugins: () => [wasm(), topLevelAwait()],
    },
  }
})
