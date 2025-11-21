#!/usr/bin/env node

const { spawnSync } = require('child_process');
const { join } = require('path');
const { existsSync } = require('fs');

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

// Get platform package name
const platform = process.platform;
const arch = process.arch;

if (!platformMap[platform] || !platformMap[platform][arch]) {
  console.error(`Unsupported platform: ${platform}-${arch}`);
  console.error('Supported platforms: darwin-arm64, darwin-x64, linux-arm64, linux-x64, win32-x64');
  process.exit(1);
}

const packageName = platformMap[platform][arch];
const binaryName = platform === 'win32' ? 'peek.exe' : 'peek';
const binaryPath = join(__dirname, '..', 'node_modules', `@peek/${packageName}`, 'bin', binaryName);

// Check if binary exists
if (!existsSync(binaryPath)) {
  console.error(`peek binary not found for ${platform}-${arch}`);
  console.error(`Expected at: ${binaryPath}`);
  console.error(`\nTry reinstalling: npm install --force`);
  process.exit(1);
}

// Execute the binary
const result = spawnSync(binaryPath, process.argv.slice(2), {
  stdio: 'inherit',
  shell: false
});

if (result.error) {
  console.error(`Failed to execute peek: ${result.error.message}`);
  process.exit(1);
}

process.exit(result.status || 0);
