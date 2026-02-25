#!/usr/bin/env node

const { existsSync } = require('fs');
const { join, dirname } = require('path');

// Platform mapping
const platformMap = {
  'darwin': {
    'arm64': 'darwin-arm64',
    'x64': 'darwin-x64'
  },
  'linux': {
    'arm64': 'linux-arm64',
    'x64': 'linux-x64'
  },
  'win32': {
    'x64': 'win32-x64'
  }
};

const platform = process.platform;
const arch = process.arch;

// Check if platform is supported
if (!platformMap[platform] || !platformMap[platform][arch]) {
  console.warn(`\n peekm: Unsupported platform ${platform}-${arch}`);
  console.warn('Supported platforms: darwin-arm64, darwin-x64, linux-arm64, linux-x64, win32-x64\n');
  process.exit(0); // Don't fail installation
}

const packageName = platformMap[platform][arch];
const binaryName = platform === 'win32' ? 'peekm.exe' : 'peekm';

// Use require.resolve to find the platform package regardless of hoisting layout
let binaryPath;
try {
  const platformPkgDir = dirname(require.resolve(`@peekm/${packageName}/package.json`));
  binaryPath = join(platformPkgDir, 'bin', binaryName);
} catch (e) {
  console.error(`\npeekm: platform package @peekm/${packageName} not installed`);
  console.error(`This might happen if you used --no-optional flag`);
  console.error(`\nTry: npm install --force\n`);
  process.exit(1);
}

if (!existsSync(binaryPath)) {
  console.error(`\npeekm binary not found for ${platform}-${arch}`);
  console.error(`Expected at: ${binaryPath}`);
  console.error(`\nTry: npm install --force\n`);
  process.exit(1);
}

console.log(`peekm installed successfully for ${platform}-${arch}`);
