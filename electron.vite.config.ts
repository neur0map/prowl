import { defineConfig } from 'electron-vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import wasm from 'vite-plugin-wasm'
import topLevelAwait from 'vite-plugin-top-level-await'
import { viteStaticCopy } from 'vite-plugin-static-copy'
import { resolve } from 'path'
import { readFileSync } from 'fs'
import type { Plugin } from 'vite'

/** Wrap the CJS `events` polyfill as a proper ESM module with named exports */
function eventsEsmPlugin(): Plugin {
  return {
    name: 'events-esm-shim',
    enforce: 'pre',
    resolveId(source) {
      if (source === 'events') return '\0events-esm'
    },
    load(id) {
      if (id === '\0events-esm') {
        const code = readFileSync(resolve(__dirname, 'node_modules/events/events.js'), 'utf-8')
        return `var module = { exports: {} };\nvar exports = module.exports;\n(function(module, exports) {\n${code}\n})(module, exports);\nvar EventEmitter = module.exports;\nexport { EventEmitter };\nexport default EventEmitter;\n`
      }
    },
  }
}

const pkg = JSON.parse(readFileSync(resolve(__dirname, 'package.json'), 'utf-8'))

export default defineConfig({
  main: {
    build: {
      outDir: 'dist/main',
      rollupOptions: {
        input: {
          index: resolve(__dirname, 'electron/main.ts')
        },
        external: ['chokidar', 'ws', 'node-pty']
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
      commonjsOptions: {
        transformMixedEsModules: true,
        include: [/node_modules/],
      },
      rollupOptions: {
        input: {
          index: resolve(__dirname, 'index.html')
        },
        output: {
          manualChunks: {
            'vendor-graph': ['graphology', 'graphology-communities-louvain', '@xyflow/react'],
            'vendor-editor': ['@monaco-editor/react'],
            'vendor-mermaid': ['mermaid'],
            'vendor-react': ['react', 'react-dom'],
          }
        }
      },
    },
    plugins: [
      eventsEsmPlugin(),
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
      'import.meta.env.VITE_APP_VERSION': JSON.stringify(pkg.version),
    },
    optimizeDeps: {
      noDiscovery: true,
      exclude: ['kuzu-wasm'],
      include: [
        'base64-js',
        'buffer',
        'camelcase',
        'comlink',
        'decamelize',
        'events',
        'extend',
        'lowlight',
        'lowlight/lib/core',
        'lru-cache',
        'minisearch',
        'msgpackr',
        'p-queue',
        'semver',
        'style-to-js',
        'web-tree-sitter',
        'zod',

        'graphology',
        'graphology-communities-louvain',
        'graphology-utils/defaults',
        'graphology-utils/is-graph',
        'graphology-utils/infer-type',
        'graphology-indices/louvain',
        'mnemonist/sparse-map',
        'mnemonist/sparse-queue-set',
        'pandemonium/random-index',
        'pandemonium/random',

        '@xyflow/react',
        '@langchain/core/messages',
        '@langchain/core/language_models/chat_models',
        '@langchain/core/tools',
        '@langchain/anthropic',
        '@langchain/openai',
        '@langchain/google-genai',
        '@langchain/ollama',
        '@langchain/langgraph/prebuilt',

        '@huggingface/transformers',

        'isomorphic-git',
        'isomorphic-git/http/web',
        '@isomorphic-git/lightning-fs',

        'react',
        'react-dom/client',
        'react-markdown',
        'react-syntax-highlighter',
        'react-syntax-highlighter/dist/esm/styles/prism',
        'remark-gfm',
        'lucide-react',

        '@monaco-editor/react',
        'monaco-editor',

        '@xterm/xterm',
        '@xterm/addon-fit',
        '@xterm/addon-webgl',
      ],
    },
    worker: {
      format: 'es' as const,
      plugins: () => [
        eventsEsmPlugin(),
        wasm(),
        topLevelAwait(),
      ],
    },
  }
})
