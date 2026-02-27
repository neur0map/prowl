import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import * as BufferModule from 'buffer';
import App from './App';
import './index.css';

/* isomorphic-git needs the Buffer global available at runtime */
const Buf = BufferModule.Buffer ?? BufferModule;
if (typeof globalThis.Buffer === 'undefined') {
  (globalThis as Record<string, unknown>).Buffer = Buf;
}

const root = document.getElementById('root');
if (root === null) throw new Error('Missing #root mount point');

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
