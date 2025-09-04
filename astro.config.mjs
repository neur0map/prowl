import { defineConfig } from 'astro/config';

import tailwind from '@astrojs/tailwind';
import compress from 'astro-compress';
import sitemap from '@astrojs/sitemap';
import node from '@astrojs/node';

export default defineConfig({
    output: 'server',
    adapter: node({
        mode: 'standalone'
    }),
    trailingSlash: 'always',
    site: 'https://prowl.sh',

    // Single page, no prefetch needed
    prefetch: false,
    
    // Allow external hosts in preview mode
    vite: {
        preview: {
            allowedHosts: true
        }
    },

    integrations: [
        tailwind(),
        sitemap(),
        compress({
            CSS: true,
            SVG: false,
            Image: false,
            HTML: {
                "html-minifier-terser": {
                    collapseWhitespace: true,
                    // collapseInlineTagWhitespace: true, // It breaks display-inline / flex-inline text
                    minifyCSS: true,
                    minifyJS: true,
                    removeComments: true,
                    removeEmptyAttributes: true,
                    // removeEmptyElements: true, // It removes sometimes SVGs
                    removeRedundantAttributes: true
                },
            },
            JavaScript: {
                'terser': {
                    compress: {
                        drop_console: true,
                        drop_debugger: true,
                    }
                }
            }
        })
    ]
});