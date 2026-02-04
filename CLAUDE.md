# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Prowl is the marketing site for an indie dev studio (prowl.sh). It's a single-page Astro site showcasing products and open source tools built by @neur0map.

## Commands

```bash
npm run dev      # Start dev server at localhost:4321
npm run build    # Build static site to ./dist
npm run preview  # Preview production build
```

## Architecture

**Framework**: Astro 5.x with static output (no SSR)
**Styling**: TailwindCSS + SCSS (src/styles/global.scss)
**Build**: Outputs to ./dist, compressed via astro-compress

### Key Files

- `src/layouts/Layout.astro` - Base HTML template with all SEO meta tags, fonts, structured data
- `src/styles/global.scss` - Design system: colors, typography, animations, button styles
- `tailwind.config.js` - Extended theme with custom colors, fonts, shadows
- `astro.config.mjs` - Build config, compression settings, sitemap

### Page Structure

**Homepage** (`src/pages/index.astro`):
1. `Hero.astro` - Landing section with headline and brand mark
2. `Products.astro` - Commercial products showcase
3. `OpenSource.astro` - CLI tools with install commands, GitHub stars
4. `Footer.astro` - Links and SEO content

**Cramly Chrome Extension** (`src/pages/cramly/`):
- `index.astro` - Product showcase page for the Chrome extension
- `privacy.astro` - Privacy policy (required for Chrome Web Store)

### Design System

Colors defined in global.scss as CSS variables:
- Primary: `--hd-orange` (#F96302) - Home Depot orange
- Background: `--cream` (#FDF8F3) - Warm off-white
- Text: `--charcoal` (#1A1915), `--slate` (#3D3930)
- Accents: `--accent-teal`, `--accent-rust`, `--accent-forest`

Typography:
- Display: Archivo Black (headlines, uppercase)
- Body: Work Sans
- Mono: Space Mono

Visual style: Neubrutalism - hard shadows, thick borders, no rounded corners on buttons.

### Adding Content

Products are defined in `Products.astro` frontmatter as a TypeScript array.
Open source tools are defined in `OpenSource.astro` frontmatter.
Social links are in `Hero.astro` and `Footer.astro`.
