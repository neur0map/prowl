/** @type {import('tailwindcss').Config} */
export default {
    content: ['./src/**/*.{astro,html,js,jsx,md,mdx,svelte,ts,tsx,vue}'],
    theme: {
        extend: {
            colors: {
                // Home Depot Orange palette
                'hd-orange': '#F96302',
                'hd-orange-bright': '#FF7519',
                'hd-orange-dark': '#D94F00',
                'hd-orange-burnt': '#B8400A',
                // Warm neutrals
                'charcoal': '#1A1915',
                'graphite': '#2D2A24',
                'slate': '#3D3930',
                'ash': '#5C554A',
                // Light canvas
                'cream': '#FDF8F3',
                'cream-warm': '#F9F3EB',
                'cream-deep': '#F0E8DC',
                // Accent colors
                'accent-teal': '#0D7377',
                'accent-rust': '#A84520',
                'accent-forest': '#2D5A27',
                'accent-navy': '#1E3A5F',
            },
            fontFamily: {
                'display': ['Archivo Black', 'Impact', 'sans-serif'],
                'sans': ['Work Sans', 'system-ui', 'sans-serif'],
                'mono': ['Space Mono', 'Courier New', 'monospace'],
            },
            borderWidth: {
                '3': '3px',
            },
            boxShadow: {
                'hard': '4px 4px 0 #1A1915',
                'hard-lg': '6px 6px 0 #1A1915',
                'orange': '4px 4px 0 #F96302',
            },
        }
    },
    plugins: [],
};
