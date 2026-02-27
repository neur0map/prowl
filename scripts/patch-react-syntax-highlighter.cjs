#!/usr/bin/env node
const fs = require('fs');
const path = require('path');

const filesToPatch = [
  'node_modules/react-syntax-highlighter/dist/esm/default-highlight.js',
  'node_modules/react-syntax-highlighter/dist/esm/light.js',
  'node_modules/react-syntax-highlighter/dist/esm/async-syntax-highlighter.js',
  'node_modules/react-syntax-highlighter/dist/esm/async-languages/create-language-async-loader.js',
];

function patchDefaultHighlight(content) {
  return content.replace(
    "import lowlight from 'lowlight';",
    "import * as lowlightModule from 'lowlight';\nconst lowlight = lowlightModule.default || lowlightModule;"
  );
}

function patchLight(content) {
  return content.replace(
    "import lowlight from 'lowlight/lib/core';",
    "import * as lowlightModule from 'lowlight/lib/core';\nconst lowlight = lowlightModule.default || lowlightModule;"
  );
}

function patchAsyncSyntaxHighlighter(content) {
  return content.replace(
    "import _regeneratorRuntime from \"@babel/runtime/regenerator\";",
    "import * as _regeneratorRuntimeModule from \"@babel/runtime/regenerator\";\nconst _regeneratorRuntime = _regeneratorRuntimeModule.default || _regeneratorRuntimeModule;"
  );
}

function patchCreateLanguageAsyncLoader(content) {
  return content.replace(
    "import _regeneratorRuntime from \"@babel/runtime/regenerator\";",
    "import * as _regeneratorRuntimeModule from \"@babel/runtime/regenerator\";\nconst _regeneratorRuntime = _regeneratorRuntimeModule.default || _regeneratorRuntimeModule;"
  );
}

filesToPatch.forEach(file => {
  const filePath = path.resolve(__dirname, '..', file);
  if (!fs.existsSync(filePath)) {
    console.log(`File not found, skipping: ${file}`);
    return;
  }

  let content = fs.readFileSync(filePath, 'utf-8');
  const originalContent = content;

  if (file.includes('default-highlight')) {
    content = patchDefaultHighlight(content);
  } else if (file.includes('light.js') && !file.includes('light-async')) {
    content = patchLight(content);
  } else if (file.includes('async-syntax-highlighter.js')) {
    content = patchAsyncSyntaxHighlighter(content);
  } else if (file.includes('create-language-async-loader')) {
    content = patchCreateLanguageAsyncLoader(content);
  }

  if (content !== originalContent) {
    fs.writeFileSync(filePath, content, 'utf-8');
    console.log(`Patched: ${file}`);
  } else {
    console.log(`No changes needed: ${file}`);
  }
});

console.log('Patching completed');
